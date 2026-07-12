# Artifact Repository Operations

## Process Model

The image contains one binary with five commands:

```text
artifact-repository api
artifact-repository worker
artifact-repository migrate
artifact-repository bootstrap-admin --name <service-account-name>
artifact-repository keygen --private-key-file <path> --public-key-file <path>
```

`migrate` is the only command that changes the database schema. `api` and
`worker` perform a read-only schema-version check and refuse to start when the
database and binary do not contain exactly the same migration versions.

Both `api` and `worker` expose `/healthz`, `/readyz`, and `/metrics` on their
configured `HTTP_ADDR`. Readiness checks PostgreSQL and MinIO with a bounded
timeout. The Worker runs Blob, UploadSession, IdempotencyRecord, and publish
recovery jobs using PostgreSQL leases.

## Required Configuration

| Variable | Sensitive | Purpose |
|---|---:|---|
| `DATABASE_URL` | yes | PostgreSQL connection URL |
| `MINIO_ENDPOINT` | no | Internal MinIO origin used for object I/O |
| `MINIO_PUBLIC_ENDPOINT` | no | Optional client-reachable origin used to sign redirect URLs |
| `MINIO_ACCESS_KEY` | yes | MinIO access key |
| `MINIO_SECRET_KEY` | yes | MinIO secret key |
| `MINIO_BUCKET` | no | Existing artifact bucket |
| `TOKEN_PEPPER` | yes | Raw base64url encoding of exactly 32 random bytes |
| `IDEMPOTENCY_RESPONSE_KEY` | yes | Independent raw base64url encoding of exactly 32 random bytes |
| `SIGNING_PRIVATE_KEY_FILE` | yes | PKCS#8 Ed25519 private PEM, mode `0600` or stricter |
| `SIGNING_PUBLIC_KEY_FILE` | no | Matching PKIX Ed25519 public PEM |
| `HTTP_ADDR` | no | HTTP listen address, default `:8080` |
| `RATE_LIMIT_READ_RPS` | no | Per-Token GET/HEAD refill rate, default `50`/second |
| `RATE_LIMIT_READ_BURST` | no | Per-Token GET/HEAD burst, default `100` |
| `RATE_LIMIT_MUTATION_RPS` | no | Per-Token POST/DELETE refill rate, default `10`/second |
| `RATE_LIMIT_MUTATION_BURST` | no | Per-Token POST/DELETE burst, default `20` |
| `RATE_LIMIT_UPLOAD_RPS` | no | Per-Token Artifact PUT refill rate, default `2`/second |
| `RATE_LIMIT_UPLOAD_BURST` | no | Per-Token Artifact PUT burst, default `4` |
| `RATE_LIMIT_UPLOAD_CONCURRENCY` | no | Simultaneous uploads per Token, default `4` |
| `RATE_LIMIT_IDLE_TTL` | no | Idle bucket retention, default `15m` |

When `MINIO_PUBLIC_ENDPOINT` is empty, redirects are disabled and downloads
remain proxied through the API. Do not rewrite the host of a presigned URL;
the host is part of the SigV4 signature.

Generate independent application keys with a secret manager or:

```bash
openssl rand -base64 32 | tr '+/' '-_' | tr -d '='
```

Do not reuse either application key as a MinIO or PostgreSQL credential.

All authenticated requests are limited after successful Token authentication.
Exceeded limits return RFC 9457 code `rate-limit-exceeded`, HTTP `429`, and an
integer `Retry-After`. The limiter is process-local, so the Helm API Deployment
is fixed at one replica and uses `Recreate`; replace the limiter with shared
state before enabling overlapping or multiple API processes.

## Docker Compose

The development stack contains PostgreSQL 17, MinIO, a bucket initializer, a
signing-key initializer, a one-shot migration service, API, and Worker.

Override the development credentials before using a shared machine:

```bash
export MINIO_ACCESS_KEY='artifact-minio'
export MINIO_SECRET_KEY='replace-with-a-long-random-secret'
export TOKEN_PEPPER="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
export IDEMPOTENCY_RESPONSE_KEY="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
docker compose up -d --build
```

Use a stable project name when the stack will also be exercised by E2E tests:

```bash
export AR_PROJECT=artifact-repository
docker compose -p "$AR_PROJECT" up -d --build
```

`key-init` creates the key pair only when both files are absent. It reuses a
complete pair and fails on a partial pair; it never overwrites a key. The
`migrate` service must finish successfully before API and Worker start.

Check the stack:

```bash
docker compose -p "${AR_PROJECT:-artifact-repository}" ps
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
curl --fail http://localhost:8081/readyz
curl --fail http://localhost:8080/metrics
```

Create the first administrator once and immediately place the printed Bearer
token in a secret manager:

```bash
export ADMIN_TOKEN="$(docker compose -p "${AR_PROJECT:-artifact-repository}" exec -T api \
  /app/artifact-repository bootstrap-admin --name operations-admin)"
```

The command prints only the one-time `ar1...` Bearer token; the assignment keeps
it out of normal terminal output. A second bootstrap attempt fails after any
ServiceAccount exists.

Retain the deployment public key independently before using signed releases.
This file, not `GET /api/v1/signing-keys/{keyId}`, is the trust root:

```bash
mkdir -p .local
docker compose -p "${AR_PROJECT:-artifact-repository}" exec -T api \
  cat /app/keys/public.pem >.local/compose-public.pem
```

Re-run migrations safely:

```bash
docker compose -p "${AR_PROJECT:-artifact-repository}" run --rm migrate
```

