package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/pivotal-cf/brokerapi/v11"
	minioBroker "github.com/williamzujkowski/cf-local-service-broker/internal/broker/minio"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	username := os.Getenv("BROKER_USERNAME")
	password := os.Getenv("BROKER_PASSWORD")
	if username == "" || password == "" {
		log.Fatal("BROKER_USERNAME and BROKER_PASSWORD must be set")
	}

	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "minio.default.svc.cluster.local:9000"
	}
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		log.Fatal("MINIO_ACCESS_KEY and MINIO_SECRET_KEY must be set")
	}

	useSSL := strings.EqualFold(os.Getenv("MINIO_USE_SSL"), "true")

	broker := minioBroker.New(endpoint, accessKey, secretKey, useSSL)

	credentials := brokerapi.BrokerCredentials{
		Username: username,
		Password: password,
	}

	logger := slog.Default()
	handler := brokerapi.New(broker, logger, credentials)

	log.Printf("MinIO broker starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
