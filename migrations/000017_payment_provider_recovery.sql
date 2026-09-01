-- +goose Up

ALTER TABLE payment_orders
    ADD COLUMN provider_payment_intent_no text,
    ADD COLUMN provider_charge_no text,
    ADD COLUMN external_refunded_amount_minor bigint NOT NULL DEFAULT 0 CHECK (
        external_refunded_amount_minor >= 0 AND external_refunded_amount_minor <= amount_minor
    ),
    ADD CONSTRAINT payment_orders_provider_payment_intent_format_check CHECK (
        provider_payment_intent_no IS NULL OR (
            provider_payment_intent_no = btrim(provider_payment_intent_no)
            AND length(provider_payment_intent_no) BETWEEN 1 AND 160
        )
    ),
    ADD CONSTRAINT payment_orders_provider_charge_format_check CHECK (
        provider_charge_no IS NULL OR (
            provider_charge_no = btrim(provider_charge_no)
            AND length(provider_charge_no) BETWEEN 1 AND 160
        )
    );
CREATE UNIQUE INDEX payment_orders_provider_payment_intent_uq
    ON payment_orders (payment_provider, provider_payment_intent_no)
    WHERE provider_payment_intent_no IS NOT NULL;
CREATE UNIQUE INDEX payment_orders_provider_charge_uq
    ON payment_orders (payment_provider, provider_charge_no)
    WHERE provider_charge_no IS NOT NULL;

ALTER TABLE payment_orders
    DROP CONSTRAINT payment_orders_terminal_fact_check,
    ADD CONSTRAINT payment_orders_terminal_fact_check
        CHECK (
            (status NOT IN ('paid', 'refund_pending', 'refunded')
                OR (paid_at IS NOT NULL AND provider_trade_no IS NOT NULL
                    AND wallet_id IS NOT NULL AND recharge_ledger_id IS NOT NULL))
            AND (status <> 'refunded' OR (
                refunded_at IS NOT NULL
                AND ((refund_ledger_id IS NOT NULL) <> (external_refunded_amount_minor = amount_minor))
            ))
            AND (status <> 'closed' OR closed_at IS NOT NULL)
        );

ALTER TABLE payment_events
    ADD COLUMN provider_payment_intent_no text,
    ADD COLUMN provider_charge_no text,
    ADD COLUMN provider_dispute_no text,
    ADD CONSTRAINT payment_events_provider_object_format_check CHECK (
        (provider_payment_intent_no IS NULL OR (
            provider_payment_intent_no = btrim(provider_payment_intent_no)
            AND length(provider_payment_intent_no) BETWEEN 1 AND 160
        ))
        AND (provider_charge_no IS NULL OR (
            provider_charge_no = btrim(provider_charge_no)
            AND length(provider_charge_no) BETWEEN 1 AND 160
        ))
        AND (provider_dispute_no IS NULL OR (
            provider_dispute_no = btrim(provider_dispute_no)
            AND length(provider_dispute_no) BETWEEN 1 AND 160
        ))
    );

ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_identity_format_check,
    ADD CONSTRAINT payment_events_identity_format_check
        CHECK (
            provider_event_id = btrim(provider_event_id)
            AND length(provider_event_id) BETWEEN 1 AND 160
            AND event_type IN (
                'pending', 'paid', 'failed', 'expired', 'refunded',
                'refund_failed', 'closed', 'dispute_opened', 'dispute_won',
                'dispute_lost', 'unverified'
            )
            AND (provider_trade_no IS NULL OR (
                provider_trade_no = btrim(provider_trade_no)
                AND length(provider_trade_no) BETWEEN 1 AND 160
            ))
            AND (provider_refund_no IS NULL OR (
                provider_refund_no = btrim(provider_refund_no)
                AND length(provider_refund_no) BETWEEN 1 AND 160
            ))
            AND (merchant_id IS NULL OR (
                merchant_id = btrim(merchant_id)
                AND length(merchant_id) BETWEEN 1 AND 160
            ))
            AND (error_class IS NULL OR (
                error_class = btrim(error_class)
                AND length(error_class) BETWEEN 1 AND 64
            ))
        );

CREATE TABLE payment_external_refunds (
    id uuid PRIMARY KEY,
    payment_provider text NOT NULL,
    provider_refund_no text NOT NULL,
    payment_order_id uuid NOT NULL REFERENCES payment_orders(id),
    wallet_liability_id uuid NOT NULL UNIQUE REFERENCES wallet_liabilities(id),
    provider_charge_no text,
    provider_payment_intent_no text,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency char(3) NOT NULL,
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_provider, provider_refund_no),
    CHECK (currency = upper(currency) AND currency ~ '^[A-Z]{3}$'),
    CHECK (provider_refund_no = btrim(provider_refund_no) AND length(provider_refund_no) BETWEEN 1 AND 160),
    CHECK (provider_charge_no IS NULL OR (
        provider_charge_no = btrim(provider_charge_no) AND length(provider_charge_no) BETWEEN 1 AND 160
    )),
    CHECK (provider_payment_intent_no IS NULL OR (
        provider_payment_intent_no = btrim(provider_payment_intent_no)
        AND length(provider_payment_intent_no) BETWEEN 1 AND 160
    ))
);
CREATE INDEX payment_external_refunds_order_created_idx
    ON payment_external_refunds (payment_order_id, created_at DESC, id DESC);

