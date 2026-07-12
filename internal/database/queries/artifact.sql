-- name: CreateUploadSession :one
INSERT INTO upload_sessions (
    id,
    repository_id,
    logical_path,
    staging_key,
    state,
    lease_owner,
    lease_generation,
    lease_expires_at,
    hard_deadline,
    last_heartbeat_at,
    created_by
) VALUES ($1, $2, $3, $4, 'active', $5, 0, $6, $7, $8, $9)
RETURNING *;

-- name: HeartbeatUploadSession :execrows
UPDATE upload_sessions
SET lease_expires_at = $4,
    last_heartbeat_at = $5
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: CompleteUploadSession :execrows
UPDATE upload_sessions
SET state = 'completed',
    sha256 = $4,
    size = $5,
    completed_at = $6,
    cleanup_completed_at = $6
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: FailUploadSession :execrows
UPDATE upload_sessions
SET state = 'failed'
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: GetBlobForUpdate :one
SELECT * FROM blobs WHERE sha256 = $1 FOR UPDATE;

-- name: InsertCreatingBlob :one
INSERT INTO blobs (
    sha256,
    size,
    object_key,
    state,
    lease_owner,
    lease_generation,
    lease_expires_at
) VALUES ($1, $2, $3, 'creating', $4, 0, $5)
ON CONFLICT (sha256) DO NOTHING
RETURNING *;

-- name: TakeExpiredCreatingBlob :one
UPDATE blobs
SET lease_owner = $2,
    lease_generation = lease_generation + 1,
    lease_expires_at = $3,
    updated_at = $4
WHERE sha256 = $1
  AND state = 'creating'
  AND lease_expires_at < $4
RETURNING *;

-- name: MarkBlobReady :one
UPDATE blobs
SET state = 'ready',
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = $4
WHERE sha256 = $1
  AND state = 'creating'
  AND lease_owner = $2
  AND lease_generation = $3
RETURNING *;

-- name: TouchReadyBlob :execrows
UPDATE blobs
SET last_referenced_at = $2,
    updated_at = $2
WHERE sha256 = $1
  AND state = 'ready';

-- name: CreateArtifact :one
INSERT INTO artifacts (
    repository_id,
    logical_path,
    blob_sha256,
    media_type,
    filename,
    properties,
    created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetArtifactByPath :one
SELECT a.*, b.size, b.object_key, b.state AS blob_state
FROM artifacts a
JOIN blobs b ON b.sha256 = a.blob_sha256
WHERE a.repository_id = $1 AND a.logical_path = $2;

-- name: FindVisibleArtifactOutsideRepository :one
SELECT a.repository_id
FROM artifacts a
WHERE a.logical_path = sqlc.arg('logical_path')
  AND a.repository_id <> sqlc.arg('target_repository_id')
  AND (
      cardinality(sqlc.arg('visible_repository_ids')::uuid[]) = 0
      OR a.repository_id = ANY(sqlc.arg('visible_repository_ids')::uuid[])
  )
LIMIT 1;

-- name: FindVisibleBlobForChecksumDeploy :one
SELECT b.*
FROM blobs b
WHERE b.sha256 = sqlc.arg('sha256')
  AND b.state = 'ready'
  AND EXISTS (
      SELECT 1
      FROM artifacts a
      WHERE a.blob_sha256 = b.sha256
        AND (
            cardinality(sqlc.arg('visible_repository_ids')::uuid[]) = 0
            OR a.repository_id = ANY(sqlc.arg('visible_repository_ids')::uuid[])
        )
  )
LIMIT 1
FOR UPDATE OF b;

-- name: MarkOrphanBlobDeleting :one
UPDATE blobs b
SET state = 'deleting',
    lease_owner = $2,
    lease_generation = lease_generation + 1,
    lease_expires_at = $3,
    delete_completed_at = NULL,
    updated_at = $4
WHERE b.sha256 = $1
  AND (
      b.state = 'ready'
      OR (b.state = 'creating' AND b.lease_expires_at < $4)
  )
  AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_sha256 = b.sha256)
  AND NOT EXISTS (
      SELECT 1 FROM release_manifests m
      WHERE m.manifest_blob_sha256 = b.sha256 OR m.signature_blob_sha256 = b.sha256
  )
RETURNING b.*;

-- name: DeleteFencedBlob :execrows
DELETE FROM blobs
WHERE sha256 = $1
  AND state = 'deleting'
  AND lease_owner = $2
  AND lease_generation = $3;

-- name: ListExpiredUploadSessions :many
SELECT *
FROM upload_sessions
WHERE state = 'active'
  AND lease_expires_at < $1
  AND hard_deadline < $1
ORDER BY hard_deadline
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: ClaimExpiredUploadSession :one
UPDATE upload_sessions AS claimed
SET lease_owner = sqlc.arg('lease_owner')::uuid,
    lease_generation = claimed.lease_generation + 1,
    lease_expires_at = sqlc.arg('lease_expires_at')::timestamptz,
    last_heartbeat_at = sqlc.arg('now')::timestamptz
WHERE claimed.id = (
    SELECT candidate.id
    FROM upload_sessions AS candidate
    WHERE candidate.state IN ('active', 'failed')
      AND candidate.cleanup_completed_at IS NULL
      AND candidate.lease_expires_at < sqlc.arg('now')::timestamptz
      AND candidate.hard_deadline < sqlc.arg('now')::timestamptz
    ORDER BY candidate.hard_deadline, candidate.created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: HasPendingUploadSessionForStagingKey :one
SELECT EXISTS (
    SELECT 1
    FROM upload_sessions
    WHERE staging_key = $1
      AND cleanup_completed_at IS NULL
);

-- name: CompleteUploadCleanup :execrows
UPDATE upload_sessions
SET state = 'failed',
    cleanup_completed_at = $4
WHERE id = $1
  AND state IN ('active', 'failed')
  AND cleanup_completed_at IS NULL
  AND lease_owner = $2
  AND lease_generation = $3;
