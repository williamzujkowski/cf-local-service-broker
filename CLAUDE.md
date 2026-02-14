# cf-local-service-broker - Claude Code Instructions

**Project:** Lightweight OSBAPI v2 service brokers for CF-on-kind
**Language:** Go 1.23
**Repository:** github.com/williamzujkowski/cf-local-service-broker

---

## Overview

This project provides two Open Service Broker API (OSBAPI) v2 compatible service brokers
built using `github.com/pivotal-cf/brokerapi/v11`:

1. **PostgreSQL Broker** — Provisions databases and roles on a shared PostgreSQL instance
2. **MinIO Broker** — Provisions buckets and access keys on a shared MinIO instance

Each broker implements the `domain.ServiceBroker` interface from brokerapi.

## Quick Reference

```bash
# Build
go build ./cmd/...

# Test
go test ./...

# Build specific broker
go build -o bin/postgres-broker ./cmd/postgres-broker
go build -o bin/minio-broker ./cmd/minio-broker

# Run locally
BROKER_USERNAME=admin BROKER_PASSWORD=secret PG_ADMIN_PASSWORD=pgpass ./bin/postgres-broker
BROKER_USERNAME=admin BROKER_PASSWORD=secret MINIO_ACCESS_KEY=minioadmin MINIO_SECRET_KEY=minioadmin ./bin/minio-broker
```

## Deployment

These brokers are designed to run as:
- **CF apps** pushed via `cf push`
- **Kubernetes deployments** via manifests in `deploy/k8s/`

Register with Cloud Foundry:
```bash
cf create-service-broker postgres-local admin secret http://postgres-broker.default.svc.cluster.local:8080
cf enable-service-access postgresql-local
```

## Architecture

- `cmd/postgres-broker/` — Entry point for PostgreSQL broker
- `cmd/minio-broker/` — Entry point for MinIO broker
- `internal/broker/postgres/` — PostgreSQL broker implementation (`domain.ServiceBroker`)
- `internal/broker/minio/` — MinIO broker implementation (`domain.ServiceBroker`)
- `deploy/k8s/` — Kubernetes deployment manifests

## Key Dependencies

- `github.com/pivotal-cf/brokerapi/v11` — OSBAPI v2 broker framework
- `github.com/lib/pq` — PostgreSQL driver
- `github.com/minio/minio-go/v7` — MinIO client

## Environment Variables

### PostgreSQL Broker
| Variable           | Required | Default                                  |
|--------------------|----------|------------------------------------------|
| BROKER_USERNAME    | Yes      | —                                        |
| BROKER_PASSWORD    | Yes      | —                                        |
| PG_HOST            | No       | postgresql.default.svc.cluster.local     |
| PG_PORT            | No       | 5432                                     |
| PG_ADMIN_USER      | No       | postgres                                 |
| PG_ADMIN_PASSWORD  | Yes      | —                                        |
| PORT               | No       | 8080                                     |

### MinIO Broker
| Variable           | Required | Default                                  |
|--------------------|----------|------------------------------------------|
| BROKER_USERNAME    | Yes      | —                                        |
| BROKER_PASSWORD    | Yes      | —                                        |
| MINIO_ENDPOINT     | No       | minio.default.svc.cluster.local:9000     |
| MINIO_ACCESS_KEY   | Yes      | —                                        |
| MINIO_SECRET_KEY   | Yes      | —                                        |
| MINIO_USE_SSL      | No       | false                                    |
| PORT               | No       | 8080                                     |
