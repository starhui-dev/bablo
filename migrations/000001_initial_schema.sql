-- +goose Up

-- Bablo application IDs are supplied by uuid.NewV7(). Keeping defaults out of the
-- schema prevents a database function from silently generating a different UUID version.

CREATE TABLE users (
    id uuid PRIMARY KEY,
    email_normalized text NOT NULL,
    password_hash text NOT NULL,
    password_params_version text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('invited', 'active', 'suspended', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE UNIQUE INDEX users_email_normalized_uq ON users (lower(email_normalized));
CREATE INDEX users_status_created_idx ON users (status, created_at DESC);

CREATE TABLE roles (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_roles (
    user_id uuid NOT NULL REFERENCES users(id),
    role_id uuid NOT NULL REFERENCES roles(id),
    assigned_by uuid REFERENCES users(id),
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_id)
);
CREATE INDEX user_roles_role_idx ON user_roles (role_id, user_id);

CREATE TABLE user_sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent text NOT NULL DEFAULT '',
    remote_addr inet,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);
CREATE INDEX user_sessions_user_active_idx ON user_sessions (user_id, expires_at DESC) WHERE revoked_at IS NULL;

CREATE TABLE mfa_factors (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    factor_type text NOT NULL CHECK (factor_type IN ('totp', 'webauthn')),
    secret_ciphertext bytea,
    secret_nonce bytea,
    key_version text,
    enabled boolean NOT NULL DEFAULT false,
    confirmed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((secret_ciphertext IS NULL AND secret_nonce IS NULL AND key_version IS NULL)
        OR (secret_ciphertext IS NOT NULL AND secret_nonce IS NOT NULL AND key_version IS NOT NULL))
);
CREATE INDEX mfa_factors_user_enabled_idx ON mfa_factors (user_id, enabled);

CREATE TABLE mfa_recovery_codes (
    id uuid PRIMARY KEY,
    factor_id uuid NOT NULL REFERENCES mfa_factors(id),
    code_hash bytea NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (factor_id, code_hash)
);

CREATE TABLE api_keys (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    name text NOT NULL,
    key_prefix text NOT NULL,
    secret_hash bytea NOT NULL UNIQUE,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked', 'expired')),
    expires_at timestamptz,
    ip_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    rpm_limit bigint CHECK (rpm_limit IS NULL OR rpm_limit > 0),
    tpm_limit bigint CHECK (tpm_limit IS NULL OR tpm_limit > 0),
    daily_budget_minor bigint CHECK (daily_budget_minor IS NULL OR daily_budget_minor >= 0),
    monthly_budget_minor bigint CHECK (monthly_budget_minor IS NULL OR monthly_budget_minor >= 0),
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX api_keys_user_status_idx ON api_keys (user_id, status, created_at DESC);
CREATE INDEX api_keys_prefix_idx ON api_keys (key_prefix);

CREATE TABLE policies (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    default_action text NOT NULL DEFAULT 'deny' CHECK (default_action IN ('allow', 'deny')),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_key_policies (
    api_key_id uuid NOT NULL REFERENCES api_keys(id),
    policy_id uuid NOT NULL REFERENCES policies(id),
    priority integer NOT NULL DEFAULT 0,
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (api_key_id, policy_id)
);
CREATE INDEX api_key_policies_policy_idx ON api_key_policies (policy_id, api_key_id);

CREATE TABLE models (
    id uuid PRIMARY KEY,
    public_model_id text NOT NULL UNIQUE,
    display_name text NOT NULL,
    visibility text NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'private', 'internal')),
    billing_class text NOT NULL DEFAULT 'token' CHECK (billing_class IN ('token', 'request', 'free', 'disabled')),
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE INDEX models_enabled_visibility_idx ON models (enabled, visibility, public_model_id);

CREATE TABLE policy_model_entitlements (
    policy_id uuid NOT NULL REFERENCES policies(id),
    model_id uuid NOT NULL REFERENCES models(id),
    effect text NOT NULL DEFAULT 'allow' CHECK (effect IN ('allow', 'deny')),
    constraints jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (policy_id, model_id)
);
CREATE INDEX entitlements_model_effect_idx ON policy_model_entitlements (model_id, effect);

CREATE TABLE providers (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    display_name text NOT NULL,
    resource_type text NOT NULL CHECK (resource_type IN ('official_api', 'enterprise_api', 'subscription', 'third_party')),
    commercial_allowed boolean NOT NULL DEFAULT false,
    endpoint_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE INDEX providers_resource_enabled_idx ON providers (resource_type, enabled, slug);

CREATE TABLE provider_models (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id),
    model_id uuid REFERENCES models(id),
    upstream_model_id text NOT NULL,
    protocol text NOT NULL CHECK (protocol IN ('openai_chat', 'openai_responses', 'claude_messages', 'gemini', 'custom')),
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, upstream_model_id)
);
CREATE INDEX provider_models_model_enabled_idx ON provider_models (model_id, enabled);

CREATE TABLE credentials (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id),
    external_stable_id text NOT NULL,
    source_kind text NOT NULL CHECK (source_kind IN ('oauth', 'api_key', 'service_account', 'custom')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked', 'error')),
    region text NOT NULL DEFAULT '',
    proxy_ref text NOT NULL DEFAULT '',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    UNIQUE (provider_id, external_stable_id)
);
CREATE INDEX credentials_provider_status_idx ON credentials (provider_id, status, id);

CREATE TABLE credential_secrets (
    id uuid PRIMARY KEY,
    credential_id uuid NOT NULL REFERENCES credentials(id),
    secret_kind text NOT NULL CHECK (secret_kind IN ('oauth_access', 'oauth_refresh', 'api_key', 'service_account', 'custom')),
    version_no bigint NOT NULL CHECK (version_no > 0),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    key_version text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz,
    UNIQUE (credential_id, secret_kind, version_no)
);
CREATE UNIQUE INDEX credential_secrets_active_uq ON credential_secrets (credential_id, secret_kind)
    WHERE rotated_at IS NULL;

CREATE TABLE credential_pools (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    provider_id uuid NOT NULL REFERENCES providers(id),
    enabled boolean NOT NULL DEFAULT true,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credential_pools_provider_enabled_idx ON credential_pools (provider_id, enabled, id);

CREATE TABLE pool_members (
    pool_id uuid NOT NULL REFERENCES credential_pools(id),
    credential_id uuid NOT NULL REFERENCES credentials(id),
    priority integer NOT NULL DEFAULT 0 CHECK (priority >= 0),
    weight integer NOT NULL DEFAULT 1 CHECK (weight > 0),
    enabled boolean NOT NULL DEFAULT true,
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pool_id, credential_id)
);
CREATE INDEX pool_members_credential_idx ON pool_members (credential_id, enabled, pool_id);

CREATE TABLE credential_health (
    credential_id uuid PRIMARY KEY REFERENCES credentials(id),
    last_success_at timestamptz,
    last_error_at timestamptz,
    last_error_class text,
    cooldown_until timestamptz,
    observed_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX credential_health_cooldown_idx ON credential_health (cooldown_until, credential_id);

CREATE TABLE model_routes (
    id uuid PRIMARY KEY,
    model_id uuid NOT NULL REFERENCES models(id),
    match_type text NOT NULL DEFAULT 'exact' CHECK (match_type IN ('exact', 'prefix', 'regex')),
    match_value text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    UNIQUE (match_type, match_value)
);
CREATE INDEX model_routes_model_enabled_idx ON model_routes (model_id, enabled, id);

CREATE TABLE route_versions (
    id uuid PRIMARY KEY,
    route_id uuid NOT NULL REFERENCES model_routes(id),
    version_no bigint NOT NULL CHECK (version_no > 0),
    effective_from timestamptz NOT NULL DEFAULT now(),
    effective_to timestamptz,
    snapshot_hash bytea NOT NULL,
    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (route_id, version_no),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);
CREATE UNIQUE INDEX route_versions_active_uq ON route_versions (route_id) WHERE effective_to IS NULL;

ALTER TABLE model_routes
    ADD COLUMN active_version_id uuid REFERENCES route_versions(id);

CREATE TABLE route_targets (
    id uuid PRIMARY KEY,
    route_version_id uuid NOT NULL REFERENCES route_versions(id),
    target_no integer NOT NULL CHECK (target_no >= 0),
    provider_model_id uuid NOT NULL REFERENCES provider_models(id),
    credential_pool_id uuid NOT NULL REFERENCES credential_pools(id),
    priority integer NOT NULL DEFAULT 0 CHECK (priority >= 0),
    weight integer NOT NULL DEFAULT 1 CHECK (weight > 0),
    commercial_policy text NOT NULL DEFAULT 'inherit' CHECK (commercial_policy IN ('inherit', 'allow', 'deny')),
    enabled boolean NOT NULL DEFAULT true,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (route_version_id, target_no),
    UNIQUE (route_version_id, provider_model_id, credential_pool_id)
);
CREATE INDEX route_targets_selection_idx ON route_targets (route_version_id, enabled, priority, weight, id);

CREATE TABLE quota_snapshots (
    id uuid PRIMARY KEY,
    credential_id uuid NOT NULL REFERENCES credentials(id),
    window_kind text NOT NULL CHECK (window_kind IN ('minute', 'hour', 'day', 'month', 'provider_specific')),
    used_tokens bigint CHECK (used_tokens IS NULL OR used_tokens >= 0),
    remaining_tokens bigint CHECK (remaining_tokens IS NULL OR remaining_tokens >= 0),
    limit_tokens bigint CHECK (limit_tokens IS NULL OR limit_tokens >= 0),
    reset_at timestamptz,
    observed_at timestamptz NOT NULL,
    source text NOT NULL,
    confidence text NOT NULL DEFAULT 'unknown' CHECK (confidence IN ('high', 'medium', 'low', 'unknown')),
    error_class text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX quota_snapshots_credential_window_idx ON quota_snapshots (credential_id, window_kind, observed_at DESC);
CREATE INDEX quota_snapshots_stale_idx ON quota_snapshots (observed_at, credential_id);

CREATE TABLE price_versions (
    id uuid PRIMARY KEY,
    scope text NOT NULL CHECK (scope IN ('global', 'model', 'provider_model')),
    version_no bigint NOT NULL CHECK (version_no > 0),
    currency char(3) NOT NULL,
    effective_from timestamptz NOT NULL,
    effective_to timestamptz,
    status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'retired')),
    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope, version_no),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);
