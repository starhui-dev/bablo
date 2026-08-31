-- +goose Up

-- amount_minor remains the signed business amount. The explicit deltas are the
-- authoritative balance movements so reservation transfers are reconstructible.
ALTER TABLE wallet_ledger
    ADD COLUMN available_delta_minor bigint,
    ADD COLUMN reserved_delta_minor bigint,
    ADD COLUMN available_balance_after_minor bigint,
    ADD COLUMN reserved_balance_after_minor bigint,
    ADD COLUMN source text;

UPDATE wallet_ledger
SET available_delta_minor = CASE entry_type
        WHEN 'reservation' THEN -abs(amount_minor)
        WHEN 'release' THEN abs(amount_minor)
        WHEN 'usage_charge' THEN -abs(amount_minor)
        ELSE amount_minor
    END,
    reserved_delta_minor = CASE entry_type
        WHEN 'reservation' THEN abs(amount_minor)
        WHEN 'release' THEN -abs(amount_minor)
        ELSE 0
    END,
    source = 'legacy';

ALTER TABLE wallet_ledger
    ALTER COLUMN available_delta_minor SET DEFAULT 0,
    ALTER COLUMN available_delta_minor SET NOT NULL,
    ALTER COLUMN reserved_delta_minor SET DEFAULT 0,
    ALTER COLUMN reserved_delta_minor SET NOT NULL,
    ALTER COLUMN source SET DEFAULT 'system',
    ALTER COLUMN source SET NOT NULL;

ALTER TABLE wallet_ledger
    DROP CONSTRAINT IF EXISTS wallet_ledger_entry_type_check;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_entry_type_check
        CHECK (entry_type IN (
            'reservation', 'usage_charge', 'release', 'recharge', 'refund',
            'adjustment', 'admin_adjustment', 'grant', 'bonus', 'expiration'
        ));
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_delta_check
        CHECK (available_delta_minor <> 0 OR reserved_delta_minor <> 0),
    ADD CONSTRAINT wallet_ledger_balance_snapshot_check
        CHECK (
            (available_balance_after_minor IS NULL OR available_balance_after_minor >= 0)
            AND (reserved_balance_after_minor IS NULL OR reserved_balance_after_minor >= 0)
        ),
    ADD CONSTRAINT wallet_ledger_source_check
        CHECK (source = btrim(source) AND length(source) BETWEEN 1 AND 64);

CREATE INDEX wallet_ledger_wallet_type_created_idx
    ON wallet_ledger (wallet_id, entry_type, created_at DESC);

CREATE TABLE wallet_reservations (
    id uuid PRIMARY KEY,
    wallet_id uuid NOT NULL REFERENCES wallets(id),
    user_id uuid NOT NULL REFERENCES users(id),
    api_key_id uuid REFERENCES api_keys(id),
    request_id text NOT NULL UNIQUE,
    request_record_id uuid REFERENCES request_records(id),
    model_id uuid REFERENCES models(id),
    provider_model_id uuid REFERENCES provider_models(id),
    route_version_id uuid REFERENCES route_versions(id),
    provider_id uuid REFERENCES providers(id),
    credential_id uuid REFERENCES credentials(id),
    price_version_id uuid NOT NULL REFERENCES price_versions(id),
    estimated_input_tokens bigint NOT NULL DEFAULT 0 CHECK (estimated_input_tokens >= 0),
    estimated_output_tokens bigint NOT NULL DEFAULT 0 CHECK (estimated_output_tokens >= 0),
    estimated_cache_read_tokens bigint NOT NULL DEFAULT 0 CHECK (estimated_cache_read_tokens >= 0),
    estimated_cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (estimated_cache_write_tokens >= 0),
    estimated_reasoning_tokens bigint NOT NULL DEFAULT 0 CHECK (estimated_reasoning_tokens >= 0),
    reservation_key text NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency char(3) NOT NULL,
    status text NOT NULL DEFAULT 'reserved'
        CHECK (status IN ('reserved', 'settlement_pending', 'settled', 'released')),
    settled_amount_minor bigint CHECK (settled_amount_minor IS NULL OR settled_amount_minor >= 0),
    usage_event_id uuid REFERENCES usage_events(id),
    reason text,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (wallet_id, reservation_key)
);
CREATE UNIQUE INDEX wallet_reservations_usage_event_uq
    ON wallet_reservations (usage_event_id)
    WHERE usage_event_id IS NOT NULL;
CREATE INDEX wallet_reservations_budget_idx
    ON wallet_reservations (api_key_id, status, created_at);
CREATE INDEX wallet_reservations_wallet_status_idx
    ON wallet_reservations (wallet_id, status, created_at);

