-- name: EnsureCLIRepository :one
INSERT INTO repositories (key, display_name, created_by)
VALUES ('cli-releases', 'CLI Releases', $1)
ON CONFLICT (key) DO UPDATE
SET key = repositories.key
RETURNING *;

-- name: EnsureProductPackage :one
INSERT INTO packages (repository_id, name, created_by)
VALUES ($1, $2, $3)
ON CONFLICT (repository_id, name) DO UPDATE
SET name = packages.name
RETURNING *;

-- name: EnsureProductChannel :one
INSERT INTO channels (package_id, name)
VALUES ($1, $2)
ON CONFLICT (package_id, name) DO UPDATE
SET name = channels.name
RETURNING *;

-- name: CreateProduct :one
INSERT INTO products (
    slug,
    package_id,
    display_name,
    description,
    command_name,
    created_by
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: RotateProductInstallKey :one
UPDATE products
SET previous_install_key = install_key,
    previous_install_key_expires_at = now() + interval '30 days',
    install_key = gen_random_uuid(),
    updated_at = now()
WHERE slug = $1
RETURNING *;

-- name: GetVisibleProductBySlug :one
SELECT product.id,
       product.slug,
       product.package_id,
       product.display_name,
       product.description,
       product.command_name,
       product.install_key,
       product.created_by,
       product.created_at,
       product.updated_at,
       package.repository_id,
       repository.key AS repository_key,
       package.name AS package_name,
       stable_release.version AS current_stable_version,
       stable_release.published_at AS published_at,
       COALESCE((
           SELECT jsonb_agg(
               jsonb_build_object(
                   'os', platform.os,
                   'arch', platform.arch,
                   'variant', platform.variant,
                   'strategy', platform.strategy,
                   'format', platform.format
               )
               ORDER BY platform.os, platform.arch, platform.variant, platform.strategy, platform.format
           )
           FROM (
               SELECT DISTINCT release_artifact.os,
                               release_artifact.arch,
                               release_artifact.variant,
                               COALESCE(release_artifact.install_spec ->> 'strategy', '') AS strategy,
                               COALESCE(release_artifact.install_spec ->> 'format', '') AS format
               FROM release_artifacts release_artifact
               WHERE release_artifact.release_id = stable_release.id
                 AND release_artifact.role = 'binary'
                 AND release_artifact.install_spec <> '{}'::jsonb
           ) platform
       ), '[]'::jsonb)::text AS platforms_json
FROM products product
JOIN packages package ON package.id = product.package_id
JOIN repositories repository ON repository.id = package.repository_id
LEFT JOIN channels stable_channel
       ON stable_channel.package_id = package.id
      AND stable_channel.name = 'stable'
LEFT JOIN releases stable_release
       ON stable_release.id = stable_channel.current_release_id
      AND stable_release.state = 'published'
WHERE product.slug = sqlc.arg('slug')
  AND (
      sqlc.arg('include_all')::boolean
      OR package.repository_id = ANY(sqlc.arg('repository_ids')::uuid[])
  );

-- name: GetProductByInstallKey :one
SELECT product.id,
       product.slug,
       product.package_id,
       product.display_name,
       product.description,
       product.command_name,
       product.install_key,
       product.created_by,
       product.created_at,
       product.updated_at,
       package.repository_id,
       repository.key AS repository_key,
       package.name AS package_name,
       stable_release.version AS current_stable_version,
       stable_release.published_at AS published_at,
       COALESCE((
           SELECT jsonb_agg(
               jsonb_build_object(
                   'os', platform.os,
                   'arch', platform.arch,
                   'variant', platform.variant,
                   'strategy', platform.strategy,
                   'format', platform.format
               )
               ORDER BY platform.os, platform.arch, platform.variant, platform.strategy, platform.format
           )
           FROM (
               SELECT DISTINCT release_artifact.os,
                               release_artifact.arch,
                               release_artifact.variant,
                               COALESCE(release_artifact.install_spec ->> 'strategy', '') AS strategy,
                               COALESCE(release_artifact.install_spec ->> 'format', '') AS format
               FROM release_artifacts release_artifact
               WHERE release_artifact.release_id = stable_release.id
                 AND release_artifact.role = 'binary'
                 AND release_artifact.install_spec <> '{}'::jsonb
           ) platform
       ), '[]'::jsonb)::text AS platforms_json
FROM products product
JOIN packages package ON package.id = product.package_id
JOIN repositories repository ON repository.id = package.repository_id
LEFT JOIN channels stable_channel
       ON stable_channel.package_id = package.id
      AND stable_channel.name = 'stable'
LEFT JOIN releases stable_release
       ON stable_release.id = stable_channel.current_release_id
      AND stable_release.state = 'published'
WHERE product.install_key = $1
   OR (
       product.previous_install_key = $1
       AND product.previous_install_key_expires_at > now()
   );

-- name: ListVisibleProducts :many
SELECT product.id,
       product.slug,
       product.package_id,
       product.display_name,
       product.description,
       product.command_name,
       product.install_key,
       product.created_by,
       product.created_at,
       product.updated_at,
       package.repository_id,
       repository.key AS repository_key,
       package.name AS package_name,
       stable_release.version AS current_stable_version,
       stable_release.published_at AS published_at,
       COALESCE((
           SELECT jsonb_agg(
               jsonb_build_object(
                   'os', platform.os,
                   'arch', platform.arch,
                   'variant', platform.variant,
                   'strategy', platform.strategy,
                   'format', platform.format
               )
               ORDER BY platform.os, platform.arch, platform.variant, platform.strategy, platform.format
           )
           FROM (
               SELECT DISTINCT release_artifact.os,
                               release_artifact.arch,
                               release_artifact.variant,
                               COALESCE(release_artifact.install_spec ->> 'strategy', '') AS strategy,
                               COALESCE(release_artifact.install_spec ->> 'format', '') AS format
               FROM release_artifacts release_artifact
               WHERE release_artifact.release_id = stable_release.id
                 AND release_artifact.role = 'binary'
                 AND release_artifact.install_spec <> '{}'::jsonb
           ) platform
       ), '[]'::jsonb)::text AS platforms_json
FROM products product
JOIN packages package ON package.id = product.package_id
JOIN repositories repository ON repository.id = package.repository_id
LEFT JOIN channels stable_channel
       ON stable_channel.package_id = package.id
      AND stable_channel.name = 'stable'
LEFT JOIN releases stable_release
       ON stable_release.id = stable_channel.current_release_id
      AND stable_release.state = 'published'
WHERE (
      sqlc.arg('include_all')::boolean
      OR package.repository_id = ANY(sqlc.arg('repository_ids')::uuid[])
  )
  AND (
      sqlc.arg('after_slug')::text = ''
      OR product.slug > sqlc.arg('after_slug')::text
  )
ORDER BY product.slug
LIMIT sqlc.arg('page_limit');

-- name: GetProductByPackageID :one
SELECT *
FROM products
WHERE package_id = $1;