-- +goose StatementBegin
CREATE FUNCTION bablo_reject_payment_external_refund_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'payment external refunds are immutable' USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_external_refunds_immutable
    BEFORE UPDATE OR DELETE ON payment_external_refunds
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_payment_external_refund_mutation();

CREATE TABLE payment_disputes (
    id uuid PRIMARY KEY,
    payment_provider text NOT NULL,
    provider_dispute_no text NOT NULL,
    payment_order_id uuid NOT NULL REFERENCES payment_orders(id),
    wallet_liability_id uuid NOT NULL UNIQUE REFERENCES wallet_liabilities(id),
    provider_charge_no text NOT NULL,
    provider_payment_intent_no text,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency char(3) NOT NULL,
    status text NOT NULL CHECK (status IN ('open', 'won', 'lost')),
    opened_at timestamptz NOT NULL,
    resolved_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_provider, provider_dispute_no),
    CHECK (currency = upper(currency) AND currency ~ '^[A-Z]{3}$'),
    CHECK (provider_dispute_no = btrim(provider_dispute_no) AND length(provider_dispute_no) BETWEEN 1 AND 160),
    CHECK (provider_charge_no = btrim(provider_charge_no) AND length(provider_charge_no) BETWEEN 1 AND 160),
    CHECK (provider_payment_intent_no IS NULL OR (
        provider_payment_intent_no = btrim(provider_payment_intent_no)
        AND length(provider_payment_intent_no) BETWEEN 1 AND 160
    )),
    CHECK ((status = 'open') = (resolved_at IS NULL)),
    CHECK (resolved_at IS NULL OR resolved_at >= opened_at)
);
CREATE INDEX payment_disputes_order_created_idx
    ON payment_disputes (payment_order_id, created_at DESC, id DESC);
CREATE INDEX payment_disputes_status_created_idx
    ON payment_disputes (status, created_at, id);

