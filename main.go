package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var (
	// Tag of current version
	Tag = "Unknown"
	// Date of current version
	Date = "Unknown"
	// GracefullTimeout set timeout before exit from server
	GracefullTimeout int = 1
	// Configuration holder
	configHolder *confHolder
	// S3 Session
	s3Session *s3.S3
)

// Application config type
type webConfig struct {
	Port      string `json:"port" yaml:"port" toml:"port"`
	S3bucket  string `json:"s3bucket" yaml:"s3bucket" toml:"s3bucket"`
	AwsRegion string `json:"awsRegion" yaml:"awsRegion" toml:"awsRegion"`
	Homepage  string `json:"homepage" yaml:"homepage" toml:"homepage"`
}

// Configuration holder type
type confHolder struct {
	Config *webConfig
}

// Get an environment variable or use a default value if not set
func getEnvOrDefault(envName, defaultVal string, fatal bool) (envVal string) {
	envVal = os.Getenv(envName)
	if len(envVal) == 0 {
		if fatal {
			log.Errorf("Unable to start as env %s is not defined", envName)
			os.Exit(1)
		}
		envVal = defaultVal
		log.Debugf("Using default %s : %s ", envName, envVal)
	} else {
		log.Debugf("%s : %s", envName, envVal)
	}
	return
}

// Read configuration file. Default is CONFIG_FILE environment variable value if it is set or file "config.yaml".
// If configuration file do not exists, then
func readConfig(configPath string) (*webConfig, error) {
	if configPath == "" {
		configPath = "config.yaml"
	}
	// Read file content
	bs, err := ioutil.ReadFile(configPath)
	if err != nil {
		return &webConfig{}, errors.Wrap(err, "failed to read configuration file")
	}
	// Check file extension
	extension := filepath.Ext(configPath)
	var cfg *webConfig = &webConfig{}
	switch extension {
	case ".yaml", ".yml":
		// Unmarshal Yaml file to config struct
		err = yaml.Unmarshal(bs, &cfg)
	case ".json":
		// Unmarshal Json file to config struct
		err = json.Unmarshal(bs, &cfg)
	case ".toml":
		// Unmarshal Toml file to config struct
		_, err = toml.DecodeFile(configPath, &cfg)
	default:
		err = fmt.Errorf("Unknown configuration file format %s (support only yaml or json)", extension)
	}
	if err != nil {
		return &webConfig{}, errors.Wrap(err, "failed to parse configuration file")
	}
	log.Debugf("config = %v", cfg)
	if cfg.Port == "" {
		cfg.Port = "8000"
	}
	if cfg.AwsRegion == "" {
		cfg.AwsRegion = getEnvOrDefault("AWS_REGION", "eu-west-1", false)
	}
	return cfg, nil
}

// showVersion get version with full runtime description
func showVersion() string {
	return fmt.Sprintf(`%s (%s on %s/%s; %s)`, Tag, runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.Compiler)
}

