-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE service_accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 128),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_account_id uuid NOT NULL REFERENCES service_accounts(id),
    secret_hmac bytea NOT NULL CHECK (octet_length(secret_hmac) = 32),
    scopes text[] NOT NULL CHECK (
        cardinality(scopes) BETWEEN 1 AND 5
        AND scopes <@ ARRAY[
            'artifact:read',
            'artifact:write',
            'release:publish',
            'channel:promote',
            'admin'
        ]::text[]
    ),
    repository_ids uuid[] NOT NULL DEFAULT '{}'::uuid[],
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_tokens_service_account_idx ON api_tokens(service_account_id, created_at, id);
CREATE INDEX api_tokens_active_idx ON api_tokens(id) WHERE revoked_at IS NULL;

CREATE TABLE repositories (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE CHECK (key ~ '^[a-z][a-z0-9._-]{1,63}$'),
    display_name text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 128),
    repository_type text NOT NULL DEFAULT 'local/raw' CHECK (repository_type = 'local/raw'),
    created_by uuid NOT NULL REFERENCES api_tokens(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_token_id uuid REFERENCES api_tokens(id),
    repository_id uuid REFERENCES repositories(id),
    action text NOT NULL CHECK (char_length(action) BETWEEN 1 AND 128),
    resource_type text NOT NULL CHECK (char_length(resource_type) BETWEEN 1 AND 64),
    resource_id text CHECK (char_length(resource_id) <= 128),
    outcome text NOT NULL CHECK (outcome IN ('success', 'denied', 'failed')),
    code text CHECK (char_length(code) <= 128),
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 64),
    details jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_created_idx ON audit_events(created_at DESC, id);
CREATE INDEX audit_events_actor_idx ON audit_events(actor_token_id, created_at DESC);
CREATE INDEX audit_events_repository_idx ON audit_events(repository_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION reject_audit_event_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_immutable
BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION reject_audit_event_mutation();

CREATE TABLE idempotency_records (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id uuid NOT NULL REFERENCES api_tokens(id),
    http_method text NOT NULL CHECK (http_method ~ '^[A-Z]+$'),
    canonical_resource text NOT NULL CHECK (char_length(canonical_resource) BETWEEN 1 AND 2048),
    idempotency_key text NOT NULL CHECK (
        char_length(idempotency_key) BETWEEN 1 AND 128
        AND idempotency_key ~ '^[A-Za-z0-9._:-]+$'
    ),
    request_fingerprint bytea NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    state text NOT NULL CHECK (state IN ('pending', 'completed')),
    http_status integer CHECK (http_status BETWEEN 200 AND 599),
    response_body bytea,
    response_encrypted boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (token_id, http_method, canonical_resource, idempotency_key),
    CHECK (
        (state = 'pending' AND http_status IS NULL AND response_body IS NULL AND completed_at IS NULL)
        OR
        (state = 'completed' AND http_status IS NOT NULL AND response_body IS NOT NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX idempotency_records_expiry_idx ON idempotency_records(expires_at);

CREATE TABLE signing_keys (
    key_id text PRIMARY KEY CHECK (char_length(key_id) BETWEEN 8 AND 128),
    algorithm text NOT NULL CHECK (algorithm = 'Ed25519'),
    public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
    fingerprint char(64) NOT NULL UNIQUE CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    active boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX signing_keys_one_active_idx ON signing_keys(active) WHERE active;

CREATE TABLE blobs (
    sha256 char(64) PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size bigint NOT NULL CHECK (size >= 0),
    object_key text NOT NULL UNIQUE CHECK (char_length(object_key) BETWEEN 1 AND 1024),
    state text NOT NULL CHECK (state IN ('creating', 'ready', 'deleting')),
    lease_owner uuid,
    lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    lease_expires_at timestamptz,
    delete_completed_at timestamptz,
    last_referenced_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (state = 'creating' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR state IN ('ready', 'deleting')
    ),
    CHECK (delete_completed_at IS NULL OR state = 'deleting')
);

CREATE INDEX blobs_cleanup_idx ON blobs(state, last_referenced_at, created_at);

CREATE TABLE upload_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id),
    logical_path text NOT NULL CHECK (octet_length(logical_path) BETWEEN 1 AND 1024),
    staging_key text NOT NULL UNIQUE CHECK (char_length(staging_key) BETWEEN 1 AND 1024),
    state text NOT NULL CHECK (state IN ('active', 'completed', 'failed')),
    lease_owner uuid NOT NULL,
    lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    lease_expires_at timestamptz NOT NULL,
    hard_deadline timestamptz NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    sha256 char(64) CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
    size bigint CHECK (size IS NULL OR size >= 0),
    cleanup_completed_at timestamptz,
    created_by uuid NOT NULL REFERENCES api_tokens(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CHECK (hard_deadline > created_at),
    CHECK ((state = 'completed' AND completed_at IS NOT NULL) OR state <> 'completed'),
    CHECK (cleanup_completed_at IS NULL OR state IN ('completed', 'failed'))
);

CREATE INDEX upload_sessions_cleanup_idx
ON upload_sessions(state, lease_expires_at, hard_deadline);

CREATE TABLE artifacts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id),
    logical_path text NOT NULL CHECK (octet_length(logical_path) BETWEEN 1 AND 1024),
    blob_sha256 char(64) NOT NULL REFERENCES blobs(sha256),
    media_type text NOT NULL CHECK (char_length(media_type) BETWEEN 1 AND 255),
    filename text NOT NULL CHECK (octet_length(filename) BETWEEN 1 AND 255),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(properties) = 'object'),
    created_by uuid NOT NULL REFERENCES api_tokens(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, logical_path)
);

CREATE INDEX artifacts_blob_idx ON artifacts(blob_sha256);

CREATE TABLE packages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id),
    name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9._-]{1,63}$'),
    created_by uuid NOT NULL REFERENCES api_tokens(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, name)
);

