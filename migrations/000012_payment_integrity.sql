-- +goose Up

ALTER TABLE payment_orders
    DROP CONSTRAINT IF EXISTS payment_orders_status_check;
ALTER TABLE payment_orders
    ADD CONSTRAINT payment_orders_status_check
        CHECK (status IN (
            'created', 'pending', 'paid', 'failed', 'expired',
            'refund_pending', 'refunded', 'closed'
        )),
    ADD COLUMN checkout_data jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN failure_class text,
    ADD COLUMN refunded_at timestamptz,
    ADD COLUMN closed_at timestamptz,
    ADD COLUMN wallet_id uuid REFERENCES wallets(id),
    ADD COLUMN recharge_ledger_id uuid UNIQUE REFERENCES wallet_ledger(id),
    ADD COLUMN refund_ledger_id uuid UNIQUE REFERENCES wallet_ledger(id),
    ADD COLUMN provider_refund_no text,
    ADD COLUMN updated_by uuid REFERENCES users(id),
    ADD CONSTRAINT payment_orders_provider_format_check
        CHECK (payment_provider ~ '^[a-z][a-z0-9_-]{0,63}$'),
    ADD CONSTRAINT payment_orders_order_no_format_check
        CHECK (order_no = btrim(order_no) AND length(order_no) BETWEEN 8 AND 80),
    ADD CONSTRAINT payment_orders_idempotency_format_check
        CHECK (idempotency_key = btrim(idempotency_key) AND length(idempotency_key) BETWEEN 16 AND 160),
    ADD CONSTRAINT payment_orders_trade_format_check
        CHECK (
            (provider_trade_no IS NULL OR (
                provider_trade_no = btrim(provider_trade_no)
                AND length(provider_trade_no) BETWEEN 1 AND 160
            ))
            AND (provider_refund_no IS NULL OR (
                provider_refund_no = btrim(provider_refund_no)
                AND length(provider_refund_no) BETWEEN 1 AND 160
            ))
        ),
    ADD CONSTRAINT payment_orders_checkout_check
        CHECK (
            jsonb_typeof(checkout_data) = 'object'
            AND octet_length(checkout_data::text) <= 16384
        ),
    ADD CONSTRAINT payment_orders_failure_class_check
        CHECK (failure_class IS NULL OR (
            failure_class = btrim(failure_class)
            AND length(failure_class) BETWEEN 1 AND 64
        )),
    ADD CONSTRAINT payment_orders_time_check
        CHECK (
            (expires_at IS NULL OR expires_at > created_at)
            AND (paid_at IS NULL OR paid_at >= created_at)
            AND (refunded_at IS NULL OR (paid_at IS NOT NULL AND refunded_at >= paid_at))
            AND (closed_at IS NULL OR closed_at >= created_at)
        ),
    ADD CONSTRAINT payment_orders_terminal_fact_check
        CHECK (
            (status NOT IN ('paid', 'refund_pending', 'refunded')
                OR (paid_at IS NOT NULL AND provider_trade_no IS NOT NULL
                    AND wallet_id IS NOT NULL AND recharge_ledger_id IS NOT NULL))
            AND (status <> 'refunded' OR (refunded_at IS NOT NULL AND refund_ledger_id IS NOT NULL))
            AND (status <> 'closed' OR closed_at IS NOT NULL)
        );
CREATE UNIQUE INDEX payment_orders_provider_refund_uq
    ON payment_orders (payment_provider, provider_refund_no)
    WHERE provider_refund_no IS NOT NULL;
CREATE INDEX payment_orders_user_status_created_idx
    ON payment_orders (user_id, status, created_at DESC, id DESC);
