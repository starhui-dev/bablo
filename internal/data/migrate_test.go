package data

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/starhui-dev/bablo/migrations"
)

func TestMigrationsUpgradeAndRepeatSafely(t *testing.T) {
	url := testDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	provider, err := newMigrationProvider(url, migrations.Files, logger)
	if err != nil {
		t.Fatalf("newMigrationProvider() error = %v", err)
	}
	defer func() { _ = provider.Close() }()

	if _, err := provider.UpByOne(ctx); err != nil {
		t.Fatalf("apply first migration: %v", err)
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 1 {
		t.Fatalf("version after first migration = %d, %v", version, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("upgrade to latest: %v", err)
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 3 {
		t.Fatalf("version after upgrade = %d, %v", version, err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("repeat migration applied %d migrations", len(results))
	}
}

func TestCoreSchemaConstraints(t *testing.T) {
	url := testDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Migrate(ctx, url, migrations.Files, logger); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	store, err := Open(ctx, Config{URL: url, MaxConns: 2})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	userID := newTestUUID(t)
	keyID := newTestUUID(t)
	policyID := newTestUUID(t)
	modelA := newTestUUID(t)
	modelB := newTestUUID(t)
	usageID := newTestUUID(t)

	if err := store.WithTx(ctx, func(q Querier) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO users (id, email_normalized, password_hash, password_params_version) VALUES ($1, $2, 'hash', 'argon2id-v1')`, []any{userID, userID.String() + "@example.test"}},
			{`INSERT INTO api_keys (id, user_id, name, key_prefix, secret_hash) VALUES ($1, $2, 'test', 'bablo_test', $3)`, []any{keyID, userID, []byte("01234567890123456789012345678901")}},
			{`INSERT INTO policies (id, name) VALUES ($1, $2)`, []any{policyID, "policy-" + policyID.String()}},
			{`INSERT INTO api_key_policies (api_key_id, policy_id) VALUES ($1, $2)`, []any{keyID, policyID}},
			{`INSERT INTO models (id, public_model_id, display_name) VALUES ($1, $2, 'A'), ($3, $4, 'B')`, []any{modelA, "model-a-" + modelA.String(), modelB, "model-b-" + modelB.String()}},
			{`INSERT INTO policy_model_entitlements (policy_id, model_id) VALUES ($1, $2), ($1, $3)`, []any{policyID, modelA, modelB}},
			{`INSERT INTO usage_events (id, settlement_key, request_id, user_id, api_key_id, requested_model, resolved_model_id, terminal_status) VALUES ($1, $2, $3, $4, $5, $6, $7, 'succeeded')`, []any{usageID, "settlement-" + usageID.String(), "request-" + usageID.String(), userID, keyID, "model-a", modelA}},
		}
		for _, statement := range statements {
			if _, err := q.Exec(ctx, statement.sql, statement.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed core schema: %v", err)
	}

	var entitlementCount int
	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FROM policy_model_entitlements WHERE policy_id = $1`, policyID).Scan(&entitlementCount); err != nil {
		t.Fatalf("count entitlements: %v", err)
	}
	if entitlementCount != 2 {
		t.Fatalf("one API key policy reached %d models, want 2", entitlementCount)
	}

	_, err = store.Queryer().Exec(ctx, `UPDATE usage_events SET input_tokens = 1 WHERE id = $1`, usageID)
	assertPostgresCode(t, err, "55000")

	duplicateUsageID := newTestUUID(t)
	_, err = store.Queryer().Exec(ctx, `INSERT INTO usage_events (id, settlement_key, request_id, requested_model, terminal_status) VALUES ($1, $2, $3, 'model-a', 'succeeded')`, duplicateUsageID, "settlement-"+usageID.String(), "duplicate-request")
	assertPostgresCode(t, err, "23505")
}

func newTestUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	return id
}

func assertPostgresCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected PostgreSQL error %s", code)
	}
	pgErr, ok := err.(*pgconn.PgError)
	if !ok || pgErr.Code != code {
		t.Fatalf("PostgreSQL error = %T %v, want code %s", err, err, code)
	}
}
