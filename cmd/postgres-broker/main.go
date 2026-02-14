package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/pivotal-cf/brokerapi/v11"
	"github.com/williamzujkowski/cf-local-service-broker/internal/broker/postgres"
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

	pgHost := os.Getenv("PG_HOST")
	if pgHost == "" {
		pgHost = "postgresql.default.svc.cluster.local"
	}
	pgPort := os.Getenv("PG_PORT")
	if pgPort == "" {
		pgPort = "5432"
	}
	pgUser := os.Getenv("PG_ADMIN_USER")
	if pgUser == "" {
		pgUser = "postgres"
	}
	pgPass := os.Getenv("PG_ADMIN_PASSWORD")
	if pgPass == "" {
		log.Fatal("PG_ADMIN_PASSWORD must be set")
	}

	broker := postgres.New(pgHost, pgPort, pgUser, pgPass)

	credentials := brokerapi.BrokerCredentials{
		Username: username,
		Password: password,
	}

	logger := slog.Default()
	handler := brokerapi.New(broker, logger, credentials)

	log.Printf("PostgreSQL broker starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