CREATE INDEX payment_orders_expiry_idx
    ON payment_orders (expires_at, id)
    WHERE status IN ('created', 'pending') AND expires_at IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_payment_order_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id <> OLD.id
        OR NEW.order_no <> OLD.order_no
        OR NEW.user_id <> OLD.user_id
        OR NEW.amount_minor <> OLD.amount_minor
        OR NEW.currency <> OLD.currency
        OR NEW.payment_provider <> OLD.payment_provider
        OR NEW.idempotency_key <> OLD.idempotency_key
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'payment order identity is immutable'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.provider_trade_no IS NOT NULL
        AND NEW.provider_trade_no IS DISTINCT FROM OLD.provider_trade_no
    THEN
        RAISE EXCEPTION 'payment provider trade number is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.provider_refund_no IS NOT NULL
        AND NEW.provider_refund_no IS DISTINCT FROM OLD.provider_refund_no
    THEN
        RAISE EXCEPTION 'payment provider refund number is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.paid_at IS NOT NULL AND NEW.paid_at IS DISTINCT FROM OLD.paid_at THEN
        RAISE EXCEPTION 'payment paid timestamp is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.refunded_at IS NOT NULL AND NEW.refunded_at IS DISTINCT FROM OLD.refunded_at THEN
        RAISE EXCEPTION 'payment refunded timestamp is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.closed_at IS NOT NULL AND NEW.closed_at IS DISTINCT FROM OLD.closed_at THEN
        RAISE EXCEPTION 'payment closed timestamp is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.wallet_id IS NOT NULL AND NEW.wallet_id IS DISTINCT FROM OLD.wallet_id THEN
        RAISE EXCEPTION 'payment wallet is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.recharge_ledger_id IS NOT NULL
        AND NEW.recharge_ledger_id IS DISTINCT FROM OLD.recharge_ledger_id
    THEN
        RAISE EXCEPTION 'payment recharge ledger is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.refund_ledger_id IS NOT NULL
        AND NEW.refund_ledger_id IS DISTINCT FROM OLD.refund_ledger_id
    THEN
        RAISE EXCEPTION 'payment refund ledger is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.status <> OLD.status AND NOT (
        (OLD.status = 'created' AND NEW.status IN ('pending', 'paid', 'failed', 'expired', 'closed'))
        OR (OLD.status = 'pending' AND NEW.status IN ('paid', 'failed', 'expired', 'closed'))
        OR (OLD.status = 'paid' AND NEW.status = 'refund_pending')
        OR (OLD.status = 'refund_pending' AND NEW.status IN ('paid', 'refunded'))
        OR (OLD.status IN ('failed', 'expired') AND NEW.status = 'closed')
    ) THEN
        RAISE EXCEPTION 'invalid payment order transition % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'payment order updated_at cannot move backwards'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_orders_transition_guard
    BEFORE UPDATE ON payment_orders
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_payment_order_transition();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_payment_order_ledger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.wallet_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallets
        WHERE id = NEW.wallet_id
          AND user_id = NEW.user_id
          AND currency = NEW.currency
    ) THEN
        RAISE EXCEPTION 'payment wallet does not match order owner and currency'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.recharge_ledger_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger
        WHERE id = NEW.recharge_ledger_id
          AND wallet_id = NEW.wallet_id
          AND entry_type = 'recharge'
          AND amount_minor = NEW.amount_minor
          AND currency = NEW.currency
          AND reference_type = 'payment_order'
          AND reference_id = NEW.id::text
    ) THEN
        RAISE EXCEPTION 'payment recharge ledger does not match order'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.refund_ledger_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger
        WHERE id = NEW.refund_ledger_id
          AND wallet_id = NEW.wallet_id
          AND entry_type = 'payment_reversal'
          AND amount_minor = -NEW.amount_minor
          AND currency = NEW.currency
          AND reference_type = 'payment_order'
          AND reference_id = NEW.id::text
    ) THEN
        RAISE EXCEPTION 'payment refund ledger does not match order'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_orders_ledger_guard
    BEFORE INSERT OR UPDATE OF wallet_id, recharge_ledger_id, refund_ledger_id,
        user_id, currency, amount_minor
    ON payment_orders
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_payment_order_ledger();

