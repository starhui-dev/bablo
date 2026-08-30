-- +goose Up

CREATE TABLE model_aliases (
    id uuid PRIMARY KEY,
    model_id uuid NOT NULL REFERENCES models(id),
    alias text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (alias)
);
CREATE UNIQUE INDEX model_aliases_alias_ci_uq ON model_aliases (lower(alias));
CREATE INDEX model_aliases_model_enabled_idx ON model_aliases (model_id, enabled, alias);
CREATE UNIQUE INDEX models_public_model_id_ci_uq ON models (lower(public_model_id));

UPDATE providers
SET commercial_allowed = false, updated_at = now()
WHERE resource_type = 'subscription' AND commercial_allowed;

ALTER TABLE providers
    ADD CONSTRAINT providers_subscription_commercial_ck
        CHECK (resource_type <> 'subscription' OR NOT commercial_allowed) NOT VALID;

ALTER TABLE provider_models
    ADD COLUMN review_status text NOT NULL DEFAULT 'approved'
        CHECK (review_status IN ('pending', 'approved', 'rejected')),
    ADD COLUMN discovery_status text NOT NULL DEFAULT 'unknown'
        CHECK (discovery_status IN ('unknown', 'present', 'missing')),
    ADD COLUMN discovered_at timestamptz,
    ADD COLUMN last_seen_at timestamptz;

-- Existing unlinked rows are discovery candidates, not routable business targets.
UPDATE provider_models
SET review_status = 'pending', enabled = false, updated_at = now()
WHERE model_id IS NULL AND enabled;

ALTER TABLE providers VALIDATE CONSTRAINT providers_subscription_commercial_ck;

ALTER TABLE provider_models
    ADD CONSTRAINT provider_models_enabled_reviewed_ck
        CHECK (NOT enabled OR (review_status = 'approved' AND model_id IS NOT NULL)) NOT VALID,
    ADD CONSTRAINT provider_models_discovery_time_ck
        CHECK (last_seen_at IS NULL OR discovered_at IS NULL OR last_seen_at >= discovered_at) NOT VALID;

ALTER TABLE provider_models VALIDATE CONSTRAINT provider_models_enabled_reviewed_ck;
ALTER TABLE provider_models VALIDATE CONSTRAINT provider_models_discovery_time_ck;

CREATE INDEX provider_models_review_idx
    ON provider_models (provider_id, review_status, discovery_status, upstream_model_id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_model_identifier()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    identifier text;
BEGIN
    identifier := lower(NEW.public_model_id);
    PERFORM pg_advisory_xact_lock(hashtextextended(identifier, 0));
    IF EXISTS (SELECT 1 FROM model_aliases WHERE lower(alias) = identifier AND model_id <> NEW.id) THEN
        RAISE EXCEPTION 'public model id conflicts with an existing alias'
            USING ERRCODE = '23505';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER models_identifier_guard
    BEFORE INSERT OR UPDATE OF public_model_id ON models
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_model_identifier();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_model_alias()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    identifier text;
BEGIN
    identifier := lower(NEW.alias);
    PERFORM pg_advisory_xact_lock(hashtextextended(identifier, 0));
    IF EXISTS (SELECT 1 FROM models WHERE lower(public_model_id) = identifier) THEN
        RAISE EXCEPTION 'model alias conflicts with an existing public model id'
            USING ERRCODE = '23505';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER model_aliases_identifier_guard
    BEFORE INSERT OR UPDATE OF alias ON model_aliases
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_model_alias();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_price_entry_scope()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM price_versions
        WHERE id = NEW.price_version_id
          AND scope = NEW.pricing_scope
    ) THEN
        RAISE EXCEPTION 'price entry scope must match its price version scope'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER model_prices_scope_guard
    BEFORE INSERT OR UPDATE OF price_version_id, pricing_scope ON model_prices
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_price_entry_scope();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_published_price_entry()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    version_status text;
    version_id uuid;
