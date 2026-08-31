package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/migrations"
)

type usageFixtures struct {
	user          uuid.UUID
	apiKey        uuid.UUID
	model         uuid.UUID
	provider      uuid.UUID
	providerModel uuid.UUID
	credential    uuid.UUID
	pool          uuid.UUID
	route         uuid.UUID
	version       uuid.UUID
	target        uuid.UUID
	price         uuid.UUID
	wallet        uuid.UUID
}

func openUsageTestStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_usage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create test schema: %v", err)
	}
	pool.Close()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	databaseURL := parsed.String()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		cleanupPool, err := pgxpool.New(cleanupCtx, baseURL)
		if err != nil {
			t.Errorf("open cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	if err := data.Migrate(ctx, databaseURL, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 16})
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func seedUsageFixtures(t *testing.T, store *data.Store) usageFixtures {
	t.Helper()
	f := usageFixtures{
		user:          uuid.New(),
		apiKey:        uuid.New(),
		model:         uuid.New(),
		provider:      uuid.New(),
		providerModel: uuid.New(),
		credential:    uuid.New(),
		pool:          uuid.New(),
		route:         uuid.New(),
		version:       uuid.New(),
		target:        uuid.New(),
		price:         uuid.New(),
		wallet:        uuid.New(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := store.WithTx(ctx, func(q data.Querier) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO users (id, email_normalized, password_hash, password_params_version) VALUES ($1, $2, 'test-hash', 'test')`, []any{f.user, "usage-" + f.user.String() + "@example.test"}},
			{`INSERT INTO wallets (id, user_id, currency) VALUES ($1, $2, 'USD')`, []any{f.wallet, f.user}},
			{`INSERT INTO api_keys (id, user_id, name, key_prefix, secret_hash) VALUES ($1, $2, 'usage-test', 'bablo_sk_usage', $3)`, []any{f.apiKey, f.user, []byte("secret-hash")}},
			{`INSERT INTO models (id, public_model_id, display_name, billing_class) VALUES ($1, $2, 'Usage Test Model', 'token')`, []any{f.model, "usage-model-" + f.model.String()}},
			{`INSERT INTO providers (id, slug, display_name, resource_type, commercial_allowed) VALUES ($1, $2, 'Usage Provider', 'official_api', true)`, []any{f.provider, "usage-provider-" + f.provider.String()}},
			{`INSERT INTO provider_models (id, provider_id, model_id, upstream_model_id, protocol) VALUES ($1, $2, $3, $4, 'openai_chat')`, []any{f.providerModel, f.provider, f.model, "upstream-" + f.providerModel.String()}},
			{`INSERT INTO credentials (id, provider_id, external_stable_id, source_kind) VALUES ($1, $2, $3, 'api_key')`, []any{f.credential, f.provider, "credential-" + f.credential.String()}},
			{`INSERT INTO credential_pools (id, name, provider_id) VALUES ($1, $2, $3)`, []any{f.pool, "usage-pool-" + f.pool.String(), f.provider}},
			{`INSERT INTO pool_members (pool_id, credential_id) VALUES ($1, $2)`, []any{f.pool, f.credential}},
			{`INSERT INTO model_routes (id, model_id, match_value) VALUES ($1, $2, $3)`, []any{f.route, f.model, "usage-route-" + f.route.String()}},
			{`INSERT INTO route_versions (id, route_id, version_no, snapshot_hash, created_by) VALUES ($1, $2, 1, $3, $4)`, []any{f.version, f.route, bytes.Repeat([]byte{0x31}, 32), f.user}},
			{`INSERT INTO route_targets (id, route_version_id, target_no, provider_model_id, credential_pool_id) VALUES ($1, $2, 0, $3, $4)`, []any{f.target, f.version, f.providerModel, f.pool}},
			{`UPDATE model_routes SET active_version_id = $2 WHERE id = $1`, []any{f.route, f.version}},
			{`INSERT INTO price_versions (id, scope, version_no, currency, effective_from, status, created_by) VALUES ($1, 'provider_model', 1, 'USD', now() - interval '1 minute', 'draft', $2)`, []any{f.price, f.user}},
			{`INSERT INTO model_prices (id, price_version_id, pricing_scope, provider_model_id, dimension, unit_price) VALUES ($1, $2, 'provider_model', $3, 'input_token', 0.000001), ($4, $2, 'provider_model', $3, 'output_token', 0.000002)`, []any{uuid.New(), f.price, f.providerModel, uuid.New()}},
		}
		for _, statement := range statements {
			if _, err := q.Exec(ctx, statement.sql, statement.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed usage fixtures: %v", err)
	}
	return f
}

func usageServiceForTest(t *testing.T, store *data.Store) *Service {
	t.Helper()
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error: %v", err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("NewService() error: %v", err)
	}
	return service
}

func baseUsageStart(f usageFixtures, requestID string) StartInput {
	return StartInput{
		RequestID:      requestID,
		UserID:         f.user,
		APIKeyID:       f.apiKey,
		Endpoint:       "/v1/chat/completions",
		RequestedModel: "usage-model",
		StartedAt:      time.Now().UTC().Add(-time.Second),
	}
}

func finalizedUsageInput(f usageFixtures) FinalizeInput {
	return FinalizeInput{
		Attempt: &AttemptInput{
			AttemptNo:       0,
			RouteVersionID:  &f.version,
			ProviderID:      &f.provider,
			ProviderModelID: &f.providerModel,
			CredentialID:    &f.credential,
		},
		ResolvedModelID: &f.model,
		ProviderID:      &f.provider,
		ProviderModelID: &f.providerModel,
		RouteVersionID:  &f.version,
		CredentialID:    &f.credential,
		PriceVersionID:  &f.price,
		WalletID:        &f.wallet,
		Usage:           TokenUsage{InputTokens: 12, OutputTokens: 8, CacheReadTokens: 3, ReasoningTokens: 2},
		AmountMinor:     new(int64(17)),
		Currency:        "USD",
		Provenance:      ProvenanceAdapter,
		TerminalStatus:  StatusSucceeded,
		UpstreamStatus:  new(200),
		Latency:         120 * time.Millisecond,
		TTFT:            new(35 * time.Millisecond),
		FinishedAt:      time.Now().UTC(),
	}
}

func TestServiceFinalizeIsIdempotentAndOutboxAtomic(t *testing.T) {
	store := openUsageTestStore(t)
	fixtures := seedUsageFixtures(t, store)
	service := usageServiceForTest(t, store)
	ctx := context.Background()
	handle, err := service.BeginRequest(ctx, baseUsageStart(fixtures, "usage-idempotent"))
	if err != nil {
		t.Fatalf("BeginRequest() error: %v", err)
	}
	input := finalizedUsageInput(fixtures)
	first, err := service.Finalize(ctx, handle, input)
	if err != nil {
		t.Fatalf("first Finalize() error: %v", err)
	}
	second, err := service.Finalize(ctx, handle, input)
	if err != nil {
		t.Fatalf("second Finalize() error: %v", err)
	}
	if first.ID != second.ID || first.SettlementKey != second.SettlementKey {
		t.Fatalf("idempotent events differ: first=%+v second=%+v", first, second)
	}
	if first.StartedAt.IsZero() || first.FinishedAt.IsZero() || first.FinishedAt.Before(first.StartedAt) {
		t.Fatalf("usage event time snapshot invalid: started=%v finished=%v", first.StartedAt, first.FinishedAt)
	}
	conflicting := input
	conflicting.Usage.OutputTokens++
	if _, err := service.Finalize(ctx, handle, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Finalize() error = %v, want ErrConflict", err)
	}
	if count := queryUsageCount(t, store, `SELECT count(*) FROM usage_events WHERE request_id = $1`, handle.RequestID); count != 1 {
		t.Fatalf("usage event count = %d, want 1", count)
	}
	var startedAt, finishedAt time.Time
	if err := store.Queryer().QueryRow(ctx, `SELECT started_at, finished_at FROM usage_events WHERE id = $1`, first.ID).Scan(&startedAt, &finishedAt); err != nil {
		t.Fatalf("read usage event timestamps: %v", err)
	}
	if finishedAt.Before(startedAt) {
		t.Fatalf("persisted usage event timestamps inverted: started=%v finished=%v", startedAt, finishedAt)
	}
	if count := queryUsageCount(t, store, `SELECT count(*) FROM outbox_events WHERE aggregate_type = 'usage_event' AND aggregate_id = $1`, first.ID); count != 1 {
		t.Fatalf("usage outbox count = %d, want 1", count)
	}
	var payload string
	if err := store.Queryer().QueryRow(ctx, `SELECT payload::text FROM outbox_events WHERE aggregate_id = $1`, first.ID).Scan(&payload); err != nil {
		t.Fatalf("read outbox payload: %v", err)
	}
	if strings.Contains(payload, "prompt") || strings.Contains(payload, "response") || strings.Contains(payload, "secret") {
		t.Fatalf("outbox payload contains body/secret material: %s", payload)
	}
	mismatchedID := uuid.New()
	_, err = store.Queryer().Exec(ctx, `
        INSERT INTO usage_events (id, settlement_key, request_record_id, request_id, requested_model, terminal_status)
        VALUES ($1, $2, $3, $4, $5, 'succeeded')`,
		mismatchedID, "mismatched-settlement-"+mismatchedID.String(), handle.RecordID, "different-request-id", handle.RequestedModel)
	assertUsagePostgresCode(t, err, "23514")
	_, err = store.Queryer().Exec(ctx, `UPDATE usage_events SET output_tokens = 99 WHERE id = $1`, first.ID)
	assertUsagePostgresCode(t, err, "55000")
}

func TestServiceReconciliationIsAppendOnlyAndIdempotent(t *testing.T) {
	store := openUsageTestStore(t)
	fixtures := seedUsageFixtures(t, store)
	service := usageServiceForTest(t, store)
	ctx := context.Background()
	handle, err := service.BeginRequest(ctx, baseUsageStart(fixtures, "usage-reconcile"))
	if err != nil {
		t.Fatalf("BeginRequest() error: %v", err)
	}
	input := FinalizeInput{TerminalStatus: StatusReconcileNeeded, Provenance: ProvenanceMissingUsage, FinishedAt: time.Now().UTC()}
	event, err := service.Finalize(ctx, handle, input)
	if err != nil {
		t.Fatalf("Finalize() error: %v", err)
	}
	reconciliationInput := ReconciliationInput{
		UsageEventID:      event.ID,
		Source:            "cpa",
		SourceEventKey:    "late-usage-1",
		InputTokensDelta:  4,
		OutputTokensDelta: 7,
		AmountMinorDelta:  2,
	}
	first, err := service.RecordReconciliation(ctx, reconciliationInput)
	if err != nil {
		t.Fatalf("first reconciliation error: %v", err)
	}
	second, err := service.RecordReconciliation(ctx, reconciliationInput)
	if err != nil {
		t.Fatalf("second reconciliation error: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("reconciliation IDs differ: %s vs %s", first.ID, second.ID)
	}
	conflicting := reconciliationInput
	conflicting.AmountMinorDelta++
	if _, err := service.RecordReconciliation(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting reconciliation error = %v, want ErrConflict", err)
	}
	if count := queryUsageCount(t, store, `SELECT count(*) FROM usage_reconciliations WHERE source = $1 AND source_event_key = $2`, "cpa", "late-usage-1"); count != 1 {
		t.Fatalf("reconciliation count = %d, want 1", count)
	}
	var inputTokens int64
	if err := store.Queryer().QueryRow(ctx, `SELECT input_tokens FROM usage_events WHERE id = $1`, event.ID).Scan(&inputTokens); err != nil {
		t.Fatalf("read original usage: %v", err)
	}
	if inputTokens != 0 {
		t.Fatalf("original usage was mutated to %d", inputTokens)
	}
	_, err = store.Queryer().Exec(ctx, `UPDATE usage_reconciliations SET amount_minor_delta = 8 WHERE id = $1`, first.ID)
	assertUsagePostgresCode(t, err, "55000")
}

func TestServiceOutboxOwnershipAndStaleRecovery(t *testing.T) {
	store := openUsageTestStore(t)
	fixtures := seedUsageFixtures(t, store)
	service := usageServiceForTest(t, store)
	ctx := context.Background()
	handle, err := service.BeginRequest(ctx, baseUsageStart(fixtures, "usage-outbox"))
	if err != nil {
		t.Fatalf("BeginRequest() error: %v", err)
	}
	event, err := service.Finalize(ctx, handle, finalizedUsageInput(fixtures))
	if err != nil {
		t.Fatalf("Finalize() error: %v", err)
	}
	claimed, err := service.ClaimOutbox(ctx, "worker-a", 10, time.Hour)
	if err != nil {
		t.Fatalf("first ClaimOutbox() error: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID == uuid.Nil || claimed[0].Attempts != 1 {
		t.Fatalf("first claims = %+v", claimed)
	}
	if other, err := service.ClaimOutbox(ctx, "worker-b", 10, time.Hour); err != nil {
		t.Fatalf("second ClaimOutbox() error: %v", err)
	} else if len(other) != 0 {
		t.Fatalf("non-stale event was reclaimed: %+v", other)
	}
	if _, err := store.Queryer().Exec(ctx, `UPDATE outbox_events SET claimed_at = now() - interval '2 hours' WHERE id = $1`, eventOutboxID(t, store, event.ID)); err != nil {
		t.Fatalf("age outbox claim: %v", err)
	}
	reclaimed, err := service.ClaimOutbox(ctx, "worker-b", 10, time.Hour)
	if err != nil {
		t.Fatalf("stale ClaimOutbox() error: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Attempts != 2 || reclaimed[0].ClaimedBy != "worker-b" {
		t.Fatalf("stale claims = %+v", reclaimed)
	}
	if err := service.MarkOutboxPublished(ctx, reclaimed[0].ID, "worker-a", time.Now()); !errors.Is(err, ErrOutboxNotOwned) {
		t.Fatalf("wrong owner error = %v, want ErrOutboxNotOwned", err)
	}
	if err := service.MarkOutboxPublished(ctx, reclaimed[0].ID, "worker-b", time.Now()); err != nil {
		t.Fatalf("MarkOutboxPublished() error: %v", err)
	}
	var status string
	if err := store.Queryer().QueryRow(ctx, `SELECT status FROM outbox_events WHERE id = $1`, reclaimed[0].ID).Scan(&status); err != nil {
		t.Fatalf("read outbox status: %v", err)
	}
	if status != "published" {
		t.Fatalf("outbox status = %q, want published", status)
	}
}

func TestServiceConcurrentFinalizeProducesOneEvent(t *testing.T) {
	store := openUsageTestStore(t)
	fixtures := seedUsageFixtures(t, store)
	service := usageServiceForTest(t, store)
	ctx := context.Background()
	handle, err := service.BeginRequest(ctx, baseUsageStart(fixtures, "usage-concurrent"))
	if err != nil {
		t.Fatalf("BeginRequest() error: %v", err)
	}
	input := finalizedUsageInput(fixtures)
	const workers = 32
	results := make(chan Event, workers)
	errorsCh := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for index := 0; index < workers; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			event, err := service.Finalize(ctx, handle, input)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- event
		}()
	}
	waitGroup.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent Finalize() error: %v", err)
	}
	var eventID uuid.UUID
	for event := range results {
		if eventID == uuid.Nil {
			eventID = event.ID
		} else if event.ID != eventID {
			t.Fatalf("concurrent event IDs differ: %s vs %s", eventID, event.ID)
		}
	}
	if eventID == uuid.Nil {
		t.Fatal("no concurrent finalize result")
	}
	if count := queryUsageCount(t, store, `SELECT count(*) FROM usage_events WHERE request_id = $1`, handle.RequestID); count != 1 {
		t.Fatalf("usage event count = %d, want 1", count)
	}
	if count := queryUsageCount(t, store, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, eventID); count != 1 {
		t.Fatalf("outbox count = %d, want 1", count)
	}
}

func queryUsageCount(t *testing.T, store *data.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.Queryer().QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	return count
}

func eventOutboxID(t *testing.T, store *data.Store, aggregateID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := store.Queryer().QueryRow(context.Background(), `SELECT id FROM outbox_events WHERE aggregate_type = 'usage_event' AND aggregate_id = $1`, aggregateID).Scan(&id); err != nil {
		t.Fatalf("read outbox id: %v", err)
	}
	return id
}

func assertUsagePostgresCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("database error = nil, want PostgreSQL code %s", want)
	}
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		t.Fatalf("database error = %v, want PostgreSQL code %s", err, want)
	}
	if databaseError.Code != want {
		t.Fatalf("database error code = %s, want %s", databaseError.Code, want)
	}
}
