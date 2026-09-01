-- +goose Up

ALTER TABLE payment_orders
    ADD COLUMN provider_live_mode boolean;

ALTER TABLE payment_events
    ADD COLUMN provider_live_mode boolean;

ALTER TABLE payment_provider_operations
    ADD COLUMN merchant_id text,
    ADD COLUMN provider_live_mode boolean;
-- The pre-identity schema did not persist provider mode. Guessing test/live
-- would corrupt immutable financial facts, so populated databases require an
-- operator-reviewed reconciliation migration before this schema can advance.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM payment_orders WHERE provider_trade_no IS NOT NULL
    ) OR EXISTS (
        SELECT 1 FROM payment_events
        WHERE verification_source IN ('webhook_signature', 'provider_api')
    ) OR EXISTS (
        SELECT 1 FROM payment_provider_operations
    ) THEN
        RAISE EXCEPTION 'payment provider identity backfill required before migration 000015'
            USING ERRCODE = '55000',
                  HINT = 'derive merchant_id and provider_live_mode from the authoritative provider before retrying';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE payment_provider_operations
    ALTER COLUMN merchant_id SET NOT NULL,
    ALTER COLUMN provider_live_mode SET NOT NULL,
    ADD CONSTRAINT payment_provider_operations_merchant_check
        CHECK (merchant_id = btrim(merchant_id) AND length(merchant_id) BETWEEN 1 AND 160);

ALTER TABLE payment_orders
    ADD CONSTRAINT payment_orders_provider_identity_check CHECK (
        provider_trade_no IS NULL
        OR (merchant_id IS NOT NULL AND provider_live_mode IS NOT NULL)
    );


ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_verified_fact_check,
    ADD CONSTRAINT payment_events_verified_fact_check CHECK (
        (verification_source IN ('webhook_signature', 'provider_api')
            AND event_type <> 'unverified'
            AND provider_trade_no IS NOT NULL
            AND (event_type NOT IN ('refunded', 'refund_failed') OR provider_refund_no IS NOT NULL)
            AND amount_minor IS NOT NULL AND amount_minor > 0
            AND currency IS NOT NULL
            AND merchant_id IS NOT NULL
            AND provider_live_mode IS NOT NULL
            AND occurred_at IS NOT NULL)
        OR (verification_source = 'legacy_unverified'
            AND event_type = 'unverified'
            AND order_id IS NULL
            AND provider_trade_no IS NULL
            AND provider_refund_no IS NULL
            AND amount_minor IS NULL
            AND currency IS NULL
            AND merchant_id IS NULL
            AND provider_live_mode IS NULL
            AND occurred_at IS NULL)
    );

-- Provider identity is part of the financial fact. Once observed, it cannot
-- be changed by a later retry or webhook.
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

    IF OLD.merchant_id IS NOT NULL
        AND NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
    THEN
        RAISE EXCEPTION 'payment merchant identity is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.provider_live_mode IS NOT NULL
        AND NEW.provider_live_mode IS DISTINCT FROM OLD.provider_live_mode
    THEN
        RAISE EXCEPTION 'payment provider mode is immutable once assigned'
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

-- +goose Down

ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_verified_fact_check,
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

ALTER TABLE payment_orders
    DROP CONSTRAINT payment_orders_provider_identity_check;

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

ALTER TABLE payment_provider_operations
    DROP CONSTRAINT payment_provider_operations_merchant_check,
    DROP COLUMN provider_live_mode,
    DROP COLUMN merchant_id;

ALTER TABLE payment_events
    DROP COLUMN provider_live_mode;
ALTER TABLE payment_orders
    DROP COLUMN provider_live_mode;
