# cf-local-service-broker

Lightweight Open Service Broker API (OSBAPI) v2 brokers for provisioning PostgreSQL databases (with optional pgvector) and MinIO buckets, backed by services you already operate.

## What This Is

When you have PostgreSQL or MinIO running somewhere reachable from your Cloud Foundry deployment — on a BOSH VM, in a Kubernetes cluster, in a Podman container, on bare metal, in a managed cloud — but no service broker to expose them to CF apps, this project gives you that broker layer.

The brokers themselves are small Go binaries that speak OSBAPI v2. Any Cloud Foundry foundation can register them and offer the services to apps via the standard `cf create-service` / `cf bind-service` workflow. They were originally built for a CF-on-kind development environment, but nothing about them is kind-specific — they work equally well against:

- **BOSH-deployed Cloud Foundry** (CF-on-BOSH-on-Incus, CF-on-vSphere, CF-on-AWS, etc.)
- **CF-on-kind** for local development
- Any other OSBAPI v2 consumer (CF, CF-for-VMs, OpenShift Service Catalog, KSC)

The brokers themselves can be deployed as CF apps (`cf push`), as Kubernetes Deployments (manifests in `deploy/k8s/`), as systemd units, or as Podman quadlets. Pick whichever matches your operational pattern.

Two brokers are included:

