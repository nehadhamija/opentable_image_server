package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/gorilla/mux"
	"github.com/nfnt/resize"
	"github.com/nu7hatch/gouuid"
	"gopkg.in/antage/eventsource.v1"
	"gopkg.in/yaml.v2"
	"image/jpeg"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
)

type Server struct {
	Config     Config
	Thumbnails []string
	es         eventsource.EventSource
	s3Client   *s3.S3
	snsClient  *sns.SNS
}

type Config struct {
	Port          uint32
	Input_bucket  string
	Output_bucket string
	Region        string
	Access_key    string
	Secret_key    string
}

type SubscriptionConfirmation struct {
	Token    string
	TopicArn string
	Type     string
	/* 
		Message
		MessageId
		SubscribeURL
		Timestamp
	*/
}

type BroadcastNotification struct {
	Message string
	Type    string
	/*
		MessageId
		Subject (if included in the message)
		Timestamp
		TopicArn
	*/
}

type NewImageMessage struct {
	Records []NewImageRecord `json:"Records"`
}

type NewImageRecord struct {
	S3 NewImageS3 `json:"s3"`
}

type NewImageS3 struct {
	Object NewImageObject `json:"object"` 
}

type NewImageObject struct {
	Key string `json:"key"`
}

func (s *Server) Init() {
	data, err := ioutil.ReadFile("config.yml")
	if err != nil {
		log.Fatalf("Error in reading config.yml: %v", err)
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		log.Fatalf("Error in decoding config.yml: %v", err)
		panic(err)
	}

	s.Config = config
	s.Thumbnails = []string{}

	awsSession, err := session.NewSession()
	if err != nil {
		log.Fatalf("Error in creating aws session: %v", err)
	}

	s.s3Client = s3.New(awsSession, aws.NewConfig().WithRegion(config.Region).WithCredentials(credentials.NewStaticCredentials(config.Access_key, config.Secret_key, "")))
	s.snsClient = sns.New(awsSession, aws.NewConfig().WithRegion(config.Region).WithCredentials(credentials.NewStaticCredentials(config.Access_key, config.Secret_key, "")))

}

func (s *Server) UploadThumbnailVersion(message string) {
	var message_s NewImageMessage
	json.Unmarshal([]byte(message), &message_s)
	if len(message_s.Records) > 0  && len(message_s.Records[0].S3.Object.Key) > 0 {
		log.Printf("Processing new file %s", message_s.Records[0].S3.Object.Key)

		// get file from s3
		/*result, err := s.s3Client.GetObject(&s3.GetObjectInput{
			Bucket: aws.String(s.Config.Input_bucket),
			Key:    aws.String(message_s.Records[0].S3.Object.Key),
		})
		if err != nil {
			log.Printf("Failed to get object %v", err)
			return
		}*/

		response, err := http.Get(fmt.Sprintf("https://s3.%s.amazonaws.com/%s/%s", s.Config.Region, s.Config.Input_bucket, message_s.Records[0].S3.Object.Key))
		if err != nil {
			log.Printf("Error getting image from s3", err)
			return
		}
		defer response.Body.Close()

		// copy file locally
		file, err := os.Create(message_s.Records[0].S3.Object.Key)
		defer file.Close()
		if err != nil {
			log.Printf("Failed to create file %v", err)
			return
		}
		if _, err = io.Copy(file, response.Body); err != nil {
			log.Printf("Failed to copy image object to file %v", err)
			return
		}
		file.Sync()
		w := bufio.NewWriter(file)
		w.Flush()

		copied_file, err := os.Open(message_s.Records[0].S3.Object.Key)
		defer copied_file.Close()
		img, err := jpeg.Decode(copied_file)
		if err != nil {
			log.Printf("Error decoding file :", err)
			return
		}

		// resize to required size
		m := resize.Thumbnail(640, 480, img, resize.NearestNeighbor)

		out, err := os.Create(fmt.Sprintf("%s_resized.jpg", message_s.Records[0].S3.Object.Key))
		if err != nil {
			log.Printf("Failed to create file %v", err)
			return
		}
		defer out.Close()

		// write resized image to file
		jpeg.Encode(out, m, nil)
		out.Sync()
		w = bufio.NewWriter(out)
		w.Flush()

		thumbnail_file, err := os.Open(fmt.Sprintf("%s_resized.jpg", message_s.Records[0].S3.Object.Key))
		fileInfo, _ := thumbnail_file.Stat()
		var size int64 = fileInfo.Size()
		buffer := make([]byte, size)
		thumbnail_file.Read(buffer)
		outBytes := bytes.NewReader(buffer)

		// upload thumbnail to s3
		resp, err := s.s3Client.PutObject(&s3.PutObjectInput{
			Bucket:      aws.String(s.Config.Output_bucket),
			Key:         aws.String(message_s.Records[0].S3.Object.Key),
			Body:        outBytes,
			ContentType: aws.String("image/jpg"),
		})
		if err != nil {
			log.Printf("Error putting thumbnail to output bucket %v", err)
			return
		}

		log.Printf("Response from aws %s", resp.GoString())

		// delete the files
		os.Remove(message_s.Records[0].S3.Object.Key)
		os.Remove(fmt.Sprintf("%s_resized.jpg", message_s.Records[0].S3.Object.Key))

		s.es.SendEventMessage(fmt.Sprintf("https://s3.%s.amazonaws.com/%s/%s", s.Config.Region, s.Config.Output_bucket, message_s.Records[0].S3.Object.Key), "", "")
	}	
}

