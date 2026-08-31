-- +goose Up

ALTER TABLE outbox_events
    ADD COLUMN claimed_by text,
    ADD CONSTRAINT outbox_events_claimed_by_ck
        CHECK (claimed_by IS NULL OR (claimed_by = btrim(claimed_by) AND length(claimed_by) BETWEEN 1 AND 128)) NOT VALID;

ALTER TABLE usage_events
    ADD COLUMN started_at timestamptz,
    ADD COLUMN finished_at timestamptz;

-- Existing fact rows need a one-time timestamp backfill. The append-only
-- trigger is restored before any subsequent migration statement can write facts.
DROP TRIGGER usage_events_append_only ON usage_events;

UPDATE usage_events AS usage
SET started_at = COALESCE(request.started_at, usage.created_at),
    finished_at = GREATEST(
        COALESCE(request.finished_at, usage.created_at),
        COALESCE(request.started_at, usage.created_at)
    )
FROM request_records AS request
WHERE usage.request_record_id = request.id;

UPDATE usage_events
SET started_at = COALESCE(started_at, created_at),
    finished_at = COALESCE(finished_at, created_at);

CREATE TRIGGER usage_events_append_only
    BEFORE UPDATE OR DELETE ON usage_events
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_usage_request_link()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.request_record_id IS NOT NULL AND NOT EXISTS (
        SELECT 1
        FROM request_records
        WHERE id = NEW.request_record_id
          AND request_id = NEW.request_id
    ) THEN
        RAISE EXCEPTION 'usage event request link does not match request record'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER usage_events_request_link_guard
    BEFORE INSERT ON usage_events
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_usage_request_link();

ALTER TABLE usage_events
    ALTER COLUMN started_at SET DEFAULT now(),
    ALTER COLUMN finished_at SET DEFAULT now(),
    ALTER COLUMN started_at SET NOT NULL,
    ALTER COLUMN finished_at SET NOT NULL,
    ADD CONSTRAINT usage_events_time_order_ck
        CHECK (finished_at >= started_at) NOT VALID,
    ADD CONSTRAINT usage_events_request_id_ck
        CHECK (request_id = btrim(request_id) AND length(request_id) BETWEEN 1 AND 128) NOT VALID,
    ADD CONSTRAINT usage_events_settlement_key_ck
        CHECK (settlement_key = btrim(settlement_key) AND length(settlement_key) BETWEEN 1 AND 160) NOT VALID;

ALTER TABLE usage_reconciliations
    ADD CONSTRAINT usage_reconciliations_source_ck
        CHECK (
            source = btrim(source) AND length(source) BETWEEN 1 AND 64 AND
            source_event_key = btrim(source_event_key) AND length(source_event_key) BETWEEN 1 AND 256
        ) NOT VALID;

ALTER TABLE outbox_events VALIDATE CONSTRAINT outbox_events_claimed_by_ck;
ALTER TABLE usage_events VALIDATE CONSTRAINT usage_events_time_order_ck;
ALTER TABLE usage_events VALIDATE CONSTRAINT usage_events_request_id_ck;
ALTER TABLE usage_events VALIDATE CONSTRAINT usage_events_settlement_key_ck;
ALTER TABLE usage_reconciliations VALIDATE CONSTRAINT usage_reconciliations_source_ck;

CREATE UNIQUE INDEX usage_events_request_id_uq ON usage_events (request_id);

-- +goose Down

DROP INDEX IF EXISTS usage_events_request_id_uq;
DROP TRIGGER IF EXISTS usage_events_request_link_guard ON usage_events;
DROP FUNCTION IF EXISTS bablo_validate_usage_request_link();
ALTER TABLE usage_reconciliations
    DROP CONSTRAINT IF EXISTS usage_reconciliations_source_ck;
ALTER TABLE usage_events
    DROP CONSTRAINT IF EXISTS usage_events_settlement_key_ck,
    DROP CONSTRAINT IF EXISTS usage_events_request_id_ck,
    DROP CONSTRAINT IF EXISTS usage_events_time_order_ck,
    DROP COLUMN IF EXISTS finished_at,
    DROP COLUMN IF EXISTS started_at;
ALTER TABLE outbox_events
    DROP CONSTRAINT IF EXISTS outbox_events_claimed_by_ck,
    DROP COLUMN IF EXISTS claimed_by;
