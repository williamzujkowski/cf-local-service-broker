.PHONY: build test clean deploy-postgres deploy-minio register-postgres register-minio

build:
	go build -o bin/postgres-broker ./cmd/postgres-broker
	go build -o bin/minio-broker ./cmd/minio-broker

test:
	go test ./...

clean:
	rm -rf bin/

deploy-postgres:
	kubectl apply -f deploy/k8s/postgres-broker.yaml

deploy-minio:
	kubectl apply -f deploy/k8s/minio-broker.yaml

register-postgres:
	cf create-service-broker postgres-local $(BROKER_USERNAME) $(BROKER_PASSWORD) http://postgres-broker.default.svc.cluster.local:8080
	cf enable-service-access postgresql-local

register-minio:
	cf create-service-broker minio-local $(BROKER_USERNAME) $(BROKER_PASSWORD) http://minio-broker.default.svc.cluster.local:8080
	cf enable-service-access minio-local
