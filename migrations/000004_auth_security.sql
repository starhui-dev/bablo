-- +goose Up

ALTER TABLE users
    ADD COLUMN password_changed_at timestamptz NOT NULL DEFAULT now();

ALTER TABLE user_sessions
    ADD COLUMN csrf_token_hash bytea,
    ADD COLUMN mfa_verified_at timestamptz;

-- No Bablo Web Session existed before this migration. Revoking any pre-existing
-- rows is safer than accepting sessions that have no server-bound CSRF token.
UPDATE user_sessions
SET revoked_at = COALESCE(revoked_at, now())
WHERE csrf_token_hash IS NULL;

CREATE INDEX user_sessions_token_active_idx
    ON user_sessions (token_hash, expires_at)
    WHERE revoked_at IS NULL AND csrf_token_hash IS NOT NULL;

ALTER TABLE mfa_factors
    ADD COLUMN last_totp_counter bigint CHECK (last_totp_counter IS NULL OR last_totp_counter >= 0);

CREATE UNIQUE INDEX mfa_factors_user_type_uq
    ON mfa_factors (user_id, factor_type);
CREATE INDEX mfa_recovery_codes_available_idx
    ON mfa_recovery_codes (factor_id, code_hash)
    WHERE consumed_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS mfa_recovery_codes_available_idx;
DROP INDEX IF EXISTS mfa_factors_user_type_uq;
ALTER TABLE mfa_factors DROP COLUMN IF EXISTS last_totp_counter;
DROP INDEX IF EXISTS user_sessions_token_active_idx;
ALTER TABLE user_sessions
    DROP COLUMN IF EXISTS mfa_verified_at,
    DROP COLUMN IF EXISTS csrf_token_hash;
ALTER TABLE users DROP COLUMN IF EXISTS password_changed_at;
