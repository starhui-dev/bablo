-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_reject_fact_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only; write a compensating event instead', TG_TABLE_NAME
        USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER usage_events_append_only
    BEFORE UPDATE OR DELETE ON usage_events
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

CREATE TRIGGER usage_reconciliations_append_only
    BEFORE UPDATE OR DELETE ON usage_reconciliations
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

CREATE TRIGGER wallet_ledger_append_only
    BEFORE UPDATE OR DELETE ON wallet_ledger
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

CREATE TRIGGER payment_events_append_only
    BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

CREATE TRIGGER scheduler_decisions_append_only
    BEFORE UPDATE OR DELETE ON scheduler_decisions
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

CREATE TRIGGER audit_logs_append_only
    BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_fact_mutation();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_active_route_version()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.active_version_id IS NOT NULL AND NOT EXISTS (
        SELECT 1
        FROM route_versions
        WHERE id = NEW.active_version_id
          AND route_id = NEW.id
          AND effective_to IS NULL
    ) THEN
        RAISE EXCEPTION 'active route version must be current and belong to route %', NEW.id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER model_routes_active_version_guard
    BEFORE INSERT OR UPDATE OF active_version_id ON model_routes
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_active_route_version();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_pool_provider()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM credential_pools p
        JOIN credentials c ON c.id = NEW.credential_id
        WHERE p.id = NEW.pool_id
          AND p.provider_id = c.provider_id
    ) THEN
        RAISE EXCEPTION 'credential and pool must belong to the same provider'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER pool_members_provider_guard
    BEFORE INSERT OR UPDATE OF pool_id, credential_id ON pool_members
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_pool_provider();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_route_target_provider()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM provider_models pm
        JOIN credential_pools p ON p.id = NEW.credential_pool_id
        WHERE pm.id = NEW.provider_model_id
          AND pm.provider_id = p.provider_id
    ) THEN
        RAISE EXCEPTION 'provider model and credential pool must belong to the same provider'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER route_targets_provider_guard
    BEFORE INSERT OR UPDATE OF provider_model_id, credential_pool_id ON route_targets
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_route_target_provider();

-- +goose Down

DROP TRIGGER IF EXISTS route_targets_provider_guard ON route_targets;
DROP FUNCTION IF EXISTS bablo_validate_route_target_provider();
DROP TRIGGER IF EXISTS pool_members_provider_guard ON pool_members;
DROP FUNCTION IF EXISTS bablo_validate_pool_provider();
DROP TRIGGER IF EXISTS model_routes_active_version_guard ON model_routes;
DROP FUNCTION IF EXISTS bablo_validate_active_route_version();
DROP TRIGGER IF EXISTS audit_logs_append_only ON audit_logs;
DROP TRIGGER IF EXISTS scheduler_decisions_append_only ON scheduler_decisions;
DROP TRIGGER IF EXISTS payment_events_append_only ON payment_events;
DROP TRIGGER IF EXISTS wallet_ledger_append_only ON wallet_ledger;
DROP TRIGGER IF EXISTS usage_reconciliations_append_only ON usage_reconciliations;
DROP TRIGGER IF EXISTS usage_events_append_only ON usage_events;
DROP FUNCTION IF EXISTS bablo_reject_fact_mutation();