// Serve a HEAD request for a S3 file
func serveHeadS3File(c *gin.Context) {
	r := c.Request
	w := c.Writer
	filePath := r.URL.Path[1:]
	input := &s3.HeadObjectInput{Bucket: aws.String(configHolder.Config.S3bucket), Key: aws.String(filePath)}
	etag := r.Header.Get("ETag")
	if etag != "" {
		input.IfNoneMatch = &etag
	}
	resp, err := s3Session.HeadObject(input)
	if handleHTTPException(filePath, w, err) != nil {
		return
	}
	w.Header().Set("Content-Type", *resp.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", *resp.ContentLength))
	w.Header().Set("Last-Modified", resp.LastModified.String())
	w.Header().Set("Etag", *resp.ETag)
}

// Serve a GET request for a S3 file
func serveGetS3File(c *gin.Context) {
	w := c.Writer
	filePath := c.Request.URL.Path[1:]

	params := &s3.GetObjectInput{Bucket: aws.String(configHolder.Config.S3bucket), Key: aws.String(filePath)}
	resp, err := s3Session.GetObject(params)
	if handleHTTPException(filePath, w, err) != nil {
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", *resp.ContentType)
	w.Header().Set("Last-Modified", resp.LastModified.String())
	w.Header().Set("Etag", *resp.ETag)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", *resp.ContentLength))

	// File is ready to download
	io.Copy(w, resp.Body)
}

// Serve a PUT request for a S3 file
func servePutS3File(c *gin.Context) {
	// Convert the uploaded body to a byte array TODO fix this for large sizes
	r := c.Request
	w := c.Writer
	filePath := r.URL.Path[1:]
	b, err := ioutil.ReadAll(r.Body)

	if handleHTTPException(filePath, w, err) != nil {
		return
	}

	params := &s3.PutObjectInput{Bucket: aws.String(configHolder.Config.S3bucket), Key: aws.String(filePath), Body: bytes.NewReader(b)}

	resp, err := s3Session.PutObject(params)

	if handleHTTPException(filePath, w, err) != nil {
		return
	}
	w.Header().Set("ETag", *resp.ETag)

	// File has been created TODO do not return a http.StatusCreated if the file was updated
	http.Redirect(w, r, "/"+filePath, http.StatusCreated)
}

// Serve a DELETE request for a S3 file
func serveDeleteS3File(c *gin.Context) {
	w := c.Writer
	filePath := c.Request.URL.Path[1:]
	params := &s3.DeleteObjectInput{Bucket: aws.String(configHolder.Config.S3bucket), Key: aws.String(filePath)}
	_, err := s3Session.DeleteObject(params)

	if handleHTTPException(filePath, w, err) != nil {
		return
	}

	// File has been deleted
	w.WriteHeader(http.StatusNoContent)
}

// Handle http method to provide the good S3 function
func methodHandler(c *gin.Context) {
	r := c.Request
	w := c.Writer
	var method = r.Method
	var path = r.URL.Path[1:] // Remove the / from the start of the URL

	// A file with no path cannot be served
	if path == "" || path[len(path)-1:] == "/" {
		if configHolder.Config.Homepage == "" {
			log.Debugln("GET : filepath is empty")
			http.Error(w, "Path must be provided", http.StatusBadRequest)
			return
		}
		r.URL.Path = r.URL.Path + configHolder.Config.Homepage
	}

	switch method {
	case "GET":
		serveGetS3File(c)
	case "PUT":
		servePutS3File(c)
	case "DELETE":
		serveDeleteS3File(c)
	case "HEAD":
		serveHeadS3File(c)
	default:
		http.Error(w, "Method "+method+" not supported", http.StatusMethodNotAllowed)
	}
}

// Handle an exception and write to response
func handleHTTPException(path string, w http.ResponseWriter, err error) (e error) {
	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			log.Debugf("Failed : %v", awsError)
			// aws error
			switch awsError.Code() {
			case "MissingContentLength":
				http.Error(w, "Bad Request", http.StatusBadRequest)
			case "NotModified":
				http.Error(w, "Object not modified", http.StatusNotModified)
			case "NoSuchKey", "NotFound":
				http.Error(w, "Path '"+path+"' not found: "+awsError.Message(), http.StatusNotFound)
			default:
				origErr := awsError.OrigErr()
				cause := ""
				if origErr != nil {
					cause = " (Cause: " + origErr.Error() + ")"
				}
				http.Error(w, "An internal error occurred: "+awsError.Code()+" = "+awsError.Message()+cause, http.StatusInternalServerError)
			}
		} else {
			log.Debugf("Failed : %v", err)
			// golang error
			http.Error(w, "An internal error occurred: "+err.Error(), http.StatusInternalServerError)
		}
	}
	return err
}

// main
func main() {
	log.SetLevel(log.InfoLevel)
	log.Printf("S3WebServer By B.LEBOEUF %s", showVersion())
	configFile := flag.String("config", "config.toml", "`config file`")
	debug := flag.Bool("debug", false, "`Mode debug`")

	flag.Parse()

	if *debug {
		log.SetLevel(log.DebugLevel)
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}
	// Read configuration
	config, err := readConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	configHolder = &confHolder{config}

	// Set up the S3 connection
	s3Session = s3.New(session.New(), &aws.Config{Region: aws.String(config.AwsRegion)})

	// Instanciate router
	router := gin.Default()

	// Add middleware
	router.Use(gzip.Gzip(gzip.DefaultCompression))

	// Init http route
	router.NoRoute(methodHandler)

	// Start HTTP Server
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", config.Port),
		Handler: router,
	}

	go func() {
		// service connections
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server with
	// a timeout of 5 seconds.
	quit := make(chan os.Signal)
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall.SIGKILL but can't be catch, so don't need add it
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Infoln("Shutdown Server ...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(GracefullTimeout)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown:", err)
	}
	// catching ctx.Done(). timeout of 5 seconds.
	select {
	case <-ctx.Done():
		log.Errorln("timeout of 5 seconds.")
	}
	log.Infoln("Server exiting")
}