-- A reservation must never be attached to another user's wallet, key, or
-- request record even when a future writer bypasses the Go service.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_wallet_reservation_owner()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM wallets
        WHERE id = NEW.wallet_id AND user_id = NEW.user_id
    ) THEN
        RAISE EXCEPTION 'wallet reservation owner does not match wallet'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.api_key_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM api_keys
        WHERE id = NEW.api_key_id AND user_id = NEW.user_id
    ) THEN
        RAISE EXCEPTION 'wallet reservation owner does not match API key'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.request_record_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM request_records
        WHERE id = NEW.request_record_id
          AND request_id = NEW.request_id
          AND (user_id IS NULL OR user_id = NEW.user_id)
          AND (api_key_id IS NULL OR api_key_id = NEW.api_key_id)
    ) THEN
        RAISE EXCEPTION 'wallet reservation owner does not match request record'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_reservations_owner_guard
    BEFORE INSERT OR UPDATE OF wallet_id, user_id, api_key_id, request_id, request_record_id
    ON wallet_reservations
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_wallet_reservation_owner();

CREATE TABLE billing_settlements (
    id uuid PRIMARY KEY,
    reservation_id uuid NOT NULL REFERENCES wallet_reservations(id),
    usage_event_id uuid NOT NULL REFERENCES usage_events(id),
    idempotency_key text NOT NULL UNIQUE,
    reserved_amount_minor bigint NOT NULL CHECK (reserved_amount_minor > 0),
    actual_amount_minor bigint CHECK (actual_amount_minor IS NULL OR actual_amount_minor >= 0),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'settled', 'failed', 'reconcile_needed')),
    estimated boolean NOT NULL DEFAULT false,
    error_class text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (reservation_id),
    UNIQUE (usage_event_id)
);
CREATE INDEX billing_settlements_status_idx
    ON billing_settlements (status, updated_at);


ALTER TABLE wallet_reservations
    ADD CONSTRAINT wallet_reservations_state_check
        CHECK (
            (status = 'reserved' AND settled_amount_minor IS NULL AND usage_event_id IS NULL)
            OR (status = 'settlement_pending' AND settled_amount_minor IS NOT NULL AND usage_event_id IS NOT NULL)
            OR (status = 'settled' AND settled_amount_minor IS NOT NULL AND usage_event_id IS NOT NULL)
            OR (status = 'released' AND settled_amount_minor IS NULL AND usage_event_id IS NULL)
        ),
    ADD CONSTRAINT wallet_reservations_text_check
        CHECK (
            request_id = btrim(request_id) AND length(request_id) BETWEEN 1 AND 128
            AND reservation_key = btrim(reservation_key) AND length(reservation_key) BETWEEN 1 AND 160
            AND (reason IS NULL OR (reason = btrim(reason) AND length(reason) BETWEEN 1 AND 128))
        );

ALTER TABLE billing_settlements
    ADD CONSTRAINT billing_settlements_state_check
        CHECK (
            (status = 'pending' AND actual_amount_minor IS NOT NULL AND error_class IS NOT NULL)
            OR (status = 'settled' AND actual_amount_minor IS NOT NULL AND error_class IS NULL)
            OR (status IN ('failed', 'reconcile_needed') AND error_class IS NOT NULL)
        ),
    ADD CONSTRAINT billing_settlements_text_check
        CHECK (
            idempotency_key = btrim(idempotency_key) AND length(idempotency_key) BETWEEN 1 AND 160
            AND (error_class IS NULL OR (error_class = btrim(error_class) AND length(error_class) BETWEEN 1 AND 128))
        );

ALTER TABLE usage_events
    ADD CONSTRAINT usage_events_billing_wallet_check
        CHECK (amount_minor IS NULL OR amount_minor = 0 OR wallet_id IS NOT NULL) NOT VALID;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_usage_wallet_owner()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.wallet_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallets
        WHERE id = NEW.wallet_id AND user_id = NEW.user_id
    ) THEN
        RAISE EXCEPTION 'usage event owner does not match wallet'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER usage_events_wallet_owner_guard
    BEFORE INSERT OR UPDATE OF wallet_id, user_id
    ON usage_events
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_usage_wallet_owner();

UPDATE wallet_ledger
SET amount_minor = CASE entry_type
        WHEN 'reservation' THEN abs(amount_minor)
        WHEN 'release' THEN -abs(amount_minor)
        WHEN 'usage_charge' THEN -abs(amount_minor)
        ELSE amount_minor
    END;

ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_amount_semantics_check
        CHECK (
            (entry_type = 'reservation'
                AND amount_minor > 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'release'
                AND amount_minor < 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'usage_charge'
                AND amount_minor < 0
                AND available_delta_minor <= 0
                AND reserved_delta_minor <= 0
                AND available_delta_minor + reserved_delta_minor = amount_minor)
            OR (entry_type IN ('recharge', 'refund', 'grant', 'bonus')
                AND amount_minor > 0
                AND available_delta_minor = amount_minor
                AND reserved_delta_minor = 0)
            OR (entry_type IN ('adjustment', 'admin_adjustment', 'expiration')
                AND available_delta_minor = amount_minor
                AND reserved_delta_minor = 0)
        ),
    ADD CONSTRAINT wallet_ledger_reference_check
        CHECK (
            reference_type = btrim(reference_type) AND length(reference_type) BETWEEN 1 AND 64
            AND reference_id = btrim(reference_id) AND length(reference_id) BETWEEN 1 AND 160
            AND idempotency_key = btrim(idempotency_key) AND length(idempotency_key) BETWEEN 1 AND 160
        ),
    ADD CONSTRAINT wallet_ledger_operator_check
        CHECK (entry_type <> 'admin_adjustment' OR operator_user_id IS NOT NULL);

