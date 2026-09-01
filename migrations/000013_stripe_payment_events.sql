-- +goose Up

ALTER TABLE payment_events
    ADD COLUMN provider_refund_no text;

ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_identity_format_check,
    DROP CONSTRAINT payment_events_verified_fact_check;

ALTER TABLE payment_events
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
        ),
    ADD CONSTRAINT payment_events_verified_fact_check
        CHECK (
            (signature_verified
                AND event_type <> 'unverified'
                AND provider_trade_no IS NOT NULL
                AND (event_type NOT IN ('refunded', 'refund_failed') OR provider_refund_no IS NOT NULL)
                AND amount_minor IS NOT NULL AND amount_minor > 0
                AND currency IS NOT NULL
                AND merchant_id IS NOT NULL
                AND occurred_at IS NOT NULL)
            OR (NOT signature_verified
                AND event_type = 'unverified'
                AND order_id IS NULL
                AND provider_trade_no IS NULL
                AND provider_refund_no IS NULL
                AND amount_minor IS NULL
                AND currency IS NULL
                AND merchant_id IS NULL
                AND occurred_at IS NULL)
        );

-- +goose Down

ALTER TABLE payment_events
    DROP CONSTRAINT payment_events_verified_fact_check,
    DROP CONSTRAINT payment_events_identity_format_check;

ALTER TABLE payment_events
    DROP COLUMN provider_refund_no;

ALTER TABLE payment_events
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
