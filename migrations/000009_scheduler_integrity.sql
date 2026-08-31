-- +goose Up

ALTER TABLE credentials
    ADD COLUMN max_concurrency integer NOT NULL DEFAULT 1,
    ADD CONSTRAINT credentials_max_concurrency_ck
        CHECK (max_concurrency BETWEEN 1 AND 10000);

ALTER TABLE scheduler_decisions
    ADD COLUMN route_version_id uuid REFERENCES route_versions(id),
    ADD COLUMN selected_provider_id uuid REFERENCES providers(id),
    ADD COLUMN selected_credential_id uuid REFERENCES credentials(id),
    ADD CONSTRAINT scheduler_decisions_request_id_ck
        CHECK (request_id = btrim(request_id) AND length(request_id) BETWEEN 1 AND 128) NOT VALID,
    ADD CONSTRAINT scheduler_decisions_strategy_version_ck
        CHECK (strategy_version = btrim(strategy_version) AND length(strategy_version) BETWEEN 1 AND 64) NOT VALID,
    ADD CONSTRAINT scheduler_decisions_candidates_array_ck
        CHECK (jsonb_typeof(candidates) = 'array') NOT VALID,
    ADD CONSTRAINT scheduler_decisions_fallback_array_ck
        CHECK (jsonb_typeof(fallback_chain) = 'array') NOT VALID,
    ADD CONSTRAINT scheduler_decisions_selected_pair_ck
        CHECK (
            (selected_target_id IS NULL AND selected_provider_id IS NULL AND selected_credential_id IS NULL)
            OR
            (selected_target_id IS NOT NULL AND selected_provider_id IS NULL AND selected_credential_id IS NULL)
            OR
            (selected_target_id IS NOT NULL AND selected_provider_id IS NOT NULL AND selected_credential_id IS NOT NULL)
        ) NOT VALID;

ALTER TABLE scheduler_decisions VALIDATE CONSTRAINT scheduler_decisions_request_id_ck;
ALTER TABLE scheduler_decisions VALIDATE CONSTRAINT scheduler_decisions_strategy_version_ck;
ALTER TABLE scheduler_decisions VALIDATE CONSTRAINT scheduler_decisions_candidates_array_ck;
ALTER TABLE scheduler_decisions VALIDATE CONSTRAINT scheduler_decisions_fallback_array_ck;
ALTER TABLE scheduler_decisions VALIDATE CONSTRAINT scheduler_decisions_selected_pair_ck;

CREATE INDEX scheduler_decisions_credential_created_idx
    ON scheduler_decisions (selected_credential_id, created_at DESC)
    WHERE selected_credential_id IS NOT NULL;
CREATE INDEX scheduler_decisions_route_created_idx
    ON scheduler_decisions (route_version_id, created_at DESC)
    WHERE route_version_id IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_scheduler_selection()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.selected_target_id IS NULL THEN
        RETURN NEW;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM route_targets rt
        JOIN route_versions rv ON rv.id = rt.route_version_id
        JOIN provider_models pm ON pm.id = rt.provider_model_id
        JOIN credentials c ON c.id = NEW.selected_credential_id
        JOIN pool_members member
          ON member.pool_id = rt.credential_pool_id
         AND member.credential_id = c.id
        WHERE rt.id = NEW.selected_target_id
          AND rv.id = NEW.route_version_id
          AND pm.provider_id = NEW.selected_provider_id
          AND c.provider_id = NEW.selected_provider_id
    ) THEN
        RAISE EXCEPTION 'scheduler selection must match route target, provider, and pool credential'
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scheduler_decisions_selection_guard
    BEFORE INSERT ON scheduler_decisions
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_scheduler_selection();

-- +goose Down

DROP TRIGGER IF EXISTS scheduler_decisions_selection_guard ON scheduler_decisions;
DROP FUNCTION IF EXISTS bablo_validate_scheduler_selection();
DROP INDEX IF EXISTS scheduler_decisions_route_created_idx;
DROP INDEX IF EXISTS scheduler_decisions_credential_created_idx;
ALTER TABLE scheduler_decisions
    DROP CONSTRAINT IF EXISTS scheduler_decisions_selected_pair_ck,
    DROP CONSTRAINT IF EXISTS scheduler_decisions_fallback_array_ck,
    DROP CONSTRAINT IF EXISTS scheduler_decisions_candidates_array_ck,
    DROP CONSTRAINT IF EXISTS scheduler_decisions_strategy_version_ck,
    DROP CONSTRAINT IF EXISTS scheduler_decisions_request_id_ck,
    DROP COLUMN IF EXISTS selected_credential_id,
    DROP COLUMN IF EXISTS selected_provider_id,
    DROP COLUMN IF EXISTS route_version_id;
ALTER TABLE credentials
    DROP CONSTRAINT IF EXISTS credentials_max_concurrency_ck,
    DROP COLUMN IF EXISTS max_concurrency;