DROP INDEX wallet_ledger_usage_event_uq;
CREATE UNIQUE INDEX wallet_ledger_usage_event_uq
    ON wallet_ledger (usage_event_id)
    WHERE usage_event_id IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_wallet_ledger_usage()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.entry_type = 'usage_charge' AND NEW.usage_event_id IS NULL THEN
        RAISE EXCEPTION 'usage charge requires usage event'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.usage_event_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM usage_events
        WHERE id = NEW.usage_event_id AND wallet_id = NEW.wallet_id
    ) THEN
        RAISE EXCEPTION 'ledger usage event does not match wallet'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_ledger_usage_guard
    BEFORE INSERT ON wallet_ledger
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_wallet_ledger_usage();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_billing_settlement()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM wallet_reservations AS reservation
        JOIN usage_events AS usage
          ON usage.id = NEW.usage_event_id
         AND usage.wallet_id = reservation.wallet_id
         AND usage.request_id = reservation.request_id
        WHERE reservation.id = NEW.reservation_id
          AND reservation.amount_minor = NEW.reserved_amount_minor
          AND usage.price_version_id = reservation.price_version_id
          AND usage.amount_minor = NEW.actual_amount_minor
          AND usage.estimated = NEW.estimated
    ) THEN
        RAISE EXCEPTION 'billing settlement does not match reservation and usage event'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER billing_settlements_consistency_guard
    BEFORE INSERT OR UPDATE OF reservation_id, usage_event_id,
        reserved_amount_minor, actual_amount_minor, estimated
    ON billing_settlements
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_billing_settlement();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_reject_wallet_ledger_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'wallet ledger is immutable'
        USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_ledger_immutable
    BEFORE UPDATE OR DELETE ON wallet_ledger
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_wallet_ledger_mutation();
-- +goose Down

DROP TRIGGER IF EXISTS wallet_ledger_immutable ON wallet_ledger;
DROP FUNCTION IF EXISTS bablo_reject_wallet_ledger_mutation();
DROP TRIGGER IF EXISTS billing_settlements_consistency_guard ON billing_settlements;
DROP FUNCTION IF EXISTS bablo_validate_billing_settlement();
DROP TRIGGER IF EXISTS wallet_ledger_usage_guard ON wallet_ledger;
DROP FUNCTION IF EXISTS bablo_validate_wallet_ledger_usage();
DROP INDEX IF EXISTS wallet_ledger_usage_event_uq;
CREATE UNIQUE INDEX wallet_ledger_usage_event_uq
    ON wallet_ledger (wallet_id, usage_event_id)
    WHERE usage_event_id IS NOT NULL;
ALTER TABLE wallet_ledger
    DROP CONSTRAINT IF EXISTS wallet_ledger_operator_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_reference_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_amount_semantics_check;
DROP TRIGGER IF EXISTS usage_events_wallet_owner_guard ON usage_events;
DROP FUNCTION IF EXISTS bablo_validate_usage_wallet_owner();
ALTER TABLE usage_events
    DROP CONSTRAINT IF EXISTS usage_events_billing_wallet_check;

DROP INDEX IF EXISTS billing_settlements_status_idx;
DROP TABLE IF EXISTS billing_settlements;
DROP TRIGGER IF EXISTS wallet_reservations_owner_guard ON wallet_reservations;
DROP FUNCTION IF EXISTS bablo_validate_wallet_reservation_owner();
DROP INDEX IF EXISTS wallet_reservations_wallet_status_idx;
DROP INDEX IF EXISTS wallet_reservations_budget_idx;
DROP INDEX IF EXISTS wallet_reservations_usage_event_uq;
DROP TABLE IF EXISTS wallet_reservations;
ALTER TABLE wallet_ledger
    DROP CONSTRAINT IF EXISTS wallet_ledger_source_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_balance_snapshot_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_delta_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_entry_type_check;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_entry_type_check
        CHECK (entry_type IN (
            'reservation', 'usage_charge', 'release', 'recharge', 'refund',
            'adjustment', 'grant', 'expiration'
        ));
DROP INDEX IF EXISTS wallet_ledger_wallet_type_created_idx;
ALTER TABLE wallet_ledger
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS reserved_balance_after_minor,
    DROP COLUMN IF EXISTS available_balance_after_minor,
    DROP COLUMN IF EXISTS reserved_delta_minor,
    DROP COLUMN IF EXISTS available_delta_minor;