Inspect logs without enabling shell tracing around secrets:

```bash
docker compose -p "${AR_PROJECT:-artifact-repository}" logs --tail=200 api worker migrate
```

Run the acceptance workflow against the same project. The first command proves
the default test performs no Docker or HTTP call; after manual bootstrap, pass
the retained token explicitly:

```bash
go test ./tests/e2e -count=1 -v
ARTIFACT_REPOSITORY_E2E=1 \
E2E_COMPOSE_PROJECT="${AR_PROJECT:-artifact-repository}" \
E2E_ADMIN_TOKEN="$ADMIN_TOKEN" \
E2E_PUBLIC_KEY_FILE="$PWD/.local/compose-public.pem" \
go test ./tests/e2e -count=1 -v
```

On a fresh stack, omit `E2E_ADMIN_TOKEN` and the harness performs the one
allowed bootstrap itself. Do not omit it after any ServiceAccount exists.

`docker compose down -v` permanently deletes the local PostgreSQL, MinIO, and
signing-key volumes. Use it only when intentionally resetting development.

## Helm

The chart is under `deploy/helm/artifact-repository`. It runs migrations as a
blocking `pre-install,pre-upgrade` Hook. API is fixed at one replica for the
MVP because request limiting is process-local. Worker coordination is fenced
in PostgreSQL.

Generate the signing pair outside the cluster:

```bash
mkdir -p .local/signing
go run ./cmd/artifact-repository keygen \
  --private-key-file .local/signing/private.pem \
  --public-key-file .local/signing/public.pem
kubectl create namespace artifact-repository \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n artifact-repository create secret generic artifact-repository-signing-key \
  --from-file=private.pem=.local/signing/private.pem \
  --from-file=public.pem=.local/signing/public.pem
```

The chart copies the read-only Secret through a root initContainer into an
`emptyDir`, changes ownership to numeric UID/GID `65532`, sets the private key
to `0600`, and mounts the prepared directory read-only in API and Worker.

For production, create the runtime Secret separately:

```bash
kubectl -n artifact-repository create secret generic artifact-repository-runtime \
  --from-literal=DATABASE_URL='postgresql://...' \
  --from-literal=MINIO_ACCESS_KEY='...' \
  --from-literal=MINIO_SECRET_KEY='...' \
  --from-literal=TOKEN_PEPPER='...' \
  --from-literal=IDEMPOTENCY_RESPONSE_KEY='...'
```

Install with the pre-existing Secret and deployment-specific MinIO origins:

```bash
helm upgrade --install artifact-repository deploy/helm/artifact-repository \
  --namespace artifact-repository --create-namespace \
  --set image.repository=registry.example/artifact-repository \
  --set image.tag=0.1.0 \
  --set secrets.create=false \
  --set secrets.existingSecret=artifact-repository-runtime \
  --set signing.existingSecret=artifact-repository-signing-key \
  --set config.minioEndpoint=http://minio.storage.svc:9000 \
  --set config.minioPublicEndpoint=https://artifacts.example.com
```

If `secrets.create=true`, replace every `CHANGE_ME` value in a protected values
file. The Migration Hook reads `secrets.databaseURL` directly because normal
release resources do not exist yet during `pre-install`.

Validate before rollout:

```bash
helm lint deploy/helm/artifact-repository
helm template artifact-repository deploy/helm/artifact-repository >/tmp/artifact-repository.yaml
helm get hooks artifact-repository -n artifact-repository
kubectl rollout status deployment/artifact-repository-artifact-repository-api \
  -n artifact-repository
```

A failed Migration Job blocks the install or upgrade. Inspect the retained
failed Hook Job and its logs before retrying. Do not start API or Worker with a
schema mismatch.

## Metrics And Alerts

Metric labels are bounded and never contain raw paths, versions, URLs, or
tokens. The endpoint includes:

- upload count, bytes, and duration by result;
- download, Resolve, publish, and promotion results;
- Blob deduplication hit/miss and signing failure code;
- oldest pending staging age and Job backlog by fixed Job kind;
- PostgreSQL and MinIO request count and duration.

Set `monitoring.prometheusRule.enabled=true` when the Prometheus Operator CRDs
are installed. The optional rules alert on dependency failures, staging data
older than 24 hours, and signing failures.

## Backup And Recovery

Back up PostgreSQL and the MinIO bucket as one recovery set. PostgreSQL is the
source of truth for visible resources, while MinIO contains immutable bytes.
Also back up the signing private key and both application encryption keys in a
separate secret system.

After restore:

1. Restore PostgreSQL, MinIO, and the original signing key.
2. Run `artifact-repository migrate` exactly once for the deployment.
3. Start API and Worker and require `/readyz` to return `200`.
4. Resolve and download a known release, verify its SHA-256, and verify the
   Manifest signature using the independently retained public key.

If the private signing key is lost, existing releases remain verifiable with
the public key, but new releases cannot be published. Do not generate a new key
under the old Key ID.

## Common Failures

- `database schema version mismatch`: run the matching image's `migrate`
  command; do not bypass the check.
- `private key must be ... mode 0600`: correct ownership/mode or use the chart's
  key preparation initContainer.
- `public endpoint is unavailable`: configure `MINIO_PUBLIC_ENDPOINT` or request
  proxy downloads.
- `/readyz` returns `503`: check both PostgreSQL and the configured MinIO bucket;
  `/healthz` only proves that the process is alive.
- bootstrap reports an existing administrator: issue additional accounts and
  tokens through the authenticated API; never delete the bootstrap record to
  obtain another one-time token.