CREATE TABLE releases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    package_id uuid NOT NULL REFERENCES packages(id),
    version text NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$'),
    state text NOT NULL CHECK (state IN ('draft', 'publishing', 'published', 'publish_failed')),
    current_attempt_id uuid,
    published_at timestamptz,
    failure_code text CHECK (char_length(failure_code) <= 128),
    created_by uuid NOT NULL REFERENCES api_tokens(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (package_id, version),
    UNIQUE (package_id, id),
    CHECK ((state = 'published' AND published_at IS NOT NULL) OR state <> 'published')
);

CREATE TABLE release_artifacts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id uuid NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    artifact_id uuid NOT NULL REFERENCES artifacts(id),
    os text NOT NULL CHECK (char_length(os) BETWEEN 1 AND 64),
    arch text NOT NULL CHECK (char_length(arch) BETWEEN 1 AND 64),
    variant text NOT NULL DEFAULT '' CHECK (char_length(variant) <= 64),
    role text NOT NULL DEFAULT '' CHECK (char_length(role) <= 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (release_id, os, arch, variant, role)
);

CREATE TABLE publish_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id uuid NOT NULL REFERENCES releases(id),
    idempotency_record_id uuid UNIQUE REFERENCES idempotency_records(id) ON DELETE SET NULL,
    actor_token_id uuid NOT NULL REFERENCES api_tokens(id),
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 64),
    published_at timestamptz NOT NULL,
    snapshot jsonb NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    snapshot_sha256 char(64) NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
    key_id text NOT NULL REFERENCES signing_keys(key_id),
    lease_owner uuid NOT NULL,
    lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    lease_expires_at timestamptz NOT NULL,
    state text NOT NULL CHECK (state IN ('active', 'completed', 'aborted', 'failed')),
    storage_completed boolean NOT NULL DEFAULT false,
    manifest_sha256 char(64) CHECK (manifest_sha256 IS NULL OR manifest_sha256 ~ '^[0-9a-f]{64}$'),
    signature_sha256 char(64) CHECK (signature_sha256 IS NULL OR signature_sha256 ~ '^[0-9a-f]{64}$'),
    failure_code text CHECK (char_length(failure_code) <= 128),
    retry_count integer NOT NULL DEFAULT 0 CHECK (retry_count BETWEEN 0 AND 10),
    next_retry_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE releases
