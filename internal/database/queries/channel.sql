-- name: GetChannelByName :one
SELECT *
FROM channels
WHERE package_id = $1 AND name = $2;

-- name: GetChannelForUpdate :one
SELECT *
FROM channels
WHERE package_id = $1 AND name = $2
FOR UPDATE;

-- name: PromoteChannel :one
WITH previous AS (
    SELECT current_release_id
    FROM channels
    WHERE id = $1 AND package_id = $2
    FOR UPDATE
), updated AS (
    UPDATE channels
    SET current_release_id = $3
    WHERE id = $1 AND package_id = $2
    RETURNING id
)
INSERT INTO channel_revisions (
    package_id,
    channel_id,
    from_release_id,
    to_release_id,
    actor_token_id,
    reason,
    request_id
)
SELECT $2, $1, previous.current_release_id, $3, $4, $5, $6
FROM previous, updated
RETURNING *;

-- name: ListChannelHistory :many
SELECT cr.*,
       previous.version AS from_version,
       target.version AS to_version
FROM channel_revisions cr
LEFT JOIN releases previous ON previous.id = cr.from_release_id
JOIN releases target ON target.id = cr.to_release_id
WHERE cr.channel_id = sqlc.arg('channel_id')
  AND (
      sqlc.narg('after_created_at')::timestamptz IS NULL
      OR (cr.created_at, cr.id) < (
          sqlc.narg('after_created_at')::timestamptz,
          sqlc.arg('after_id')::uuid
      )
  )
ORDER BY cr.created_at DESC, cr.id DESC
LIMIT sqlc.arg('page_limit');

-- name: ResolveChannelArtifact :one
SELECT r.id AS release_id,
       r.version,
       rm.manifest_blob_sha256,
       rm.signature_blob_sha256,
       rm.key_id,
       manifest_blob.object_key AS manifest_object_key,
       signature_blob.object_key AS signature_object_key,
       signing_key.public_key,
       ra.id AS release_artifact_id,
       ra.os,
       ra.arch,
       ra.variant,
       ra.role,
       a.logical_path,
       a.blob_sha256,
       a.media_type,
       a.filename,
       b.size,
       b.object_key
FROM channels c
JOIN releases r ON r.id = c.current_release_id AND r.package_id = c.package_id
JOIN release_manifests rm ON rm.release_id = r.id
JOIN blobs manifest_blob ON manifest_blob.sha256 = rm.manifest_blob_sha256
JOIN blobs signature_blob ON signature_blob.sha256 = rm.signature_blob_sha256
JOIN signing_keys signing_key ON signing_key.key_id = rm.key_id
JOIN release_artifacts ra ON ra.release_id = r.id
JOIN artifacts a ON a.id = ra.artifact_id
JOIN blobs b ON b.sha256 = a.blob_sha256
WHERE c.package_id = $1
  AND c.name = $2
  AND ra.os = $3
  AND ra.arch = $4
  AND ra.variant = $5
  AND ra.role = $6
  AND r.state = 'published'
  AND b.state = 'ready'
  AND manifest_blob.state = 'ready'
  AND signature_blob.state = 'ready';
