-- +goose Up

ALTER TABLE api_keys
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN rotated_at timestamptz,
    ADD COLUMN secret_version bigint NOT NULL DEFAULT 1 CHECK (secret_version > 0);

CREATE INDEX api_keys_active_expiry_idx
    ON api_keys (expires_at)
    WHERE status = 'active' AND expires_at IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS api_keys_active_expiry_idx;
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS secret_version,
    DROP COLUMN IF EXISTS rotated_at,
    DROP COLUMN IF EXISTS updated_at;