func (s *Server) handleNewUpload(w http.ResponseWriter, r *http.Request) {
	MessageType := r.Header.Get("x-amz-sns-message-type")

	requestBody, _ := ioutil.ReadAll(r.Body)
	
	if MessageType == "SubscriptionConfirmation" {
		go func() {
			var data SubscriptionConfirmation
			json.Unmarshal(requestBody, &data)
			log.Printf("Subscription Confirmation data %v", data);
			resp, err := s.snsClient.ConfirmSubscription(&sns.ConfirmSubscriptionInput{
				Token:                     aws.String(data.Token),
				TopicArn:                  aws.String(data.TopicArn),
				AuthenticateOnUnsubscribe: aws.String("true"),
			})
			if err != nil {
				log.Printf("Error subscribing %v", err);
			} else {
				log.Printf("Subscription confirmation response %s", resp.GoString());
			}
		}()
		fmt.Fprint(w, "OK")
		return
	}
	
	if MessageType == "Notification" {
		go func() {
			var data BroadcastNotification
			json.Unmarshal(requestBody, &data)
			s.UploadThumbnailVersion(data.Message)
		}()
		fmt.Fprint(w, "OK")
	}

}

func (s *Server) handleUploadUrl(w http.ResponseWriter, r *http.Request) {
	uuid, _ := uuid.NewV4()
	fmt.Fprint(w, fmt.Sprintf("http://s3.%s.amazonaws.com/%s/%s.jpg", s.Config.Region, s.Config.Input_bucket, uuid.String()))
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleImages...")
	op, err := s.s3Client.ListObjects(&s3.ListObjectsInput{Bucket: aws.String(s.Config.Output_bucket)})
	if err != nil {
		log.Printf("Error listing Thumbnails: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Response from aws: %s", op.GoString())
	thumbnails := []string{}
	for _,object := range op.Contents {
		if object.Key != nil {
			thumbnails = append(thumbnails,fmt.Sprintf("https://s3.%s.amazonaws.com/%s/%s", s.Config.Region, s.Config.Output_bucket, *object.Key))
		}
	}
	resp_bytes, err := json.Marshal(thumbnails)
	if err != nil {
		log.Printf("Error marshalling: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, string(resp_bytes))
}

func (s *Server) handleCORS(handlerFunc func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		//w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST")
		handlerFunc(w,r)
	}
}

func main() {
	port := os.Getenv("PORT")

	if port == "" {
		log.Fatal("$PORT must be set")
	}

	s := Server{}
	s.Init()

	s.es = eventsource.New(
		eventsource.DefaultSettings(),
		func(req *http.Request) [][]byte {
			return [][]byte{
				[]byte("Content-Type: text/event-stream"),
				[]byte("Access-Control-Allow-Origin: *"),
			}
		},
	)

	router := mux.NewRouter()
	router.HandleFunc("/new_image_notify", s.handleCORS(s.handleNewUpload)).Methods("POST")
	router.HandleFunc("/upload_url", s.handleCORS(s.handleUploadUrl)).Methods("GET")
	router.HandleFunc("/images", s.handleCORS(s.handleImages)).Methods("GET")
	router.Handle("/image_uploaded", s.es)

	log.Println("Running on port ", port)
	err := http.ListenAndServe(fmt.Sprintf(":%s",port), router)
	if err != nil {
		log.Println(err)
	}
}