ALTER TABLE payment_events
    ADD COLUMN merchant_id text,
    ADD COLUMN occurred_at timestamptz,
    ADD COLUMN error_class text,
    ADD CONSTRAINT payment_events_provider_format_check
        CHECK (payment_provider ~ '^[a-z][a-z0-9_-]{0,63}$'),
    ADD CONSTRAINT payment_events_identity_format_check
        CHECK (
            provider_event_id = btrim(provider_event_id)
            AND length(provider_event_id) BETWEEN 1 AND 160
            AND event_type IN ('paid', 'failed', 'expired', 'refunded', 'closed', 'unverified')
            AND (provider_trade_no IS NULL OR (
                provider_trade_no = btrim(provider_trade_no)
                AND length(provider_trade_no) BETWEEN 1 AND 160
            ))
            AND (merchant_id IS NULL OR (
                merchant_id = btrim(merchant_id)
                AND length(merchant_id) BETWEEN 1 AND 160
            ))
            AND (error_class IS NULL OR (
                error_class = btrim(error_class)
                AND length(error_class) BETWEEN 1 AND 64
            ))
        ),
    ADD CONSTRAINT payment_events_payload_hash_check
        CHECK (octet_length(payload_sha256) = 32),
    ADD CONSTRAINT payment_events_verified_fact_check
        CHECK (
            (signature_verified
                AND event_type <> 'unverified'
                AND provider_trade_no IS NOT NULL
                AND amount_minor IS NOT NULL AND amount_minor > 0
                AND currency IS NOT NULL
                AND merchant_id IS NOT NULL
                AND occurred_at IS NOT NULL)
            OR (NOT signature_verified
                AND event_type = 'unverified'
                AND order_id IS NULL
                AND provider_trade_no IS NULL
                AND amount_minor IS NULL
                AND currency IS NULL
                AND merchant_id IS NULL
                AND occurred_at IS NULL)
        );
CREATE UNIQUE INDEX payment_events_rejected_payload_uq
    ON payment_events (payment_provider, payload_sha256)
    WHERE signature_verified = false;
CREATE INDEX payment_events_provider_trade_received_idx
    ON payment_events (payment_provider, provider_trade_no, received_at DESC)
    WHERE provider_trade_no IS NOT NULL;
ALTER TABLE payment_event_processing
    ADD CONSTRAINT payment_event_processing_error_class_check
        CHECK (last_error_class IS NULL OR (
            last_error_class = btrim(last_error_class)
            AND length(last_error_class) BETWEEN 1 AND 64
        ));

ALTER TABLE wallet_ledger
    DROP CONSTRAINT IF EXISTS wallet_ledger_entry_type_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_amount_semantics_check;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_entry_type_check
        CHECK (entry_type IN (
            'reservation', 'usage_charge', 'release', 'recharge', 'refund',
            'adjustment', 'admin_adjustment', 'grant', 'bonus', 'expiration',
            'payment_refund_hold', 'payment_reversal', 'payment_refund_release'
        )),
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
            OR (entry_type = 'payment_refund_hold'
                AND amount_minor > 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'payment_reversal'
                AND amount_minor < 0
                AND available_delta_minor = 0
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'payment_refund_release'
                AND amount_minor < 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
        );

CREATE TABLE payment_vouchers (
    id uuid PRIMARY KEY,
    code_hash bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
    code_prefix text NOT NULL CHECK (
        code_prefix = btrim(code_prefix) AND length(code_prefix) BETWEEN 6 AND 20
    ),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    idempotency_key text NOT NULL UNIQUE CHECK (
        idempotency_key = btrim(idempotency_key) AND length(idempotency_key) BETWEEN 16 AND 160
    ),
    status text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'redeemed', 'revoked', 'expired')),
    expires_at timestamptz,
    created_by uuid NOT NULL REFERENCES users(id),
    redeemed_by uuid REFERENCES users(id),
    redeemed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at IS NULL OR expires_at > created_at),
    CHECK ((status = 'redeemed') = (redeemed_by IS NOT NULL AND redeemed_at IS NOT NULL)),
    CHECK (redeemed_at IS NULL OR redeemed_at >= created_at)
);
CREATE INDEX payment_vouchers_status_expiry_idx
    ON payment_vouchers (status, expires_at, id);