- **postgresql-local** — Creates databases and roles on a shared PostgreSQL instance. Two plans:
  - `shared` — plain database + role with privileges on it.
  - `pgvector` — same, plus `CREATE EXTENSION vector` on the new database. Requires the backing PostgreSQL to ship the pgvector extension; see [`bosh-pgvector-release`](https://github.com/williamzujkowski/bosh-pgvector-release) if you need a BOSH-deployable PostgreSQL with pgvector baked in.
- **minio-local** — Creates buckets and access keys on a shared MinIO instance.

## Prerequisites

- A Cloud Foundry foundation that can register an OSBAPI broker (any flavor: CF-on-BOSH, CF-on-kind, others).
- A PostgreSQL instance reachable from where the broker runs. For the `pgvector` plan, the instance must ship the pgvector extension (e.g., `pgvector/pgvector:pg16` image, or a PostgreSQL deployment that has `pgvector` installed at the OS level).
- A MinIO instance reachable from where the broker runs.
- `cf` CLI to register the brokers and use the services.
- `kubectl` only if deploying the brokers as Kubernetes workloads (one of several options).
- Go 1.23+ to build from source.

## Quick Start

### Build

```bash
make build
```

This produces `bin/postgres-broker` and `bin/minio-broker`. They're single-binary HTTP servers; you can run them however you deploy other Go binaries.

### Configure credentials

Each broker needs:

- HTTP basic-auth credentials that CF will use to talk to the broker.
- Admin credentials for the backing service (Postgres admin, MinIO root keys) so the broker can create databases/buckets on demand.

Pass them as environment variables (full reference in [CLAUDE.md](./CLAUDE.md)).

### Deploy the brokers

Pick one:

**(a) As a CF app:**

```bash
cd <your-deploy-tree>
cf push postgres-broker -b binary_buildpack -c './postgres-broker' -m 64M
cf push minio-broker -b binary_buildpack -c './minio-broker' -m 64M
cf set-env postgres-broker BROKER_USERNAME admin
# ... etc.
cf restage postgres-broker
```

**(b) On Kubernetes** (manifests in `deploy/k8s/`):

```bash
kubectl create secret generic postgres-broker-creds \
  --from-literal=BROKER_USERNAME=admin \
  --from-literal=BROKER_PASSWORD=$(openssl rand -hex 16) \
  --from-literal=PG_ADMIN_PASSWORD=your-pg-password

kubectl create secret generic minio-broker-creds \
  --from-literal=BROKER_USERNAME=admin \
  --from-literal=BROKER_PASSWORD=$(openssl rand -hex 16) \
  --from-literal=MINIO_ACCESS_KEY=your-minio-access-key \
  --from-literal=MINIO_SECRET_KEY=your-minio-secret-key

make deploy-postgres
make deploy-minio
```

**(c) As a systemd unit or Podman quadlet** — write your own unit pointing at the binary; the broker only needs the env vars listed in [CLAUDE.md](./CLAUDE.md).

### Register with Cloud Foundry

```bash
cf create-service-broker postgres-local admin <password> https://postgres-broker.<your-route>
cf create-service-broker minio-local   admin <password> https://minio-broker.<your-route>
cf enable-service-access postgresql-local
cf enable-service-access minio-local
```

(Adjust URLs/hostnames for your deployment shape.)

### Use from a CF App

```bash
# Create service instances
cf create-service postgresql-local shared    my-postgres
cf create-service postgresql-local pgvector  my-vector-db   # pgvector-enabled
cf create-service minio-local      shared    my-minio

# Bind to your app
cf bind-service my-app my-postgres
cf bind-service my-app my-minio

# Restage to pick up credentials
cf restage my-app
```

## Available Services

### postgresql-local

| Plan      | Description                                                                |
|-----------|----------------------------------------------------------------------------|
| shared    | Creates a database and role on the shared instance.                        |
| pgvector  | As shared, plus `CREATE EXTENSION vector` on the new database. Requires a pgvector-capable backing PostgreSQL (see [pgvector backing PostgreSQL](#pgvector-backing-postgresql)). |

Binding credentials (host/port depend on where your backing PostgreSQL lives):

```json
{
  "host": "postgresql.example.invalid",
  "port": "5432",
  "database": "cf_<instance_id>",
  "username": "cf_<binding_id>",
  "password": "<generated>",
  "uri": "postgres://cf_<binding_id>:<password>@host:5432/cf_<instance_id>"
}
```

#### Shared owner role (multi-binding apps)

By default, every service instance also gets a per-instance non-LOGIN
group role named `cf_<instance_id>_owner`. The DB is owned by that role,
and every Bind grants the binding role membership in it plus a
`SET ROLE = <owner_role>` session default. Result: tables/sequences
created from one binding (e.g. an API doing alembic migrations) are
automatically owned by the shared role and are read/writable from any
other binding against the same instance (e.g. a worker). No admin-level
reconcile required.

The `username` returned in binding credentials is still the per-binding
login role — apps connect with their own creds; the shared-role
inheritance happens server-side.

To opt out (e.g. for in-place upgrades where you don't want to rebind
existing instances), set `POSTGRES_BROKER_SHARED_OWNER_ROLE=false`. In
that mode the broker behaves exactly as it did before this feature: per
binding role only, DB owner stays as the admin role. Existing instances
provisioned in opt-out mode keep working; rebinding them later under
opt-in mode is not automatic — you'd need to deprovision/reprovision or
do a one-time admin reconcile.

### minio-local

| Plan   | Description                                   |
|--------|-----------------------------------------------|
| shared | Creates a bucket on the shared MinIO instance |

Binding credentials:

```json
{
  "endpoint": "minio.example.invalid:9000",
  "access_key": "<generated>",
  "secret_key": "<generated>",
  "bucket": "cf-<instance_id>",
  "use_ssl": false
}
```

## pgvector backing PostgreSQL

The `pgvector` plan runs `CREATE EXTENSION IF NOT EXISTS vector` against each new database. That extension must already be available in the backing PostgreSQL — stock `postgres:N` images and most distro packages do **not** ship it.

Several options:

- **Containerized** — use the official `pgvector/pgvector:pg16` image (or `pg15`, `pg17`) as your backing PostgreSQL.
- **BOSH-deployed** — use [`bosh-pgvector-release`](https://github.com/williamzujkowski/bosh-pgvector-release), a thin fork of `cloudfoundry/postgres-release` that adds pgvector as a co-installed package for PostgreSQL 15, 16, and 17.
- **Bare-metal / VM** — install the `postgresql-N-pgvector` apt/dnf package alongside the PostgreSQL server (Debian/Ubuntu: `apt install postgresql-16-pgvector`).
- **Managed cloud** — AWS RDS, Azure Database for PostgreSQL, Google Cloud SQL, and Crunchy Bridge all support pgvector as a managed extension; enable it per their docs.

If you point the `pgvector` plan at a stock PostgreSQL image, `Provision` returns a clear error (`extension "vector" is not available`) and the empty database is rolled back so a retry succeeds once the backing PostgreSQL is fixed.

## Architecture

```text
                    CF Cloud Controller
                           |
                    OSBAPI v2 API (HTTP basic auth)
                    /              \
     +-----------------+    +-----------------+
     | postgres-broker |    |  minio-broker   |
     |   (port 8080)   |    |   (port 8080)   |
     +-----------------+    +-----------------+
            |                        |
     +-------------+         +-------------+
     |  PostgreSQL |         |    MinIO    |
     |   anywhere  |         |   anywhere  |
     +-------------+         +-------------+
```

Both brokers:

- Speak OSBAPI v2 over HTTP with basic auth.
- Implement the full lifecycle (catalog, provision, bind, unbind, deprovision).
- Generate cryptographically random credentials for each binding.
- Run as single-binary deployments — no JVM, no runtime, no sidecars.

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