CREATE INDEX price_versions_active_idx ON price_versions (scope, status, effective_from DESC);

CREATE TABLE model_prices (
    id uuid PRIMARY KEY,
    price_version_id uuid NOT NULL REFERENCES price_versions(id),
    pricing_scope text NOT NULL CHECK (pricing_scope IN ('global', 'model', 'provider_model')),
    model_id uuid REFERENCES models(id),
    provider_model_id uuid REFERENCES provider_models(id),
    dimension text NOT NULL CHECK (dimension IN ('input_token', 'output_token', 'cache_read_token', 'cache_write_token', 'reasoning_token', 'request')),
    unit_price numeric(30, 12) NOT NULL CHECK (unit_price >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((pricing_scope = 'global' AND model_id IS NULL AND provider_model_id IS NULL)
        OR (pricing_scope = 'model' AND model_id IS NOT NULL AND provider_model_id IS NULL)
        OR (pricing_scope = 'provider_model' AND model_id IS NULL AND provider_model_id IS NOT NULL))
);
CREATE UNIQUE INDEX model_prices_global_uq ON model_prices (price_version_id, dimension)
    WHERE pricing_scope = 'global';
CREATE UNIQUE INDEX model_prices_model_uq ON model_prices (price_version_id, model_id, dimension)
    WHERE pricing_scope = 'model';
CREATE UNIQUE INDEX model_prices_provider_model_uq ON model_prices (price_version_id, provider_model_id, dimension)
    WHERE pricing_scope = 'provider_model';
CREATE INDEX model_prices_lookup_idx ON model_prices (price_version_id, pricing_scope, dimension);

CREATE TABLE request_records (
    id uuid PRIMARY KEY,
    request_id text NOT NULL UNIQUE,
    user_id uuid REFERENCES users(id),
    api_key_id uuid REFERENCES api_keys(id),
    endpoint text NOT NULL,
    requested_model text NOT NULL,
    stream boolean NOT NULL DEFAULT false,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    terminal_status text CHECK (terminal_status IS NULL OR terminal_status IN ('succeeded', 'failed', 'cancelled', 'reconcile_needed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX request_records_user_started_idx ON request_records (user_id, started_at DESC);
CREATE INDEX request_records_api_key_started_idx ON request_records (api_key_id, started_at DESC);
CREATE INDEX request_records_status_started_idx ON request_records (terminal_status, started_at DESC);

CREATE TABLE request_attempts (
    id uuid PRIMARY KEY,
    request_record_id uuid NOT NULL REFERENCES request_records(id),
    attempt_no integer NOT NULL CHECK (attempt_no >= 0),
    route_version_id uuid REFERENCES route_versions(id),
    provider_id uuid REFERENCES providers(id),
    provider_model_id uuid REFERENCES provider_models(id),
    credential_id uuid REFERENCES credentials(id),
    upstream_status integer CHECK (upstream_status IS NULL OR upstream_status BETWEEN 100 AND 599),
    error_class text,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    latency_ms bigint CHECK (latency_ms IS NULL OR latency_ms >= 0),
    ttft_ms bigint CHECK (ttft_ms IS NULL OR ttft_ms >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (request_record_id, attempt_no),
    CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX request_attempts_route_started_idx ON request_attempts (route_version_id, started_at DESC);
CREATE INDEX request_attempts_credential_started_idx ON request_attempts (credential_id, started_at DESC);

CREATE TABLE usage_events (
    id uuid PRIMARY KEY,
    settlement_key text NOT NULL UNIQUE,
    request_record_id uuid REFERENCES request_records(id),
    request_id text NOT NULL,
    user_id uuid REFERENCES users(id),
    api_key_id uuid REFERENCES api_keys(id),
    requested_model text NOT NULL,
    resolved_model_id uuid REFERENCES models(id),
    provider_id uuid REFERENCES providers(id),
    provider_model_id uuid REFERENCES provider_models(id),
    route_version_id uuid REFERENCES route_versions(id),
    credential_id uuid REFERENCES credentials(id),
    price_version_id uuid REFERENCES price_versions(id),
    input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    reasoning_tokens bigint NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
    amount_minor bigint CHECK (amount_minor IS NULL OR amount_minor >= 0),
    currency char(3),
    estimated boolean NOT NULL DEFAULT false,
    provenance text NOT NULL DEFAULT 'adapter',
    terminal_status text NOT NULL CHECK (terminal_status IN ('succeeded', 'failed', 'cancelled', 'reconcile_needed')),
    upstream_status integer CHECK (upstream_status IS NULL OR upstream_status BETWEEN 100 AND 599),
    error_class text,
    latency_ms bigint CHECK (latency_ms IS NULL OR latency_ms >= 0),
    ttft_ms bigint CHECK (ttft_ms IS NULL OR ttft_ms >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX usage_events_user_created_idx ON usage_events (user_id, created_at DESC);
CREATE INDEX usage_events_api_key_created_idx ON usage_events (api_key_id, created_at DESC);
CREATE INDEX usage_events_model_created_idx ON usage_events (resolved_model_id, created_at DESC);
CREATE INDEX usage_events_provider_created_idx ON usage_events (provider_id, created_at DESC);
CREATE INDEX usage_events_credential_created_idx ON usage_events (credential_id, created_at DESC);
CREATE INDEX usage_events_request_idx ON usage_events (request_id);
CREATE INDEX usage_events_status_created_idx ON usage_events (terminal_status, created_at DESC);

CREATE TABLE usage_reconciliations (
    id uuid PRIMARY KEY,
    usage_event_id uuid NOT NULL REFERENCES usage_events(id),
    source text NOT NULL,
    source_event_key text NOT NULL,
    input_tokens_delta bigint NOT NULL DEFAULT 0,
    output_tokens_delta bigint NOT NULL DEFAULT 0,
    cache_read_tokens_delta bigint NOT NULL DEFAULT 0,
    cache_write_tokens_delta bigint NOT NULL DEFAULT 0,
    reasoning_tokens_delta bigint NOT NULL DEFAULT 0,
    amount_minor_delta bigint NOT NULL DEFAULT 0,
    status text NOT NULL DEFAULT 'recorded' CHECK (status IN ('recorded', 'applied', 'rejected')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, source_event_key)
);
CREATE INDEX usage_reconciliations_usage_idx ON usage_reconciliations (usage_event_id, created_at DESC);

CREATE TABLE scheduler_decisions (
    id uuid PRIMARY KEY,
    request_record_id uuid REFERENCES request_records(id),
    request_id text NOT NULL,
    attempt_no integer NOT NULL CHECK (attempt_no >= 0),
    decision_no integer NOT NULL CHECK (decision_no >= 0),
    strategy_version text NOT NULL,
    candidates jsonb NOT NULL DEFAULT '[]'::jsonb,
    selected_target_id uuid REFERENCES route_targets(id),
    fallback_chain jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (request_id, attempt_no, decision_no)
);
CREATE INDEX scheduler_decisions_request_created_idx ON scheduler_decisions (request_id, created_at DESC);
CREATE INDEX scheduler_decisions_selected_created_idx ON scheduler_decisions (selected_target_id, created_at DESC);

CREATE TABLE wallets (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    currency char(3) NOT NULL,
    available_balance_minor bigint NOT NULL DEFAULT 0,
    reserved_balance_minor bigint NOT NULL DEFAULT 0,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'closed')),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, currency),
    CHECK (available_balance_minor >= 0),
    CHECK (reserved_balance_minor >= 0)
);
CREATE INDEX wallets_user_status_idx ON wallets (user_id, status);


CREATE TABLE wallet_ledger (
    id uuid PRIMARY KEY,
    wallet_id uuid NOT NULL REFERENCES wallets(id),
    entry_type text NOT NULL CHECK (entry_type IN ('reservation', 'usage_charge', 'release', 'recharge', 'refund', 'adjustment', 'grant', 'expiration')),
    amount_minor bigint NOT NULL CHECK (amount_minor <> 0),
    currency char(3) NOT NULL,
    reference_type text NOT NULL,
    reference_id text NOT NULL,
    idempotency_key text NOT NULL,
    usage_event_id uuid REFERENCES usage_events(id),
    operator_user_id uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (wallet_id, idempotency_key)
);
CREATE UNIQUE INDEX wallet_ledger_usage_event_uq ON wallet_ledger (wallet_id, usage_event_id)
    WHERE usage_event_id IS NOT NULL;
CREATE INDEX wallet_ledger_wallet_created_idx ON wallet_ledger (wallet_id, created_at DESC);
CREATE INDEX wallet_ledger_reference_idx ON wallet_ledger (reference_type, reference_id);

CREATE TABLE payment_orders (
    id uuid PRIMARY KEY,
    order_no text NOT NULL UNIQUE,
    user_id uuid NOT NULL REFERENCES users(id),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency char(3) NOT NULL,
    payment_provider text NOT NULL,
    provider_trade_no text,
    status text NOT NULL DEFAULT 'created' CHECK (status IN ('created', 'pending', 'paid', 'failed', 'expired', 'refunded', 'closed')),
    idempotency_key text NOT NULL UNIQUE,
    expires_at timestamptz,
    paid_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_orders_provider_trade_uq ON payment_orders (payment_provider, provider_trade_no)
    WHERE provider_trade_no IS NOT NULL;
CREATE INDEX payment_orders_user_created_idx ON payment_orders (user_id, created_at DESC);
CREATE INDEX payment_orders_status_created_idx ON payment_orders (status, created_at DESC);

CREATE TABLE payment_events (
    id uuid PRIMARY KEY,
    payment_provider text NOT NULL,
    provider_event_id text NOT NULL,
    order_id uuid REFERENCES payment_orders(id),
    provider_trade_no text,
    event_type text NOT NULL,
    amount_minor bigint,
    currency char(3),
    payload_sha256 bytea NOT NULL,
    signature_verified boolean NOT NULL DEFAULT false,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_provider, provider_event_id)
);
CREATE INDEX payment_events_order_received_idx ON payment_events (order_id, received_at DESC);


CREATE TABLE audit_logs (
    id uuid PRIMARY KEY,
    event_id text NOT NULL UNIQUE,
    actor_user_id uuid REFERENCES users(id),
    action text NOT NULL,
    target_type text NOT NULL,
    target_id text NOT NULL,
    request_id text,
    result text NOT NULL CHECK (result IN ('success', 'denied', 'failure')),
    before_summary jsonb,
    after_summary jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_actor_created_idx ON audit_logs (actor_user_id, created_at DESC);
CREATE INDEX audit_logs_target_created_idx ON audit_logs (target_type, target_id, created_at DESC);
CREATE INDEX audit_logs_request_idx ON audit_logs (request_id);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY,
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    event_type text NOT NULL,
    idempotency_key text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'published', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claimed_at timestamptz,
    published_at timestamptz,
    last_error_class text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (aggregate_type, aggregate_id, event_type, idempotency_key)
);
CREATE INDEX outbox_claim_idx ON outbox_events (status, next_attempt_at, created_at);
CREATE INDEX outbox_aggregate_idx ON outbox_events (aggregate_type, aggregate_id, created_at DESC);

CREATE TABLE stats_rollups (
    id uuid PRIMARY KEY,
    bucket_kind text NOT NULL CHECK (bucket_kind IN ('hour', 'day')),
    bucket_start timestamptz NOT NULL,
    dimension_key text NOT NULL,
    request_count bigint NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    success_count bigint NOT NULL DEFAULT 0 CHECK (success_count >= 0),
    input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    amount_minor bigint NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    latency_sum_ms bigint NOT NULL DEFAULT 0 CHECK (latency_sum_ms >= 0),
    ttft_sum_ms bigint NOT NULL DEFAULT 0 CHECK (ttft_sum_ms >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (bucket_kind, bucket_start, dimension_key)
);
CREATE INDEX stats_rollups_bucket_idx ON stats_rollups (bucket_kind, bucket_start DESC);

-- +goose Down

DROP TABLE IF EXISTS stats_rollups;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS payment_events;
DROP TABLE IF EXISTS payment_orders;
DROP TABLE IF EXISTS wallet_ledger;
DROP TABLE IF EXISTS wallets;
DROP TABLE IF EXISTS scheduler_decisions;
DROP TABLE IF EXISTS usage_reconciliations;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS request_attempts;
DROP TABLE IF EXISTS request_records;
DROP TABLE IF EXISTS model_prices;
DROP TABLE IF EXISTS price_versions;
DROP TABLE IF EXISTS quota_snapshots;
DROP TABLE IF EXISTS route_targets;
DROP TABLE IF EXISTS route_versions;
DROP TABLE IF EXISTS model_routes;
DROP TABLE IF EXISTS credential_health;
DROP TABLE IF EXISTS pool_members;
DROP TABLE IF EXISTS credential_pools;
DROP TABLE IF EXISTS credential_secrets;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS provider_models;
DROP TABLE IF EXISTS providers;
DROP TABLE IF EXISTS policy_model_entitlements;
DROP TABLE IF EXISTS models;
DROP TABLE IF EXISTS api_key_policies;
DROP TABLE IF EXISTS policies;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS mfa_recovery_codes;
DROP TABLE IF EXISTS mfa_factors;
DROP TABLE IF EXISTS user_sessions;
DROP TABLE IF EXISTS user_roles;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS users;
