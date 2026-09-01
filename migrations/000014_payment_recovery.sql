-- +goose Up

CREATE TABLE payment_provider_operations (
    id uuid PRIMARY KEY,
    payment_order_id uuid NOT NULL REFERENCES payment_orders(id),
    operation_type text NOT NULL CHECK (operation_type IN ('create', 'refund')),
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    status text NOT NULL CHECK (status IN ('pending', 'processing', 'retryable', 'succeeded', 'definitive_failed')),
    owner_token uuid,
    lease_expires_at timestamptz,
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    provider_reference text,
    last_error_class text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_order_id, operation_type),
    CHECK (
        (status = 'processing' AND owner_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'processing' AND owner_token IS NULL AND lease_expires_at IS NULL)
    ),
    CHECK (provider_reference IS NULL OR (
        provider_reference = btrim(provider_reference)
        AND length(provider_reference) BETWEEN 1 AND 160
    )),
    CHECK (last_error_class IS NULL OR (
        last_error_class = btrim(last_error_class)
        AND length(last_error_class) BETWEEN 1 AND 64
    ))
);
CREATE INDEX payment_provider_operations_due_idx
    ON payment_provider_operations (next_attempt_at, payment_order_id)
    WHERE status IN ('pending', 'retryable', 'processing');


ALTER TABLE payment_orders
    ADD COLUMN merchant_id text,
    ADD COLUMN refund_hold_ledger_id uuid UNIQUE REFERENCES wallet_ledger(id),
    ADD CONSTRAINT payment_orders_merchant_format_check CHECK (
        merchant_id IS NULL OR (
            merchant_id = btrim(merchant_id)
            AND length(merchant_id) BETWEEN 1 AND 160
        )
    );

ALTER TABLE payment_events
    ADD COLUMN verification_source text;
UPDATE payment_events
SET verification_source = CASE
    WHEN signature_verified THEN 'webhook_signature'
    ELSE 'legacy_unverified'
END;
ALTER TABLE payment_events
    ALTER COLUMN verification_source SET NOT NULL,
    DROP CONSTRAINT payment_events_verified_fact_check,
    ADD CONSTRAINT payment_events_verification_source_check CHECK (
        verification_source IN ('webhook_signature', 'provider_api', 'legacy_unverified')
        AND signature_verified = (verification_source = 'webhook_signature')
    ),
    ADD CONSTRAINT payment_events_verified_fact_check CHECK (
        (verification_source IN ('webhook_signature', 'provider_api')
            AND event_type <> 'unverified'
            AND provider_trade_no IS NOT NULL
            AND (event_type NOT IN ('refunded', 'refund_failed') OR provider_refund_no IS NOT NULL)
            AND amount_minor IS NOT NULL AND amount_minor > 0
            AND currency IS NOT NULL
            AND merchant_id IS NOT NULL
            AND occurred_at IS NOT NULL)
        OR (verification_source = 'legacy_unverified'
            AND event_type = 'unverified'
            AND order_id IS NULL
            AND provider_trade_no IS NULL
            AND provider_refund_no IS NULL
            AND amount_minor IS NULL
            AND currency IS NULL
            AND merchant_id IS NULL
            AND occurred_at IS NULL)
    );
ALTER TABLE payment_vouchers
    ADD COLUMN code_ciphertext bytea,
    ADD COLUMN code_nonce bytea,
    ADD COLUMN code_key_version text;

ALTER TABLE payment_vouchers
    ADD CONSTRAINT payment_vouchers_secret_shape_check
    CHECK (
        (code_ciphertext IS NULL AND code_nonce IS NULL AND code_key_version IS NULL)
        OR (code_ciphertext IS NOT NULL AND octet_length(code_ciphertext) > 16
            AND code_nonce IS NOT NULL AND octet_length(code_nonce) = 12
            AND code_key_version IS NOT NULL
            AND code_key_version = btrim(code_key_version)
            AND length(code_key_version) BETWEEN 1 AND 64)
    );

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
    IF NEW.refund_hold_ledger_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger
        WHERE id = NEW.refund_hold_ledger_id
          AND wallet_id = NEW.wallet_id
          AND entry_type = 'payment_refund_hold'
          AND amount_minor = NEW.amount_minor
          AND currency = NEW.currency
          AND reference_type = 'payment_order'
          AND reference_id = NEW.id::text
    ) THEN
        RAISE EXCEPTION 'payment refund hold ledger does not match order'
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
    IF NEW.status = 'refund_pending' AND NEW.refund_hold_ledger_id IS NULL THEN
        RAISE EXCEPTION 'refund-pending payment order requires a refund hold ledger'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER payment_orders_ledger_guard ON payment_orders;
CREATE TRIGGER payment_orders_ledger_guard
    BEFORE INSERT OR UPDATE OF wallet_id, recharge_ledger_id, refund_hold_ledger_id,
        refund_ledger_id, status, user_id, currency, amount_minor
    ON payment_orders
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_payment_order_ledger();

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
    IF OLD.refund_hold_ledger_id IS NOT NULL
        AND NEW.refund_hold_ledger_id IS DISTINCT FROM OLD.refund_hold_ledger_id
    THEN
        RAISE EXCEPTION 'payment refund hold ledger is immutable once assigned'
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
        OR (OLD.status IN ('failed', 'expired') AND NEW.status IN ('pending', 'paid', 'closed'))
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
    IF OLD.status <> 'active' AND (
        NEW.status IS DISTINCT FROM OLD.status
        OR NEW.redeemed_by IS DISTINCT FROM OLD.redeemed_by
        OR NEW.redeemed_at IS DISTINCT FROM OLD.redeemed_at
    ) THEN
        RAISE EXCEPTION 'terminal payment voucher fact is immutable'
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

-- +goose Down

DROP TABLE IF EXISTS payment_provider_operations;

DROP TRIGGER payment_orders_ledger_guard ON payment_orders;
ALTER TABLE payment_orders DROP COLUMN refund_hold_ledger_id;
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
