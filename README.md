# cf-local-service-broker

Lightweight Open Service Broker API (OSBAPI) v2 brokers for provisioning PostgreSQL databases and MinIO buckets in a CF-on-kind environment.

## What This Is

When running Cloud Foundry on kind (CF-on-kind), you often have infrastructure services like PostgreSQL and MinIO already deployed in the cluster. This project provides OSBAPI-compliant service brokers that let CF applications bind to those existing services using the standard `cf create-service` / `cf bind-service` workflow.

Two brokers are included:

- **postgresql-local** — Creates databases and roles on a shared PostgreSQL instance
- **minio-local** — Creates buckets and access keys on a shared MinIO instance

## Prerequisites

- A CF-on-kind deployment (or any Cloud Foundry with access to the backing services)
- PostgreSQL instance accessible from the cluster
- MinIO instance accessible from the cluster
- `kubectl` and `cf` CLI tools
- Go 1.23+ (for building from source)

## Quick Start

### Build

```bash
make build
```

### Deploy to Kubernetes

```bash
# Create secrets for broker credentials
kubectl create secret generic postgres-broker-creds \
  --from-literal=BROKER_USERNAME=admin \
  --from-literal=BROKER_PASSWORD=$(openssl rand -hex 16) \
  --from-literal=PG_ADMIN_PASSWORD=your-pg-password

kubectl create secret generic minio-broker-creds \
  --from-literal=BROKER_USERNAME=admin \
  --from-literal=BROKER_PASSWORD=$(openssl rand -hex 16) \
  --from-literal=MINIO_ACCESS_KEY=your-minio-access-key \
  --from-literal=MINIO_SECRET_KEY=your-minio-secret-key

# Deploy
make deploy-postgres
make deploy-minio
```

### Register with Cloud Foundry

```bash
make register-postgres BROKER_USERNAME=admin BROKER_PASSWORD=<password>
make register-minio BROKER_USERNAME=admin BROKER_PASSWORD=<password>
```

### Use from a CF App

```bash
# Create service instances
cf create-service postgresql-local shared my-postgres
cf create-service minio-local shared my-minio

# Bind to your app
cf bind-service my-app my-postgres
cf bind-service my-app my-minio

# Restage to pick up credentials
cf restage my-app
```

## Available Services

### postgresql-local

| Plan   | Description                                      |
|--------|--------------------------------------------------|
| shared | Creates a database and role on the shared instance |

Binding credentials:
```json
{
  "host": "postgresql.default.svc.cluster.local",
  "port": "5432",
  "database": "cf_<instance_id>",
  "username": "cf_<binding_id>",
  "password": "<generated>",
  "uri": "postgres://cf_<binding_id>:<password>@host:5432/cf_<instance_id>"
}
```

### minio-local

| Plan   | Description                                   |
|--------|-----------------------------------------------|
| shared | Creates a bucket on the shared MinIO instance |

Binding credentials:
```json
{
  "endpoint": "minio.default.svc.cluster.local:9000",
  "access_key": "<generated>",
  "secret_key": "<generated>",
  "bucket": "cf-<instance_id>",
  "use_ssl": false
}
```

## Architecture

```
                    CF Cloud Controller
                           |
                    OSBAPI v2 API
                    /              \
     +-----------------+    +-----------------+
     | postgres-broker |    |  minio-broker   |
     |   (port 8080)   |    |   (port 8080)   |
     +-----------------+    +-----------------+
            |                        |
     +-------------+         +-------------+
     |  PostgreSQL  |         |    MinIO    |
     |   Instance   |         |   Instance  |
     +-------------+         +-------------+
```

Both brokers:
- Accept HTTP basic auth for broker API authentication
- Implement the full OSBAPI v2 lifecycle (catalog, provision, bind, unbind, deprovision)
- Generate cryptographically random credentials for each binding
- Run as single-binary deployments

## Development

```bash
# Run tests
make test

# Build binaries
make build

# Binaries are placed in bin/
ls bin/
```

## License

MIT
