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
configured `HTTP_ADDR`. Readiness checks PostgreSQL and the selected storage
backend with a bounded timeout. The Worker runs Blob, UploadSession,
IdempotencyRecord, and publish recovery jobs using PostgreSQL leases.

The API process also serves the embedded administration dashboard at
`/dashboard/`; `/` redirects there. The dashboard has no separate backend or
credential store. It calls `/api/v1` on the same origin with the Bearer Token
provided by the operator, so normal scopes, repository restrictions, rate
limits, idempotency, and audit behavior remain in force. Tokens are stored in
`sessionStorage` by default and only use `localStorage` when the operator
explicitly enables persistent login.

## Required Configuration

| Variable | Sensitive | Purpose |
|---|---:|---|
| `DATABASE_URL` | yes | PostgreSQL connection URL |
| `FILESYSTEM_ROOT` | no | Absolute persistent storage path |
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

Filesystem storage never produces public URLs, so downloads remain proxied
through the authenticated API.

Generate independent application keys with a secret manager or:

```bash
openssl rand -base64 32 | tr '+/' '-_' | tr -d '='
```

Do not reuse either application key as a PostgreSQL credential.

All authenticated requests are limited after successful Token authentication.
Exceeded limits return RFC 9457 code `rate-limit-exceeded`, HTTP `429`, and an
integer `Retry-After`. The limiter is process-local, so the Helm API Deployment
is fixed at one replica and uses `Recreate`; replace the limiter with shared
state before enabling overlapping or multiple API processes.

## Docker Compose

The development stack contains PostgreSQL 17, a shared filesystem volume, a
signing-key initializer, a one-shot migration service, API, and Worker.

Override the development credentials before using a shared machine:

```bash
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

默认验收流程覆盖 filesystem 的代理下载和显式重定向拒绝。

On a fresh stack, omit `E2E_ADMIN_TOKEN` and the harness performs the one
allowed bootstrap itself. Do not omit it after any ServiceAccount exists.

`docker compose down -v` permanently deletes the local PostgreSQL,
`artifact-data`, and signing-key volumes. Use it only when intentionally
resetting development.

## Helm

The chart is under `deploy/helm/artifact-repository`. It runs migrations as a
blocking `pre-install,pre-upgrade` Hook. API is fixed at one replica for the
MVP because request limiting is process-local. Worker coordination is fenced
in PostgreSQL. In filesystem mode, API and Worker run as containers in the same
Pod and share one RWO PVC; this is intentionally a single-node storage design.

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
  --from-literal=TOKEN_PEPPER='...' \
  --from-literal=IDEMPOTENCY_RESPONSE_KEY='...'
```

Install with the pre-existing Secret and a new 100 GiB filesystem PVC:

```bash
helm upgrade --install artifact-repository deploy/helm/artifact-repository \
  --namespace artifact-repository --create-namespace \
  --set image.repository=registry.example/artifact-repository \
  --set image.tag=0.1.0 \
  --set secrets.create=false \
  --set secrets.existingSecret=artifact-repository-runtime \
  --set signing.existingSecret=artifact-repository-signing-key \
  --set storage.filesystem.persistence.size=100Gi
```

Set `storage.filesystem.persistence.existingClaim` instead when a prepared PVC
already exists. The generated PVC has `helm.sh/resource-policy: keep`; removing
the release does not authorize deleting artifact bytes. Filesystem mode rejects
`api.replicaCount` or `worker.replicaCount` values other than one.

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

### Docker Desktop

For a self-contained local cluster, install the development-only PostgreSQL
chart first, then install the application with the Docker Desktop values:

```bash
kubectl --context docker-desktop create namespace forge-artifactory

helm --kube-context docker-desktop upgrade --install forge-dependencies \
  deploy/helm/artifact-repository-local \
  --namespace forge-artifactory \
  --set-string postgresql.password='<random-password>' \
  --wait

helm --kube-context docker-desktop upgrade --install forge \
  deploy/helm/artifact-repository \
  --namespace forge-artifactory \
  -f deploy/helm/artifact-repository/values-docker-desktop.yaml \
  --set-string secrets.databaseURL='<postgresql-url>' \
  --set-string secrets.tokenPepper='<base64url-32-bytes>' \
  --set-string secrets.idempotencyResponseKey='<base64url-32-bytes>' \
  --wait
```

The local values use the Docker Desktop image cache (`pullPolicy: Never`), the
`nginx` IngressClass, and `forge.fanchao.local`. Build
`artifact-repository:local` before upgrading. The local dependency chart is not
intended for production; its single-replica PostgreSQL workload and the
application's filesystem PVC use the cluster's default StorageClass. The local
Ingress annotations raise nginx's request body limit to the repository's 10 GiB
default and extend proxy timeouts, so large CLI binaries are not rejected by the
controller before reaching the API.

A failed Migration Job blocks the install or upgrade. Inspect the retained
failed Hook Job and its logs before retrying. Do not start API or Worker with a
schema mismatch.

## Filesystem Storage

`FILESYSTEM_ROOT` must be a dedicated, persistent POSIX filesystem. The store
creates staging objects with exclusive creation, syncs completed writes, and
promotes a staging file to its content-addressed path with an atomic hard link
that never overwrites an existing Blob. Staging and final paths must therefore
remain on the same filesystem, and the volume must support hard links and
directory `fsync`.

Do not use a container writable layer, `emptyDir`, or independent per-Pod
volumes. Monitor capacity and inode usage. A local PVC does not provide node
failure tolerance: keep a tested backup on another machine or storage system.
Do not enable multiple API Pods, cross-node Workers, direct-download URLs, or
multi-site operation with this storage model.

## Metrics And Alerts

Metric labels are bounded and never contain raw paths, versions, URLs, or
tokens. The endpoint includes:

- upload count, bytes, and duration by result;
- download, Resolve, publish, and promotion results;
- Blob deduplication hit/miss and signing failure code;
- oldest pending staging age and Job backlog by fixed Job kind;
- PostgreSQL and selected-storage request count and duration.

Set `monitoring.prometheusRule.enabled=true` when the Prometheus Operator CRDs
are installed. The optional rules alert on dependency failures, staging data
older than 24 hours, and signing failures.

## Backup And Recovery

Back up PostgreSQL and the filesystem root (or configured object-storage bucket)
as one recovery set. PostgreSQL is the source of truth for visible resources,
while storage contains immutable bytes.
Also back up the signing private key and both application encryption keys in a
separate secret system.

For the simplest consistent filesystem backup, pause API and Worker, snapshot
or copy PostgreSQL and the filesystem root, then resume them. RAID and a second
directory on the same node are not backups.

After restore:

1. Restore PostgreSQL, storage bytes, and the original signing key.
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
- `public endpoint is unavailable`: request proxy downloads with `redirect=false`.
- `/readyz` returns `503`: check PostgreSQL and the configured storage root;
  `/healthz` only proves that the process is alive.
- bootstrap reports an existing administrator: issue additional accounts and
  tokens through the authenticated API; never delete the bootstrap record to
  obtain another one-time token.
