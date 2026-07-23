-- name: CreateRelease :one
INSERT INTO releases (package_id, version, state, created_by)
VALUES ($1, $2, 'draft', $3)
RETURNING *;

-- name: GetReleaseByVersion :one
SELECT *
FROM releases
WHERE package_id = $1 AND version = $2;

-- name: GetReleaseByID :one
SELECT *
FROM releases
WHERE id = $1;

-- name: GetReleaseForUpdate :one
SELECT *
FROM releases
WHERE package_id = $1 AND version = $2
FOR UPDATE;

-- name: ListReleases :many
SELECT *
FROM releases
WHERE package_id = sqlc.arg('package_id')
  AND (
      sqlc.narg('after_created_at')::timestamptz IS NULL
      OR (created_at, id) < (
          sqlc.narg('after_created_at')::timestamptz,
          sqlc.arg('after_id')::uuid
      )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: AddReleaseArtifact :one
INSERT INTO release_artifacts (release_id, artifact_id, os, arch, variant, role, install_spec)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: RemoveReleaseArtifact :execrows
DELETE FROM release_artifacts ra
USING releases r
WHERE ra.id = sqlc.arg('release_artifact_id')
  AND ra.release_id = r.id
  AND r.id = sqlc.arg('release_id')
  AND r.state = 'draft';

-- name: ListReleaseArtifacts :many
SELECT ra.*, a.repository_id, a.logical_path, a.blob_sha256, a.media_type, a.filename,
       a.properties, a.created_by AS artifact_created_by, a.created_at AS artifact_created_at,
       b.size, b.state AS blob_state
FROM release_artifacts ra
JOIN artifacts a ON a.id = ra.artifact_id
JOIN blobs b ON b.sha256 = a.blob_sha256
WHERE ra.release_id = $1
ORDER BY ra.os, ra.arch, ra.variant, ra.role, a.logical_path;

-- name: CancelDraftRelease :execrows
DELETE FROM releases
WHERE id = $1 AND state = 'draft';

-- name: CreatePublishAttempt :one
INSERT INTO publish_attempts (
    id,
    release_id,
    idempotency_record_id,
    actor_token_id,
    request_id,
    published_at,
    snapshot,
    snapshot_sha256,
    key_id,
    lease_owner,
    lease_generation,
    lease_expires_at,
    state
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, $11, 'active')
RETURNING *;

-- name: GetPublishAttempt :one
SELECT *
FROM publish_attempts
WHERE id = $1;

-- name: ListRecoverablePublishAttemptIDs :many
SELECT id
FROM publish_attempts
WHERE state = 'active'
  AND lease_expires_at < sqlc.arg('now')::timestamptz
  AND (next_retry_at IS NULL OR next_retry_at <= sqlc.arg('now')::timestamptz)
  AND retry_count <= 10
ORDER BY lease_expires_at, id
LIMIT sqlc.arg('batch_size')
FOR UPDATE SKIP LOCKED;

-- name: GetPublishReleaseContext :one
SELECT r.id AS release_id,
       r.version,
       p.id AS package_id,
       p.name AS package_name,
       repo.id AS repository_id,
       repo.key AS repository_key
FROM releases r
JOIN packages p ON p.id = r.package_id
JOIN repositories repo ON repo.id = p.repository_id
WHERE r.id = $1;

-- name: SetReleasePublishing :execrows
UPDATE releases
SET state = 'publishing',
    current_attempt_id = $2,
    failure_code = NULL,
    updated_at = $3
WHERE id = $1 AND state = 'draft';

-- name: RenewPublishLease :execrows
UPDATE publish_attempts
SET lease_expires_at = $4,
    updated_at = $4
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: RecordPublishStorage :execrows
UPDATE publish_attempts
SET storage_completed = true,
    manifest_sha256 = $4,
    signature_sha256 = $5,
    updated_at = $6
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: RecordPublishFailure :execrows
UPDATE publish_attempts
SET failure_code = $4,
    next_retry_at = $5,
    updated_at = $6
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: FinalizePublishedRelease :execrows
WITH fenced_attempt AS (
    UPDATE publish_attempts
    SET state = 'completed',
        updated_at = $4
    WHERE id = $1
      AND lease_owner = $2
      AND lease_generation = $3
      AND state = 'active'
      AND storage_completed
    RETURNING release_id, manifest_sha256, signature_sha256, key_id, published_at
)
UPDATE releases r
SET state = 'published',
    published_at = a.published_at,
    failure_code = NULL,
    updated_at = $4
FROM fenced_attempt a
WHERE r.id = a.release_id
  AND r.current_attempt_id = $1
  AND r.state = 'publishing';

-- name: InsertReleaseManifest :one
INSERT INTO release_manifests (
    release_id,
    attempt_id,
    manifest_blob_sha256,
    signature_blob_sha256,
    key_id
) VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetReleaseManifest :one
SELECT rm.*, mb.object_key AS manifest_object_key, sb.object_key AS signature_object_key
FROM release_manifests rm
JOIN blobs mb ON mb.sha256 = rm.manifest_blob_sha256
JOIN blobs sb ON sb.sha256 = rm.signature_blob_sha256
WHERE rm.release_id = $1;

-- name: TakeExpiredPublishAttempt :one
UPDATE publish_attempts
SET lease_owner = $1,
    lease_generation = lease_generation + 1,
    lease_expires_at = $2,
    retry_count = LEAST(retry_count + 1, 10),
    updated_at = $3
WHERE id = (
    SELECT id
    FROM publish_attempts
    WHERE state = 'active'
      AND lease_expires_at < $3
      AND (next_retry_at IS NULL OR next_retry_at <= $3)
      AND retry_count <= 10
    ORDER BY lease_expires_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: TakePublishAttemptLease :one
UPDATE publish_attempts
SET lease_owner = $2,
    lease_generation = lease_generation + 1,
    lease_expires_at = $4,
    retry_count = LEAST(retry_count + 1, 10),
    updated_at = $3
WHERE id = $1
  AND state = 'active'
  AND lease_expires_at < $3
  AND (next_retry_at IS NULL OR next_retry_at <= $3)
  AND retry_count <= 10
RETURNING *;

-- name: FencePublishAttempt :execrows
UPDATE publish_attempts
SET updated_at = $4
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'active';

-- name: MarkPublishAttemptFailed :execrows
WITH failed_attempt AS (
    UPDATE publish_attempts
    SET state = 'failed',
        failure_code = $4,
        updated_at = $5
    WHERE id = $1
      AND lease_owner = $2
      AND lease_generation = $3
      AND state = 'active'
    RETURNING release_id
)
UPDATE releases r
SET state = 'publish_failed',
    failure_code = $4,
    updated_at = $5
FROM failed_attempt a
WHERE r.id = a.release_id AND r.current_attempt_id = $1;

-- name: AbortPublishAttemptToDraft :execrows
WITH aborted_attempt AS (
    UPDATE publish_attempts
    SET state = 'aborted',
        failure_code = $4,
        updated_at = $5
    WHERE id = $1
      AND lease_owner = $2
      AND lease_generation = $3
      AND state = 'active'
      AND NOT storage_completed
      AND manifest_sha256 IS NULL
      AND signature_sha256 IS NULL
    RETURNING release_id
)
UPDATE releases r
SET state = 'draft',
    current_attempt_id = NULL,
    failure_code = NULL,
    updated_at = $5
FROM aborted_attempt a
WHERE r.id = a.release_id AND r.current_attempt_id = $1;
