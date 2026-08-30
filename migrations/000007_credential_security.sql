-- +goose Up

ALTER TABLE credentials
    ADD COLUMN runtime_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT credentials_external_id_ck
        CHECK (external_stable_id = btrim(external_stable_id) AND length(external_stable_id) BETWEEN 1 AND 255) NOT VALID,
    ADD CONSTRAINT credentials_region_ck
        CHECK (region = '' OR region ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$') NOT VALID,
    ADD CONSTRAINT credentials_proxy_ref_ck
        CHECK (proxy_ref = '' OR proxy_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$') NOT VALID,
    ADD CONSTRAINT credentials_metadata_object_ck
        CHECK (jsonb_typeof(metadata) = 'object' AND jsonb_typeof(runtime_metadata) = 'object') NOT VALID;

ALTER TABLE credential_secrets
    DROP CONSTRAINT credential_secrets_secret_kind_check,
    ADD CONSTRAINT credential_secrets_secret_kind_check
        CHECK (secret_kind IN ('oauth_access', 'oauth_refresh', 'oauth_id', 'api_key', 'service_account', 'custom')),
    ADD CONSTRAINT credential_secrets_cipher_ck
        CHECK (octet_length(ciphertext) > 16 AND octet_length(nonce) = 12) NOT VALID,
    ADD CONSTRAINT credential_secrets_key_version_ck
        CHECK (key_version ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$') NOT VALID,
    ADD CONSTRAINT credential_secrets_rotation_time_ck
        CHECK (rotated_at IS NULL OR rotated_at >= created_at) NOT VALID;

ALTER TABLE credential_pools
    ADD CONSTRAINT credential_pools_name_ck
        CHECK (name = btrim(name) AND length(name) BETWEEN 1 AND 120) NOT VALID,
    ADD CONSTRAINT credential_pools_metadata_object_ck
        CHECK (jsonb_typeof(metadata) = 'object') NOT VALID;

ALTER TABLE credential_health
    ADD CONSTRAINT credential_health_error_class_ck
        CHECK (last_error_class IS NULL OR (last_error_class = btrim(last_error_class) AND length(last_error_class) BETWEEN 1 AND 80)) NOT VALID,
    ADD CONSTRAINT credential_health_observation_ck
        CHECK ((last_success_at IS NULL OR last_success_at <= observed_at)
            AND (last_error_at IS NULL OR last_error_at <= observed_at)) NOT VALID;

ALTER TABLE credentials VALIDATE CONSTRAINT credentials_external_id_ck;
ALTER TABLE credentials VALIDATE CONSTRAINT credentials_region_ck;
ALTER TABLE credentials VALIDATE CONSTRAINT credentials_proxy_ref_ck;
ALTER TABLE credentials VALIDATE CONSTRAINT credentials_metadata_object_ck;
ALTER TABLE credential_secrets VALIDATE CONSTRAINT credential_secrets_cipher_ck;
ALTER TABLE credential_secrets VALIDATE CONSTRAINT credential_secrets_key_version_ck;
ALTER TABLE credential_secrets VALIDATE CONSTRAINT credential_secrets_rotation_time_ck;
ALTER TABLE credential_pools VALIDATE CONSTRAINT credential_pools_name_ck;
ALTER TABLE credential_pools VALIDATE CONSTRAINT credential_pools_metadata_object_ck;
ALTER TABLE credential_health VALIDATE CONSTRAINT credential_health_error_class_ck;
ALTER TABLE credential_health VALIDATE CONSTRAINT credential_health_observation_ck;

CREATE UNIQUE INDEX credential_pools_name_ci_uq ON credential_pools (lower(name));
CREATE INDEX credential_secrets_credential_active_idx
    ON credential_secrets (credential_id, secret_kind, version_no DESC)
    WHERE rotated_at IS NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_credential_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.provider_id <> OLD.provider_id
        OR NEW.external_stable_id <> OLD.external_stable_id
        OR NEW.source_kind <> OLD.source_kind
    THEN
        RAISE EXCEPTION 'credential provider, external identity, and source kind are immutable'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.status = 'revoked' AND NEW.status <> 'revoked' THEN
        RAISE EXCEPTION 'revoked credential cannot be reactivated'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credentials_identity_guard
    BEFORE UPDATE ON credentials
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_credential_identity();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_credential_secret_kind()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    credential_source text;
BEGIN
    SELECT source_kind INTO credential_source FROM credentials WHERE id = NEW.credential_id;
    IF credential_source IS NULL
        OR (credential_source = 'api_key' AND NEW.secret_kind <> 'api_key')
        OR (credential_source = 'oauth' AND NEW.secret_kind NOT IN ('oauth_access', 'oauth_refresh', 'oauth_id'))
        OR (credential_source = 'service_account' AND NEW.secret_kind <> 'service_account')
        OR (credential_source = 'custom' AND NEW.secret_kind <> 'custom')
    THEN
        RAISE EXCEPTION 'credential secret kind does not match credential source kind'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credential_secrets_kind_guard
    BEFORE INSERT OR UPDATE OF credential_id, secret_kind ON credential_secrets
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_credential_secret_kind();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_credential_secret_history()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'credential secret history is immutable'
            USING ERRCODE = '55000';
    END IF;
    IF NEW.credential_id <> OLD.credential_id
        OR NEW.secret_kind <> OLD.secret_kind
        OR NEW.version_no <> OLD.version_no
        OR NEW.ciphertext <> OLD.ciphertext
        OR NEW.nonce <> OLD.nonce
        OR NEW.key_version <> OLD.key_version
        OR NEW.created_at <> OLD.created_at
        OR OLD.rotated_at IS NOT NULL
        OR NEW.rotated_at IS NULL
    THEN
        RAISE EXCEPTION 'credential secret history is immutable'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credential_secrets_history_guard
    BEFORE UPDATE OR DELETE ON credential_secrets
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_credential_secret_history();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_credential_pool_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.provider_id <> OLD.provider_id THEN
        RAISE EXCEPTION 'credential pool provider is immutable'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credential_pools_identity_guard
    BEFORE UPDATE ON credential_pools
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_credential_pool_identity();

-- +goose Down

DROP TRIGGER IF EXISTS credential_pools_identity_guard ON credential_pools;
DROP FUNCTION IF EXISTS bablo_guard_credential_pool_identity();
DROP TRIGGER IF EXISTS credential_secrets_history_guard ON credential_secrets;
DROP FUNCTION IF EXISTS bablo_guard_credential_secret_history();
DROP TRIGGER IF EXISTS credential_secrets_kind_guard ON credential_secrets;
DROP FUNCTION IF EXISTS bablo_validate_credential_secret_kind();
DROP TRIGGER IF EXISTS credentials_identity_guard ON credentials;
DROP FUNCTION IF EXISTS bablo_guard_credential_identity();
DROP INDEX IF EXISTS credential_secrets_credential_active_idx;
DROP INDEX IF EXISTS credential_pools_name_ci_uq;
ALTER TABLE credential_health
    DROP CONSTRAINT IF EXISTS credential_health_observation_ck,
    DROP CONSTRAINT IF EXISTS credential_health_error_class_ck;
ALTER TABLE credential_pools
    DROP CONSTRAINT IF EXISTS credential_pools_metadata_object_ck,
    DROP CONSTRAINT IF EXISTS credential_pools_name_ck;
ALTER TABLE credential_secrets
    DROP CONSTRAINT IF EXISTS credential_secrets_rotation_time_ck,
    DROP CONSTRAINT IF EXISTS credential_secrets_key_version_ck,
    DROP CONSTRAINT IF EXISTS credential_secrets_cipher_ck,
    DROP CONSTRAINT credential_secrets_secret_kind_check,
    ADD CONSTRAINT credential_secrets_secret_kind_check
        CHECK (secret_kind IN ('oauth_access', 'oauth_refresh', 'api_key', 'service_account', 'custom'));
ALTER TABLE credentials
    DROP CONSTRAINT IF EXISTS credentials_metadata_object_ck,
    DROP CONSTRAINT IF EXISTS credentials_proxy_ref_ck,
    DROP CONSTRAINT IF EXISTS credentials_region_ck,
    DROP CONSTRAINT IF EXISTS credentials_external_id_ck,
    DROP COLUMN IF EXISTS runtime_metadata;