CREATE INDEX payment_vouchers_created_by_idx
    ON payment_vouchers (created_by, created_at DESC);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_payment_voucher_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id <> OLD.id
        OR NEW.code_hash <> OLD.code_hash
        OR NEW.code_prefix <> OLD.code_prefix
        OR NEW.amount_minor <> OLD.amount_minor
        OR NEW.currency <> OLD.currency
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
        OR NEW.created_by <> OLD.created_by
        OR NEW.created_at <> OLD.created_at
        OR NEW.idempotency_key <> OLD.idempotency_key
    THEN
        RAISE EXCEPTION 'payment voucher identity is immutable'
            USING ERRCODE = '55000';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
        OLD.status = 'active' AND NEW.status IN ('redeemed', 'revoked', 'expired')
    ) THEN
        RAISE EXCEPTION 'invalid payment voucher transition % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'payment voucher updated_at cannot move backwards'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_vouchers_transition_guard
    BEFORE UPDATE ON payment_vouchers
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_payment_voucher_transition();

-- +goose Down

DROP TRIGGER IF EXISTS payment_vouchers_transition_guard ON payment_vouchers;
DROP FUNCTION IF EXISTS bablo_validate_payment_voucher_transition();
DROP TABLE IF EXISTS payment_vouchers;

ALTER TABLE wallet_ledger
    DROP CONSTRAINT IF EXISTS wallet_ledger_entry_type_check,
    DROP CONSTRAINT IF EXISTS wallet_ledger_amount_semantics_check;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_entry_type_check
        CHECK (entry_type IN (
            'reservation', 'usage_charge', 'release', 'recharge', 'refund',
            'adjustment', 'admin_adjustment', 'grant', 'bonus', 'expiration'
        )),
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
        );

ALTER TABLE payment_event_processing
    DROP CONSTRAINT IF EXISTS payment_event_processing_error_class_check;
DROP INDEX IF EXISTS payment_events_provider_trade_received_idx;
DROP INDEX IF EXISTS payment_events_rejected_payload_uq;
ALTER TABLE payment_events
    DROP CONSTRAINT IF EXISTS payment_events_verified_fact_check,
    DROP CONSTRAINT IF EXISTS payment_events_payload_hash_check,
    DROP CONSTRAINT IF EXISTS payment_events_identity_format_check,
    DROP CONSTRAINT IF EXISTS payment_events_provider_format_check,
    DROP COLUMN IF EXISTS error_class,
    DROP COLUMN IF EXISTS occurred_at,
    DROP COLUMN IF EXISTS merchant_id;

DROP TRIGGER IF EXISTS payment_orders_ledger_guard ON payment_orders;
DROP FUNCTION IF EXISTS bablo_validate_payment_order_ledger();
DROP TRIGGER IF EXISTS payment_orders_transition_guard ON payment_orders;
DROP FUNCTION IF EXISTS bablo_validate_payment_order_transition();
DROP INDEX IF EXISTS payment_orders_expiry_idx;
DROP INDEX IF EXISTS payment_orders_user_status_created_idx;
DROP INDEX IF EXISTS payment_orders_provider_refund_uq;
ALTER TABLE payment_orders
    DROP CONSTRAINT IF EXISTS payment_orders_terminal_fact_check,
    DROP CONSTRAINT IF EXISTS payment_orders_time_check,
    DROP CONSTRAINT IF EXISTS payment_orders_failure_class_check,
    DROP CONSTRAINT IF EXISTS payment_orders_checkout_check,
    DROP CONSTRAINT IF EXISTS payment_orders_trade_format_check,
    DROP CONSTRAINT IF EXISTS payment_orders_idempotency_format_check,
    DROP CONSTRAINT IF EXISTS payment_orders_order_no_format_check,
    DROP COLUMN IF EXISTS provider_refund_no,
    DROP CONSTRAINT IF EXISTS payment_orders_provider_format_check,
    DROP COLUMN IF EXISTS updated_by,
    DROP COLUMN IF EXISTS refund_ledger_id,
    DROP COLUMN IF EXISTS recharge_ledger_id,
    DROP COLUMN IF EXISTS wallet_id,
    DROP COLUMN IF EXISTS closed_at,
    DROP COLUMN IF EXISTS refunded_at,
    DROP COLUMN IF EXISTS failure_class,
    DROP COLUMN IF EXISTS checkout_data,
    DROP CONSTRAINT IF EXISTS payment_orders_status_check;
ALTER TABLE payment_orders
    ADD CONSTRAINT payment_orders_status_check
        CHECK (status IN ('created', 'pending', 'paid', 'failed', 'expired', 'refunded', 'closed'));
