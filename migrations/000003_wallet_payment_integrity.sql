-- +goose Up

ALTER TABLE price_versions
    ADD CONSTRAINT price_versions_currency_format CHECK (currency ~ '^[A-Z]{3}$');
ALTER TABLE usage_events
    ADD CONSTRAINT usage_events_currency_format CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$');
ALTER TABLE wallets
    ADD CONSTRAINT wallets_currency_format CHECK (currency ~ '^[A-Z]{3}$');
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_currency_format CHECK (currency ~ '^[A-Z]{3}$');
ALTER TABLE payment_orders
    ADD CONSTRAINT payment_orders_currency_format CHECK (currency ~ '^[A-Z]{3}$');
ALTER TABLE payment_events
    ADD CONSTRAINT payment_events_currency_format CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$');

ALTER TABLE usage_events
    ADD COLUMN wallet_id uuid REFERENCES wallets(id);
CREATE INDEX usage_events_wallet_created_idx ON usage_events (wallet_id, created_at DESC);

CREATE TABLE payment_event_processing (
    payment_event_id uuid PRIMARY KEY REFERENCES payment_events(id),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'processed', 'rejected', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    claimed_at timestamptz,
    processed_at timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error_class text,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_event_processing_claim_idx ON payment_event_processing (status, next_attempt_at);

-- +goose Down

DROP TABLE IF EXISTS payment_event_processing;
DROP INDEX IF EXISTS usage_events_wallet_created_idx;
ALTER TABLE usage_events DROP COLUMN IF EXISTS wallet_id;
ALTER TABLE payment_events DROP CONSTRAINT IF EXISTS payment_events_currency_format;
ALTER TABLE payment_orders DROP CONSTRAINT IF EXISTS payment_orders_currency_format;
ALTER TABLE wallet_ledger DROP CONSTRAINT IF EXISTS wallet_ledger_currency_format;
ALTER TABLE wallets DROP CONSTRAINT IF EXISTS wallets_currency_format;
ALTER TABLE usage_events DROP CONSTRAINT IF EXISTS usage_events_currency_format;
ALTER TABLE price_versions DROP CONSTRAINT IF EXISTS price_versions_currency_format;
