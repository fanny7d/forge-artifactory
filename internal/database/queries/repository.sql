-- name: CreateRepository :one
INSERT INTO repositories (key, display_name, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRepositoryByKey :one
SELECT * FROM repositories WHERE key = $1;

-- name: GetRepositoryByID :one
SELECT * FROM repositories WHERE id = $1;

-- name: ListRepositories :many
SELECT *
FROM repositories
WHERE (cardinality(sqlc.arg('repository_ids')::uuid[]) = 0 OR id = ANY(sqlc.arg('repository_ids')::uuid[]))
  AND (sqlc.arg('after_key')::text = '' OR key > sqlc.arg('after_key')::text)
ORDER BY key
LIMIT sqlc.arg('page_limit');

-- name: CreatePackage :one
INSERT INTO packages (repository_id, name, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: CreateDefaultChannel :one
INSERT INTO channels (package_id, name)
VALUES ($1, $2)
RETURNING *;

-- name: ListChannelsByPackage :many
SELECT *
FROM channels
WHERE package_id = $1
ORDER BY name;

-- name: GetPackageByName :one
SELECT p.*
FROM packages p
JOIN repositories r ON r.id = p.repository_id
WHERE r.key = $1 AND p.name = $2;

-- name: ListPackages :many
SELECT p.*
FROM packages p
WHERE p.repository_id = sqlc.arg('repository_id')
  AND (
      sqlc.narg('after_created_at')::timestamptz IS NULL
      OR (p.created_at, p.id) < (
          sqlc.narg('after_created_at')::timestamptz,
          sqlc.arg('after_id')::uuid
      )
  )
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg('page_limit');
