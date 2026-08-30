-- +goose Up

ALTER TABLE model_routes
    ADD CONSTRAINT model_routes_match_value_ck
        CHECK (match_value = btrim(match_value) AND length(match_value) BETWEEN 1 AND 128) NOT VALID;

ALTER TABLE route_versions
    ADD CONSTRAINT route_versions_snapshot_hash_ck
        CHECK (octet_length(snapshot_hash) = 32) NOT VALID;

ALTER TABLE route_targets
    ADD CONSTRAINT route_targets_metadata_object_ck
        CHECK (jsonb_typeof(metadata) = 'object') NOT VALID;

ALTER TABLE model_routes VALIDATE CONSTRAINT model_routes_match_value_ck;
ALTER TABLE route_versions VALIDATE CONSTRAINT route_versions_snapshot_hash_ck;
ALTER TABLE route_targets VALIDATE CONSTRAINT route_targets_metadata_object_ck;

-- Route versions are append-only snapshots. Closing an active version is the
-- sole permitted mutation: effective_to changes once from NULL to a timestamp.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_route_version_history()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'route version history is immutable'
            USING ERRCODE = '55000';
    END IF;
    IF NEW.route_id IS DISTINCT FROM OLD.route_id
        OR NEW.version_no IS DISTINCT FROM OLD.version_no
        OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.snapshot_hash IS DISTINCT FROM OLD.snapshot_hash
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR OLD.effective_to IS NOT NULL
        OR NEW.effective_to IS NULL
        OR NEW.effective_to < OLD.effective_from
    THEN
        RAISE EXCEPTION 'route version history is immutable'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER route_versions_history_guard
    BEFORE UPDATE OR DELETE ON route_versions
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_route_version_history();

-- Targets are immutable members of their parent snapshot. A new target set is
-- published through a new route version rather than editing this table.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_route_target_history()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'route target history is immutable'
        USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER route_targets_history_guard
    BEFORE UPDATE OR DELETE ON route_targets
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_route_target_history();

-- +goose Down

DROP TRIGGER IF EXISTS route_targets_history_guard ON route_targets;
DROP FUNCTION IF EXISTS bablo_guard_route_target_history();
DROP TRIGGER IF EXISTS route_versions_history_guard ON route_versions;
DROP FUNCTION IF EXISTS bablo_guard_route_version_history();
ALTER TABLE route_targets DROP CONSTRAINT IF EXISTS route_targets_metadata_object_ck;
ALTER TABLE route_versions DROP CONSTRAINT IF EXISTS route_versions_snapshot_hash_ck;
ALTER TABLE model_routes DROP CONSTRAINT IF EXISTS model_routes_match_value_ck;