BEGIN
    version_id := COALESCE(NEW.price_version_id, OLD.price_version_id);
    SELECT status INTO version_status FROM price_versions WHERE id = version_id;
    IF version_status IN ('active', 'retired') THEN
        RAISE EXCEPTION 'published price entries are immutable'
            USING ERRCODE = '55000';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER model_prices_published_guard
    BEFORE INSERT OR UPDATE OR DELETE ON model_prices
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_published_price_entry();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_guard_published_price_version()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.status IN ('active', 'retired') THEN
        IF NEW.scope <> OLD.scope
            OR NEW.version_no <> OLD.version_no
            OR NEW.currency <> OLD.currency
            OR NEW.effective_from <> OLD.effective_from
            OR NEW.created_by IS DISTINCT FROM OLD.created_by
            OR NEW.status = 'draft'
            OR (OLD.status = 'retired' AND NEW.status <> 'retired')
        THEN
            RAISE EXCEPTION 'published price version identity is immutable'
                USING ERRCODE = '55000';
        END IF;
        IF OLD.effective_to IS NOT NULL AND NEW.effective_to IS DISTINCT FROM OLD.effective_to THEN
            RAISE EXCEPTION 'published price version end cannot be changed twice'
                USING ERRCODE = '55000';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER price_versions_published_guard
    BEFORE UPDATE ON price_versions
    FOR EACH ROW EXECUTE FUNCTION bablo_guard_published_price_version();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bablo_validate_price_version_interval()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status NOT IN ('active', 'retired') THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('price:' || NEW.scope, 0));
    IF EXISTS (
        SELECT 1
        FROM price_versions existing
        WHERE existing.scope = NEW.scope
          AND existing.status IN ('active', 'retired')
          AND existing.id <> NEW.id
          AND tstzrange(existing.effective_from, existing.effective_to, '[)')
              && tstzrange(NEW.effective_from, NEW.effective_to, '[)')
    ) THEN
        RAISE EXCEPTION 'published price version intervals cannot overlap for scope %', NEW.scope
            USING ERRCODE = '23505';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER price_versions_interval_guard
    BEFORE INSERT OR UPDATE OF scope, effective_from, effective_to, status ON price_versions
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_price_version_interval();

-- +goose Down

DROP TRIGGER IF EXISTS price_versions_interval_guard ON price_versions;
DROP FUNCTION IF EXISTS bablo_validate_price_version_interval();
DROP TRIGGER IF EXISTS price_versions_published_guard ON price_versions;
DROP FUNCTION IF EXISTS bablo_guard_published_price_version();
DROP TRIGGER IF EXISTS model_prices_published_guard ON model_prices;
ALTER TABLE providers DROP CONSTRAINT IF EXISTS providers_subscription_commercial_ck;
DROP FUNCTION IF EXISTS bablo_guard_published_price_entry();
DROP TRIGGER IF EXISTS model_prices_scope_guard ON model_prices;
DROP FUNCTION IF EXISTS bablo_validate_price_entry_scope();
DROP TRIGGER IF EXISTS model_aliases_identifier_guard ON model_aliases;
DROP FUNCTION IF EXISTS bablo_validate_model_alias();
DROP TRIGGER IF EXISTS models_identifier_guard ON models;
DROP FUNCTION IF EXISTS bablo_validate_model_identifier();
DROP INDEX IF EXISTS provider_models_review_idx;
ALTER TABLE provider_models
    DROP CONSTRAINT IF EXISTS provider_models_discovery_time_ck,
    DROP CONSTRAINT IF EXISTS provider_models_enabled_reviewed_ck,
    DROP COLUMN IF EXISTS last_seen_at,
    DROP COLUMN IF EXISTS discovered_at,
    DROP COLUMN IF EXISTS discovery_status,
    DROP COLUMN IF EXISTS review_status;
DROP INDEX IF EXISTS models_public_model_id_ci_uq;
DROP TABLE IF EXISTS model_aliases;
