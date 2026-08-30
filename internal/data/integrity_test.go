package data

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/starhui-dev/bablo/migrations"
)

func TestProviderOwnershipConstraints(t *testing.T) {
	url := testDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := Migrate(ctx, url, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	store, err := Open(ctx, Config{URL: url, MaxConns: 2})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	providerA := newTestUUID(t)
	providerB := newTestUUID(t)
	credentialA := newTestUUID(t)
	credentialB := newTestUUID(t)
	poolA := newTestUUID(t)
	poolB := newTestUUID(t)
	model := newTestUUID(t)
	providerModelA := newTestUUID(t)
	providerModelB := newTestUUID(t)
	route := newTestUUID(t)
	routeVersion := newTestUUID(t)

	if err := store.WithTx(ctx, func(q Querier) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO providers (id, slug, display_name, resource_type) VALUES ($1, $2, 'A', 'official_api'), ($3, $4, 'B', 'third_party')`, []any{providerA, "provider-a-" + providerA.String(), providerB, "provider-b-" + providerB.String()}},
			{`INSERT INTO credentials (id, provider_id, external_stable_id, source_kind) VALUES ($1, $2, $3, 'api_key'), ($4, $5, $6, 'api_key')`, []any{credentialA, providerA, "credential-a-" + credentialA.String(), credentialB, providerB, "credential-b-" + credentialB.String()}},
			{`INSERT INTO credential_pools (id, name, provider_id) VALUES ($1, $2, $3), ($4, $5, $6)`, []any{poolA, "pool-a-" + poolA.String(), providerA, poolB, "pool-b-" + poolB.String(), providerB}},
			{`INSERT INTO models (id, public_model_id, display_name) VALUES ($1, $2, 'test')`, []any{model, "ownership-model-" + model.String()}},
			{`INSERT INTO provider_models (id, provider_id, model_id, upstream_model_id, protocol) VALUES ($1, $2, $3, $4, 'openai_chat'), ($5, $6, $3, $7, 'openai_chat')`, []any{providerModelA, providerA, model, "upstream-a-" + providerModelA.String(), providerModelB, providerB, "upstream-b-" + providerModelB.String()}},
			{`INSERT INTO model_routes (id, model_id, match_value) VALUES ($1, $2, $3)`, []any{route, model, "ownership-route-" + route.String()}},
			{`INSERT INTO route_versions (id, route_id, version_no, snapshot_hash) VALUES ($1, $2, 1, $3)`, []any{routeVersion, route, bytes.Repeat([]byte{0x5a}, 32)}},
		}
		for _, statement := range statements {
			if _, err := q.Exec(ctx, statement.sql, statement.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed ownership fixtures: %v", err)
	}

	_, err = store.Queryer().Exec(ctx, `INSERT INTO pool_members (pool_id, credential_id) VALUES ($1, $2)`, poolB, credentialA)
	assertPostgresCode(t, err, "23514")

	_, err = store.Queryer().Exec(ctx, `INSERT INTO route_targets (id, route_version_id, target_no, provider_model_id, credential_pool_id) VALUES ($1, $2, 0, $3, $4)`, newTestUUID(t), routeVersion, providerModelA, poolB)
	assertPostgresCode(t, err, "23514")
}
