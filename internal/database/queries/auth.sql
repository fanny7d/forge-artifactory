-- name: CreateServiceAccount :one
INSERT INTO service_accounts (name)
VALUES ($1)
RETURNING *;

-- name: CountServiceAccounts :one
SELECT count(*) FROM service_accounts;

-- name: GetServiceAccount :one
SELECT * FROM service_accounts WHERE id = $1;

-- name: ListServiceAccounts :many
SELECT *
FROM service_accounts
WHERE (
    sqlc.narg('after_created_at')::timestamptz IS NULL
    OR (created_at, id) < (
        sqlc.narg('after_created_at')::timestamptz,
        sqlc.arg('after_id')::uuid
    )
)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: CreateAPIToken :one
INSERT INTO api_tokens (
	ID,
    service_account_id,
    secret_hmac,
    scopes,
    repository_ids,
    expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAPITokenForAuthentication :one
SELECT t.*, s.name AS service_account_name
FROM api_tokens t
JOIN service_accounts s ON s.id = t.service_account_id
WHERE t.id = $1;

-- name: ListAPITokens :many
SELECT *
FROM api_tokens
WHERE service_account_id = sqlc.arg('service_account_id')
  AND (
      sqlc.narg('after_created_at')::timestamptz IS NULL
      OR (created_at, id) < (
          sqlc.narg('after_created_at')::timestamptz,
          sqlc.arg('after_id')::uuid
      )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: RevokeAPIToken :execrows
UPDATE api_tokens
SET revoked_at = COALESCE(revoked_at, $2)
WHERE id = $1;

-- name: TouchAPIToken :exec
UPDATE api_tokens
SET last_used_at = sqlc.arg('last_used_at')::timestamptz
WHERE id = sqlc.arg('id')::uuid
  AND (
      last_used_at IS NULL
      OR last_used_at < sqlc.arg('last_used_at')::timestamptz - interval '5 minutes'
  );

-- name: CreateAuditEvent :one
INSERT INTO audit_events (
    actor_token_id,
    repository_id,
    action,
    resource_type,
    resource_id,
    outcome,
    code,
    request_id,
    details
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListAuditEvents :many
SELECT *
FROM audit_events
WHERE (
    sqlc.narg('after_created_at')::timestamptz IS NULL
    OR (created_at, id) < (
        sqlc.narg('after_created_at')::timestamptz,
        sqlc.arg('after_id')::uuid
    )
)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: InsertIdempotencyRecord :one
INSERT INTO idempotency_records (
    token_id,
    http_method,
    canonical_resource,
    idempotency_key,
    request_fingerprint,
    state,
    expires_at
) VALUES ($1, $2, $3, $4, $5, 'pending', $6)
ON CONFLICT (token_id, http_method, canonical_resource, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetIdempotencyRecordForUpdate :one
SELECT *
FROM idempotency_records
WHERE token_id = $1
  AND http_method = $2
  AND canonical_resource = $3
  AND idempotency_key = $4
FOR UPDATE;

-- name: CompleteIdempotencyRecord :one
UPDATE idempotency_records
SET state = 'completed',
    http_status = $2,
    response_body = $3,
    response_encrypted = $4,
    completed_at = $5,
    updated_at = $5
WHERE id = $1
  AND state = 'pending'
RETURNING *;

-- name: DeleteExpiredIdempotencyRecords :execrows
DELETE FROM idempotency_records
WHERE expires_at < $1
  AND state = 'completed';

-- name: UpsertSigningKey :one
INSERT INTO signing_keys (key_id, algorithm, public_key, fingerprint, active)
VALUES ($1, 'Ed25519', $2, $3, $4)
ON CONFLICT (key_id) DO UPDATE
SET public_key = EXCLUDED.public_key,
    fingerprint = EXCLUDED.fingerprint,
    active = EXCLUDED.active
RETURNING *;

-- name: GetSigningKey :one
SELECT * FROM signing_keys WHERE key_id = $1;
