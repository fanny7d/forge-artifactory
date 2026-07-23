-- +goose Up
CREATE TABLE products (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z][a-z0-9._-]{1,63}$'),
    package_id uuid NOT NULL UNIQUE REFERENCES packages(id),
    display_name text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 128),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 2048),
    command_name text NOT NULL CHECK (
        char_length(command_name) BETWEEN 1 AND 64
        AND command_name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'
    ),
    install_key uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    previous_install_key uuid UNIQUE,
    previous_install_key_expires_at timestamptz,
    created_by uuid NOT NULL REFERENCES api_tokens(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (previous_install_key IS NULL AND previous_install_key_expires_at IS NULL)
        OR
        (previous_install_key IS NOT NULL AND previous_install_key_expires_at IS NOT NULL)
    )
);

ALTER TABLE release_artifacts
ADD COLUMN install_spec jsonb NOT NULL DEFAULT '{}'::jsonb
CHECK (jsonb_typeof(install_spec) = 'object');

-- +goose Down
ALTER TABLE release_artifacts DROP COLUMN install_spec;
DROP TABLE products;