-- +goose StatementBegin
CREATE FUNCTION bablo_validate_payment_dispute_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment disputes cannot be deleted' USING ERRCODE = '55000';
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.payment_provider <> OLD.payment_provider
        OR NEW.provider_dispute_no <> OLD.provider_dispute_no
        OR NEW.payment_order_id <> OLD.payment_order_id
        OR NEW.wallet_liability_id <> OLD.wallet_liability_id
        OR NEW.provider_charge_no <> OLD.provider_charge_no
        OR NEW.provider_payment_intent_no IS DISTINCT FROM OLD.provider_payment_intent_no
        OR NEW.amount_minor <> OLD.amount_minor
        OR NEW.currency <> OLD.currency
        OR NEW.opened_at <> OLD.opened_at
        OR NEW.created_at <> OLD.created_at
        OR OLD.status <> 'open'
        OR NEW.status NOT IN ('won', 'lost')
    THEN
        RAISE EXCEPTION 'invalid payment dispute mutation' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_disputes_mutation_guard
    BEFORE UPDATE OR DELETE ON payment_disputes
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_payment_dispute_mutation();

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
    IF NEW.external_refunded_amount_minor <> COALESCE((
        SELECT sum(external_refund.amount_minor)
        FROM payment_external_refunds external_refund
        JOIN wallet_liabilities liability ON liability.id = external_refund.wallet_liability_id
        WHERE external_refund.payment_order_id = NEW.id
          AND external_refund.payment_provider = NEW.payment_provider
          AND external_refund.currency = NEW.currency
          AND liability.wallet_id = NEW.wallet_id
          AND liability.liability_type = 'payment_refund'
          AND liability.reference_type = 'payment_refund'
          AND liability.principal_amount_minor = external_refund.amount_minor
          AND liability.currency = NEW.currency
    ), 0) THEN
        RAISE EXCEPTION 'payment external refund total does not match order'
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
        refund_ledger_id, external_refunded_amount_minor, status, user_id, currency, amount_minor
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
    IF OLD.provider_trade_no IS NOT NULL AND NEW.provider_trade_no IS DISTINCT FROM OLD.provider_trade_no THEN
        RAISE EXCEPTION 'payment provider trade number is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.provider_refund_no IS NOT NULL AND NEW.provider_refund_no IS DISTINCT FROM OLD.provider_refund_no THEN
        RAISE EXCEPTION 'payment provider refund number is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.provider_payment_intent_no IS NOT NULL AND NEW.provider_payment_intent_no IS DISTINCT FROM OLD.provider_payment_intent_no THEN
        RAISE EXCEPTION 'payment provider intent number is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.provider_charge_no IS NOT NULL AND NEW.provider_charge_no IS DISTINCT FROM OLD.provider_charge_no THEN
        RAISE EXCEPTION 'payment provider charge number is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.paid_at IS NOT NULL AND NEW.paid_at IS DISTINCT FROM OLD.paid_at THEN
        RAISE EXCEPTION 'payment paid timestamp is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.refunded_at IS NOT NULL AND NEW.refunded_at IS DISTINCT FROM OLD.refunded_at THEN
        RAISE EXCEPTION 'payment refunded timestamp is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.closed_at IS NOT NULL AND NEW.closed_at IS DISTINCT FROM OLD.closed_at THEN
        RAISE EXCEPTION 'payment closed timestamp is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.wallet_id IS NOT NULL AND NEW.wallet_id IS DISTINCT FROM OLD.wallet_id THEN
        RAISE EXCEPTION 'payment wallet is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.recharge_ledger_id IS NOT NULL AND NEW.recharge_ledger_id IS DISTINCT FROM OLD.recharge_ledger_id THEN
        RAISE EXCEPTION 'payment recharge ledger is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.refund_hold_ledger_id IS NOT NULL AND NEW.refund_hold_ledger_id IS DISTINCT FROM OLD.refund_hold_ledger_id THEN
        RAISE EXCEPTION 'payment refund hold ledger is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.refund_ledger_id IS NOT NULL AND NEW.refund_ledger_id IS DISTINCT FROM OLD.refund_ledger_id THEN
        RAISE EXCEPTION 'payment refund ledger is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF NEW.external_refunded_amount_minor < OLD.external_refunded_amount_minor THEN
        RAISE EXCEPTION 'payment external refund total cannot decrease' USING ERRCODE = '55000';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
        (OLD.status = 'created' AND NEW.status IN ('pending', 'paid', 'failed', 'expired', 'closed'))
        OR (OLD.status = 'pending' AND NEW.status IN ('paid', 'failed', 'expired', 'closed'))
        OR (OLD.status = 'paid' AND NEW.status IN ('refund_pending', 'refunded'))
        OR (OLD.status = 'refund_pending' AND NEW.status IN ('paid', 'refunded'))
        OR (OLD.status IN ('failed', 'expired') AND NEW.status IN ('pending', 'paid', 'closed'))
    ) THEN
        RAISE EXCEPTION 'invalid payment order transition % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'payment order updated_at cannot move backwards' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
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
    IF OLD.provider_trade_no IS NOT NULL AND NEW.provider_trade_no IS DISTINCT FROM OLD.provider_trade_no THEN
        RAISE EXCEPTION 'payment provider trade number is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.provider_refund_no IS NOT NULL AND NEW.provider_refund_no IS DISTINCT FROM OLD.provider_refund_no THEN
        RAISE EXCEPTION 'payment provider refund number is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.paid_at IS NOT NULL AND NEW.paid_at IS DISTINCT FROM OLD.paid_at THEN
        RAISE EXCEPTION 'payment paid timestamp is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.refunded_at IS NOT NULL AND NEW.refunded_at IS DISTINCT FROM OLD.refunded_at THEN
        RAISE EXCEPTION 'payment refunded timestamp is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.closed_at IS NOT NULL AND NEW.closed_at IS DISTINCT FROM OLD.closed_at THEN
        RAISE EXCEPTION 'payment closed timestamp is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.wallet_id IS NOT NULL AND NEW.wallet_id IS DISTINCT FROM OLD.wallet_id THEN
        RAISE EXCEPTION 'payment wallet is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.recharge_ledger_id IS NOT NULL AND NEW.recharge_ledger_id IS DISTINCT FROM OLD.recharge_ledger_id THEN
        RAISE EXCEPTION 'payment recharge ledger is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.refund_hold_ledger_id IS NOT NULL AND NEW.refund_hold_ledger_id IS DISTINCT FROM OLD.refund_hold_ledger_id THEN
        RAISE EXCEPTION 'payment refund hold ledger is immutable once assigned' USING ERRCODE = '55000';
    END IF;
    IF OLD.refund_ledger_id IS NOT NULL AND NEW.refund_ledger_id IS DISTINCT FROM OLD.refund_ledger_id THEN
        RAISE EXCEPTION 'payment refund ledger is immutable once assigned' USING ERRCODE = '55000';
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
        RAISE EXCEPTION 'payment order updated_at cannot move backwards' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER payment_orders_ledger_guard ON payment_orders;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_payment_order_ledger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.wallet_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallets WHERE id = NEW.wallet_id AND user_id = NEW.user_id AND currency = NEW.currency
    ) THEN
        RAISE EXCEPTION 'payment wallet does not match order owner and currency' USING ERRCODE = '23514';
    END IF;
    IF NEW.recharge_ledger_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger WHERE id = NEW.recharge_ledger_id AND wallet_id = NEW.wallet_id
          AND entry_type = 'recharge' AND amount_minor = NEW.amount_minor AND currency = NEW.currency
          AND reference_type = 'payment_order' AND reference_id = NEW.id::text
    ) THEN
        RAISE EXCEPTION 'payment recharge ledger does not match order' USING ERRCODE = '23514';
    END IF;
    IF NEW.refund_hold_ledger_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger WHERE id = NEW.refund_hold_ledger_id AND wallet_id = NEW.wallet_id
          AND entry_type = 'payment_refund_hold' AND amount_minor = NEW.amount_minor AND currency = NEW.currency
          AND reference_type = 'payment_order' AND reference_id = NEW.id::text
    ) THEN
        RAISE EXCEPTION 'payment refund hold ledger does not match order' USING ERRCODE = '23514';
    END IF;
    IF NEW.refund_ledger_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger WHERE id = NEW.refund_ledger_id AND wallet_id = NEW.wallet_id
          AND entry_type = 'payment_reversal' AND amount_minor = -NEW.amount_minor AND currency = NEW.currency
          AND reference_type = 'payment_order' AND reference_id = NEW.id::text
    ) THEN
        RAISE EXCEPTION 'payment refund ledger does not match order' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'refund_pending' AND NEW.refund_hold_ledger_id IS NULL THEN
        RAISE EXCEPTION 'refund-pending payment order requires a refund hold ledger' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER payment_orders_ledger_guard
    BEFORE INSERT OR UPDATE OF wallet_id, recharge_ledger_id, refund_hold_ledger_id,
        refund_ledger_id, status, user_id, currency, amount_minor
    ON payment_orders
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_payment_order_ledger();