ADD CONSTRAINT releases_current_attempt_fk
FOREIGN KEY (current_attempt_id) REFERENCES publish_attempts(id);

CREATE UNIQUE INDEX publish_attempts_active_release_idx
ON publish_attempts(release_id) WHERE state = 'active';

CREATE INDEX publish_attempts_recovery_idx
ON publish_attempts(state, lease_expires_at, next_retry_at);

CREATE TABLE release_manifests (
    release_id uuid PRIMARY KEY REFERENCES releases(id),
    attempt_id uuid NOT NULL UNIQUE REFERENCES publish_attempts(id),
    manifest_blob_sha256 char(64) NOT NULL REFERENCES blobs(sha256),
    signature_blob_sha256 char(64) NOT NULL REFERENCES blobs(sha256),
    key_id text NOT NULL REFERENCES signing_keys(key_id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE channels (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    package_id uuid NOT NULL REFERENCES packages(id),
    name text NOT NULL CHECK (name IN ('candidate', 'stable')),
    current_release_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (package_id, name),
    UNIQUE (package_id, id),
    FOREIGN KEY (package_id, current_release_id) REFERENCES releases(package_id, id)
);

CREATE TABLE channel_revisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    package_id uuid NOT NULL REFERENCES packages(id),
    channel_id uuid NOT NULL,
    from_release_id uuid,
    to_release_id uuid NOT NULL,
    actor_token_id uuid NOT NULL REFERENCES api_tokens(id),
    reason text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 512),
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (package_id, channel_id) REFERENCES channels(package_id, id),
    FOREIGN KEY (package_id, from_release_id) REFERENCES releases(package_id, id),
    FOREIGN KEY (package_id, to_release_id) REFERENCES releases(package_id, id)
);

CREATE INDEX channel_revisions_history_idx ON channel_revisions(channel_id, created_at DESC, id);

CREATE TABLE jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL CHECK (kind IN (
        'cleanup_blob',
        'cleanup_upload',
        'cleanup_idempotency',
        'recover_publish'
    )),
    payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    state text NOT NULL CHECK (state IN ('pending', 'running', 'completed', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts integer NOT NULL DEFAULT 10 CHECK (max_attempts BETWEEN 1 AND 100),
    lease_owner uuid,
    lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    lease_expires_at timestamptz,
    available_at timestamptz NOT NULL DEFAULT now(),
    failure_code text CHECK (char_length(failure_code) <= 128),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobs_claim_idx ON jobs(state, available_at, lease_expires_at);
CREATE UNIQUE INDEX jobs_active_kind_idx
ON jobs(kind) WHERE state IN ('pending', 'running');

-- +goose Down
DROP TABLE jobs;
DROP TABLE channel_revisions;
DROP TABLE channels;
DROP TABLE release_manifests;
ALTER TABLE releases DROP CONSTRAINT releases_current_attempt_fk;
DROP TABLE publish_attempts;
DROP TABLE release_artifacts;
DROP TABLE releases;
DROP TABLE packages;
DROP TABLE artifacts;
DROP TABLE upload_sessions;
DROP TABLE blobs;
DROP TABLE signing_keys;
DROP TABLE idempotency_records;
DROP TRIGGER audit_events_immutable ON audit_events;
DROP FUNCTION reject_audit_event_mutation();
DROP TABLE audit_events;
DROP TABLE repositories;
DROP TABLE api_tokens;
DROP TABLE service_accounts;
