-- +goose Up

ALTER TABLE quota_snapshots
    ADD COLUMN provider_slug text,
    ADD COLUMN model text,
    ADD COLUMN observation_key text,
    ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'::jsonb;

UPDATE quota_snapshots qs
SET provider_slug = p.slug
FROM credentials c
JOIN providers p ON p.id = c.provider_id
WHERE qs.credential_id = c.id AND qs.provider_slug IS NULL;

UPDATE quota_snapshots
SET provider_slug = COALESCE(provider_slug, 'legacy'),
    model = COALESCE(model, ''),
    observation_key = COALESCE(observation_key, 'legacy:' || id::text)
WHERE provider_slug IS NULL OR model IS NULL OR observation_key IS NULL;

ALTER TABLE quota_snapshots
    ALTER COLUMN provider_slug SET NOT NULL,
    ALTER COLUMN model SET NOT NULL,
    ALTER COLUMN observation_key SET NOT NULL,
    ADD CONSTRAINT quota_snapshots_provider_slug_ck
        CHECK (length(btrim(provider_slug)) BETWEEN 1 AND 120),
    ADD CONSTRAINT quota_snapshots_model_ck
        CHECK (length(model) <= 255),
    ADD CONSTRAINT quota_snapshots_observation_key_ck
        CHECK (length(btrim(observation_key)) BETWEEN 1 AND 128),
    ADD CONSTRAINT quota_snapshots_metadata_object_ck
        CHECK (jsonb_typeof(metadata) = 'object'),
    ADD CONSTRAINT quota_snapshots_metadata_size_ck
        CHECK (pg_column_size(metadata) <= 8192),
    ADD CONSTRAINT quota_snapshots_error_class_ck
        CHECK (error_class IS NULL OR (length(btrim(error_class)) BETWEEN 1 AND 80));

CREATE UNIQUE INDEX quota_snapshots_observation_uq
    ON quota_snapshots (credential_id, observation_key, window_kind);
CREATE INDEX quota_snapshots_latest_idx
    ON quota_snapshots (credential_id, window_kind, observed_at DESC, id DESC);

CREATE TABLE quota_probe_states (
    credential_id uuid PRIMARY KEY REFERENCES credentials(id),
    provider_slug text NOT NULL CHECK (length(btrim(provider_slug)) BETWEEN 1 AND 120),
    probe_name text NOT NULL DEFAULT '' CHECK (length(probe_name) <= 120),
    status text NOT NULL DEFAULT 'unknown'
        CHECK (status IN ('unknown', 'success', 'no_observation', 'error', 'unsupported')),
    last_attempt_at timestamptz,
    last_observed_at timestamptz,
    next_attempt_at timestamptz,
    failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    last_error_class text,
    last_http_status integer,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (last_error_class IS NULL OR (length(btrim(last_error_class)) BETWEEN 1 AND 80)),
    CHECK (last_http_status IS NULL OR (last_http_status BETWEEN 100 AND 599)),
    CHECK (last_attempt_at IS NULL OR last_attempt_at <= updated_at + interval '5 minutes'),
    CHECK (last_observed_at IS NULL OR last_observed_at <= updated_at + interval '5 minutes')
);

CREATE INDEX quota_probe_states_due_idx
    ON quota_probe_states (next_attempt_at, credential_id);
CREATE INDEX quota_probe_states_status_idx
    ON quota_probe_states (provider_slug, status, updated_at DESC);

CREATE TRIGGER quota_snapshots_append_only
    BEFORE UPDATE OR DELETE ON quota_snapshots
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

-- +goose Down

DROP TRIGGER IF EXISTS quota_snapshots_append_only ON quota_snapshots;
DROP INDEX IF EXISTS quota_probe_states_status_idx;
DROP INDEX IF EXISTS quota_probe_states_due_idx;
DROP TABLE IF EXISTS quota_probe_states;
DROP INDEX IF EXISTS quota_snapshots_latest_idx;
DROP INDEX IF EXISTS quota_snapshots_observation_uq;
ALTER TABLE quota_snapshots
    DROP CONSTRAINT IF EXISTS quota_snapshots_error_class_ck,
    DROP CONSTRAINT IF EXISTS quota_snapshots_metadata_size_ck,
    DROP CONSTRAINT IF EXISTS quota_snapshots_metadata_object_ck,
    DROP CONSTRAINT IF EXISTS quota_snapshots_observation_key_ck,
    DROP CONSTRAINT IF EXISTS quota_snapshots_model_ck,
    DROP CONSTRAINT IF EXISTS quota_snapshots_provider_slug_ck,
    DROP COLUMN IF EXISTS metadata,
    DROP COLUMN IF EXISTS observation_key,
    DROP COLUMN IF EXISTS model,
    DROP COLUMN IF EXISTS provider_slug;