DROP TRIGGER payment_disputes_mutation_guard ON payment_disputes;
DROP FUNCTION bablo_validate_payment_dispute_mutation();
DROP TABLE payment_disputes;

DROP TRIGGER payment_external_refunds_immutable ON payment_external_refunds;
DROP FUNCTION bablo_reject_payment_external_refund_mutation();
DROP TABLE payment_external_refunds;

ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_identity_format_check,
    ADD CONSTRAINT payment_events_identity_format_check
        CHECK (
            provider_event_id = btrim(provider_event_id)
            AND length(provider_event_id) BETWEEN 1 AND 160
            AND event_type IN (
                'pending', 'paid', 'failed', 'expired',
                'refunded', 'refund_failed', 'closed', 'unverified'
            )
            AND (provider_trade_no IS NULL OR (
                provider_trade_no = btrim(provider_trade_no)
                AND length(provider_trade_no) BETWEEN 1 AND 160
            ))
            AND (provider_refund_no IS NULL OR (
                provider_refund_no = btrim(provider_refund_no)
                AND length(provider_refund_no) BETWEEN 1 AND 160
            ))
            AND (merchant_id IS NULL OR (
                merchant_id = btrim(merchant_id)
                AND length(merchant_id) BETWEEN 1 AND 160
            ))
            AND (error_class IS NULL OR (
                error_class = btrim(error_class)
                AND length(error_class) BETWEEN 1 AND 64
            ))
        );

ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_provider_object_format_check,
    DROP COLUMN provider_payment_intent_no,
    DROP COLUMN provider_charge_no,
    DROP COLUMN provider_dispute_no;

DROP INDEX payment_orders_provider_payment_intent_uq;
DROP INDEX payment_orders_provider_charge_uq;
ALTER TABLE payment_orders
    DROP CONSTRAINT payment_orders_terminal_fact_check,
    DROP CONSTRAINT payment_orders_provider_payment_intent_format_check,
    DROP CONSTRAINT payment_orders_provider_charge_format_check,
    DROP COLUMN external_refunded_amount_minor,
    DROP COLUMN provider_payment_intent_no,
    DROP COLUMN provider_charge_no,
    ADD CONSTRAINT payment_orders_terminal_fact_check
        CHECK (
            (status NOT IN ('paid', 'refund_pending', 'refunded')
                OR (paid_at IS NOT NULL AND provider_trade_no IS NOT NULL
                    AND wallet_id IS NOT NULL AND recharge_ledger_id IS NOT NULL))
            AND (status <> 'refunded' OR (refunded_at IS NOT NULL AND refund_ledger_id IS NOT NULL))
            AND (status <> 'closed' OR closed_at IS NOT NULL)
        );
