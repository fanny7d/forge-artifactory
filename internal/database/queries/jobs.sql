-- name: EnqueueJob :one
INSERT INTO jobs (kind, payload, state, max_attempts, available_at)
VALUES ($1, $2, 'pending', $3, $4)
ON CONFLICT (kind) WHERE state IN ('pending', 'running') DO NOTHING
RETURNING *;

-- name: ClaimJob :one
UPDATE jobs AS claimed
SET state = 'running',
    attempts = claimed.attempts + 1,
    lease_owner = $2,
    lease_generation = claimed.lease_generation + 1,
    lease_expires_at = $3,
    updated_at = $4
WHERE claimed.id = (
    SELECT candidate.id
    FROM jobs AS candidate
    WHERE (
        candidate.state = 'pending'
        OR (candidate.state = 'running' AND candidate.lease_expires_at < $4)
    )
      AND candidate.available_at <= $4
      AND candidate.attempts < candidate.max_attempts
      AND candidate.kind = $1
    ORDER BY candidate.available_at, candidate.created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: CompleteJob :execrows
UPDATE jobs
SET state = 'completed',
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = $4
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'running';

-- name: RetryJob :execrows
UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    failure_code = $4,
    available_at = $5,
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = $6
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'running';

-- name: ReapExpiredExhaustedJob :execrows
UPDATE jobs
SET state = 'failed',
    failure_code = 'job-attempts-exhausted',
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = $2
WHERE kind = $1
  AND state = 'running'
  AND lease_expires_at < $2
  AND attempts >= max_attempts;

-- name: ClaimOrphanBlob :one
UPDATE blobs AS claimed
SET state = 'deleting',
    lease_owner = sqlc.arg('lease_owner'),
    lease_generation = claimed.lease_generation + 1,
    lease_expires_at = sqlc.arg('lease_expires_at'),
    delete_completed_at = NULL,
    updated_at = sqlc.arg('now')
WHERE claimed.sha256 = (
    SELECT candidate.sha256
    FROM blobs AS candidate
    WHERE (
        (
            candidate.state = 'deleting'
            AND candidate.delete_completed_at IS NULL
            AND candidate.lease_expires_at < sqlc.arg('now')
        )
        OR (
            candidate.state = 'ready'
            AND COALESCE(candidate.last_referenced_at, candidate.created_at) < sqlc.arg('cutoff')
        )
        OR (
            candidate.state = 'creating'
            AND candidate.lease_expires_at < sqlc.arg('now')
            AND candidate.created_at < sqlc.arg('cutoff')
        )
    )
      AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_sha256 = candidate.sha256)
      AND NOT EXISTS (
          SELECT 1
          FROM release_manifests m
          WHERE m.manifest_blob_sha256 = candidate.sha256
             OR m.signature_blob_sha256 = candidate.sha256
      )
	  AND NOT EXISTS (
	      SELECT 1
	      FROM publish_attempts p
	      WHERE p.state = 'active'
	        AND (
	            p.manifest_sha256 = candidate.sha256
	            OR p.signature_sha256 = candidate.sha256
	        )
	  )
    ORDER BY candidate.created_at, candidate.sha256
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING claimed.*;

-- name: InsertStorageOrphanBlobTombstone :one
INSERT INTO blobs (
    sha256,
    size,
    object_key,
    state,
    lease_owner,
    lease_generation,
    lease_expires_at,
    created_at,
    updated_at
)
VALUES (
    sqlc.arg('sha256'),
    sqlc.arg('size'),
    sqlc.arg('object_key'),
    'deleting',
    sqlc.arg('lease_owner'),
    0,
    sqlc.arg('lease_expires_at'),
    sqlc.arg('now'),
    sqlc.arg('now')
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: RenewBlobDeleteLease :execrows
UPDATE blobs
SET lease_expires_at = $4,
    updated_at = $5
WHERE sha256 = $1
  AND state = 'deleting'
  AND delete_completed_at IS NULL
  AND lease_owner = $2
  AND lease_generation = $3;

-- name: MarkBlobDeleteCompleted :execrows
UPDATE blobs
SET delete_completed_at = $4,
    lease_expires_at = $5,
    updated_at = $4
WHERE sha256 = $1
  AND state = 'deleting'
  AND delete_completed_at IS NULL
  AND lease_owner = $2
  AND lease_generation = $3;

-- name: DeleteQuarantinedBlob :one
DELETE FROM blobs AS deleted
WHERE deleted.sha256 = (
    SELECT candidate.sha256
    FROM blobs AS candidate
    WHERE candidate.state = 'deleting'
      AND candidate.delete_completed_at IS NOT NULL
      AND candidate.lease_expires_at < sqlc.arg('now')
      AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_sha256 = candidate.sha256)
      AND NOT EXISTS (
          SELECT 1
          FROM release_manifests m
          WHERE m.manifest_blob_sha256 = candidate.sha256
             OR m.signature_blob_sha256 = candidate.sha256
      )
      AND NOT EXISTS (
          SELECT 1
          FROM publish_attempts p
          WHERE p.state = 'active'
            AND (
                p.manifest_sha256 = candidate.sha256
                OR p.signature_sha256 = candidate.sha256
            )
      )
    ORDER BY candidate.delete_completed_at, candidate.sha256
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING deleted.*;
