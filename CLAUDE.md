# cf-local-service-broker - Claude Code Instructions

**Project:** Lightweight OSBAPI v2 service brokers for PostgreSQL and MinIO. Deployable as CF apps, on Kubernetes, or any other shape that runs a single Go binary. Originally written for CF-on-kind, but nothing in the broker code is kind-specific — works equally against BOSH-deployed CF, CF-for-VMs, and other OSBAPI v2 consumers.
**Language:** Go 1.23
**Repository:** github.com/williamzujkowski/cf-local-service-broker

---

## Overview

This project provides two Open Service Broker API (OSBAPI) v2 compatible service brokers
built using `github.com/pivotal-cf/brokerapi/v11`:

1. **PostgreSQL Broker** — Provisions databases and roles on a shared PostgreSQL instance.
   Two plans: `shared` (database + role) and `pgvector` (also runs `CREATE EXTENSION vector`,
   requires a pgvector-capable backing PostgreSQL).
2. **MinIO Broker** — Provisions buckets and access keys on a shared MinIO instance.

Each broker implements the `domain.ServiceBroker` interface from brokerapi.

Deployment is decoupled from the broker code. The broker is a single Go binary; deploy it
however your foundation deploys other binaries (`cf push`, k8s Deployment, systemd, Podman
quadlet). The `deploy/k8s/` manifests are one option, not the only one.

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

These brokers run as single Go binaries. Common deployment shapes:

- **CF apps** pushed via `cf push -b binary_buildpack`
- **Kubernetes deployments** via the manifests in `deploy/k8s/`
- **Systemd units** or **Podman quadlets** on a regular VM/host
- Anything else that can run a HTTP-serving Go binary with env vars

Register with Cloud Foundry (URL depends on where you deployed the broker —
substitute your real route/hostname):

```bash
cf create-service-broker postgres-local admin <password> https://postgres-broker.<your-route>
cf enable-service-access postgresql-local
```

## Architecture

- `cmd/postgres-broker/` — Entry point for PostgreSQL broker
- `cmd/minio-broker/` — Entry point for MinIO broker
- `internal/broker/postgres/` — PostgreSQL broker implementation (`domain.ServiceBroker`).
  Two plans: `shared` (database + role) and `pgvector` (also runs `CREATE EXTENSION vector`).
  Pgvector plan requires a pgvector-capable backing image (e.g. `pgvector/pgvector:pg16`).
- `internal/broker/minio/` — MinIO broker implementation (`domain.ServiceBroker`)
- `deploy/k8s/` — Kubernetes deployment manifests

## Key Dependencies

- `github.com/pivotal-cf/brokerapi/v11` — OSBAPI v2 broker framework
- `github.com/lib/pq` — PostgreSQL driver
- `github.com/minio/minio-go/v7` — MinIO client

## Environment Variables

### PostgreSQL Broker
| Variable           | Required | Default                                  | Notes |
|--------------------|----------|------------------------------------------|-------|
| BROKER_USERNAME    | Yes      | —                                        | HTTP basic-auth username CF uses to talk to the broker |
| BROKER_PASSWORD    | Yes      | —                                        | HTTP basic-auth password |
| PG_HOST            | No       | postgresql.default.svc.cluster.local     | Override to point at non-k8s PostgreSQL (BOSH VM, managed cloud, etc.) |
| PG_PORT            | No       | 5432                                     |       |
| PG_ADMIN_USER      | No       | postgres                                 |       |
| PG_ADMIN_PASSWORD  | Yes      | —                                        | Admin creds for the backing PostgreSQL |
| PORT               | No       | 8080                                     | Broker's HTTP listen port |

For the `pgvector` plan to work, the backing PostgreSQL must ship the pgvector
extension. See README's [pgvector backing PostgreSQL](./README.md#pgvector-backing-postgresql)
section. For BOSH-deployed PostgreSQL, use [`bosh-pgvector-release`](https://github.com/williamzujkowski/bosh-pgvector-release).

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
