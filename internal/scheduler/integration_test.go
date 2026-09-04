package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/route"
	"github.com/starhui-dev/bablo/migrations"
)

func TestSelectFiltersCredentialStateAndPersistsDecision(t *testing.T) {
	fixture := newSchedulerFixture(t, 3)
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	service := fixture.service(t, coordinator)

	now := time.Now().UTC().Truncate(time.Microsecond)
	cooldown := now.Add(time.Hour)
	if _, err := fixture.store.Queryer().Exec(context.Background(), `UPDATE credentials SET status = 'disabled' WHERE id = $1`, fixture.credentials[1]); err != nil {
		t.Fatalf("disable credential: %v", err)
	}
	if _, err := fixture.store.Queryer().Exec(context.Background(), `INSERT INTO credential_health (credential_id, last_error_at, last_error_class, cooldown_until, observed_at) VALUES ($1, $3, 'upstream_429', $2, $3) ON CONFLICT (credential_id) DO UPDATE SET last_error_at = EXCLUDED.last_error_at, last_error_class = EXCLUDED.last_error_class, cooldown_until = EXCLUDED.cooldown_until, observed_at = EXCLUDED.observed_at`, fixture.credentials[2], cooldown, now); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}

	selection, err := service.Select(context.Background(), Request{
		RequestID:  "scheduler-filter",
		Resolution: fixture.resolution,
		Strategy:   StrategyFillFirst,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.CredentialID != fixture.credentials[0] {
		t.Fatalf("selected credential = %s, want %s", selection.CredentialID, fixture.credentials[0])
	}
	if err := selection.Release(context.Background()); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if selection.Decision.SelectedProviderID == nil || *selection.Decision.SelectedProviderID != fixture.providerID {
		t.Fatalf("selected provider = %v, want %s", selection.Decision.SelectedProviderID, fixture.providerID)
	}
	if !hasCandidateReason(selection.Decision, fixture.credentials[1], "credential_inactive") {
		t.Fatalf("decision did not record disabled credential: %+v", selection.Decision)
	}
	if !hasCandidateReason(selection.Decision, fixture.credentials[2], "cooldown") {
		t.Fatalf("decision did not record 429 cooldown credential: %+v", selection.Decision)
	}

	var storedCandidates []byte
	var storedFallback []byte
	if err := fixture.store.Queryer().QueryRow(context.Background(), `SELECT candidates, fallback_chain FROM scheduler_decisions WHERE request_id = $1`, "scheduler-filter").Scan(&storedCandidates, &storedFallback); err != nil {
		t.Fatalf("read scheduler decision: %v", err)
	}
	var candidates []CandidateDecision
	if err := json.Unmarshal(storedCandidates, &candidates); err != nil || len(candidates) != len(selection.Decision.Candidates) {
		t.Fatalf("stored candidates = %s, err=%v", storedCandidates, err)
	}
	if len(storedFallback) == 0 {
		t.Fatal("stored fallback chain is empty JSON")
	}
	_, err = fixture.store.Queryer().Exec(context.Background(), `UPDATE scheduler_decisions SET request_id = 'mutated' WHERE request_id = $1`, "scheduler-filter")
	if !isPostgresCode(err, "55000") {
		t.Fatalf("scheduler decision mutation error = %v, want 55000", err)
	}
	_, err = fixture.store.Queryer().Exec(context.Background(), `
		INSERT INTO scheduler_decisions (
			id, request_id, attempt_no, decision_no, strategy_version,
			candidates, route_version_id, selected_target_id, fallback_chain
		) VALUES ($1, 'scheduler-invalid-selection', 0, 0, 'test/v1', '[]', $2, $3, '[]')`,
		uuid.New(), fixture.resolution.Version.ID, selection.Target.ID)
	if !isPostgresCode(err, "23514") {
		t.Fatalf("incomplete scheduler selection error = %v, want 23514", err)
	}
}

func TestRoundRobinUsesCursorAndFallsBackAroundBusyLease(t *testing.T) {
	fixture := newSchedulerFixture(t, 2)
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	service := fixture.service(t, coordinator)
	ctx := context.Background()

	first, err := service.Select(ctx, Request{RequestID: "scheduler-round-robin-1", Resolution: fixture.resolution, Strategy: StrategyRoundRobin})
	if err != nil {
		t.Fatalf("first Select() error = %v", err)
	}
	defer first.Release(ctx)
	second, err := service.Select(ctx, Request{RequestID: "scheduler-round-robin-2", Resolution: fixture.resolution, Strategy: StrategyFillFirst})
	if err != nil {
		t.Fatalf("second Select() error = %v", err)
	}
	defer second.Release(ctx)
	if first.CredentialID == second.CredentialID {
		t.Fatalf("fallback selected busy credential twice: %s", first.CredentialID)
	}
	if !hasCandidateReason(second.Decision, first.CredentialID, "concurrency_lease_busy") {
		t.Fatalf("fallback decision omitted busy credential: %+v", second.Decision)
	}
}

func TestConcurrentSelectionHonorsCredentialCapacityAcrossInstances(t *testing.T) {
	fixture := newSchedulerFixture(t, 2)
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	services := []*Service{fixture.service(t, coordinator), fixture.service(t, coordinator)}
	ctx := context.Background()

	var successes atomic.Int64
	var failures atomic.Int64
	var waitGroup sync.WaitGroup
	selections := make(chan Selection, 20)
	for index := range 20 {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			selection, err := services[index%len(services)].Select(ctx, Request{
				RequestID:  "scheduler-concurrent-" + uuid.NewString(),
				DecisionNo: index,
				Resolution: fixture.resolution,
				Strategy:   StrategyFillFirst,
			})
			if err != nil {
				if !errors.Is(err, ErrNoEligible) {
					t.Errorf("concurrent Select() error = %v", err)
				}
				failures.Add(1)
				return
			}
			successes.Add(1)
			selections <- selection
		}(index)
	}
	waitGroup.Wait()
	close(selections)
	for selection := range selections {
		if err := selection.Release(ctx); err != nil {
			t.Errorf("concurrent Release() error = %v", err)
		}
	}
	if successes.Load() != 2 || failures.Load() != 18 {
		t.Fatalf("concurrent selections = success:%d failure:%d, want 2/18", successes.Load(), failures.Load())
	}
}

func TestQuotaFreshnessAndAffinityFailover(t *testing.T) {
	fixture := newSchedulerFixture(t, 4)
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	service := fixture.service(t, coordinator)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := now.Add(-10 * time.Minute)
	// Scheduler only consumes a snapshot for the resolved provider model; seed the
	// fixture with that exact identity so quota eligibility is tested, not bypassed.
	target := fixture.resolution.Candidates[0]
	if _, err := fixture.store.Queryer().Exec(ctx, `
		INSERT INTO quota_snapshots (id, credential_id, provider_slug, model, observation_key, window_kind, remaining_tokens, reset_at, observed_at, source, confidence)
		VALUES ($1, $2, $3, $4, 'scheduler-1', 'minute', 1, $5, $6, 'test', 'high'),
		       ($7, $8, $3, $4, 'scheduler-2', 'minute', 100, $5, $6, 'test', 'high'),
		       ($9, $10, $3, $4, 'scheduler-3', 'minute', 100, $11, $6, 'test', 'high'),
		       ($12, $13, $3, $4, 'scheduler-4', 'minute', 100, $5, $14, 'test', 'high')`,
		uuid.New(), fixture.credentials[0], target.ProviderSlug, target.UpstreamModelID, now.Add(time.Hour), now,
		uuid.New(), fixture.credentials[1],
		uuid.New(), fixture.credentials[2], now.Add(-time.Minute),
		uuid.New(), fixture.credentials[3], old); err != nil {
		t.Fatalf("seed quota snapshots: %v", err)
	}
	first, err := service.Select(ctx, Request{
		RequestID:  "scheduler-quota",
		Resolution: fixture.resolution,
		Strategy:   StrategyQuotaAware,
		Quota:      QuotaPolicy{Enabled: true, WindowKind: "minute", RequiredTokens: 10, RequireFresh: true},
		Now:        now,
	})
	if err != nil {
		t.Fatalf("quota Select() error = %v", err)
	}
	if first.CredentialID != fixture.credentials[1] {
		t.Fatalf("quota-aware credential = %s, want %s", first.CredentialID, fixture.credentials[1])
	}
	if !hasCandidateReason(first.Decision, fixture.credentials[0], "quota_exhausted") ||
		!hasCandidateReason(first.Decision, fixture.credentials[2], "quota_stale") ||
		!hasCandidateReason(first.Decision, fixture.credentials[3], "quota_stale") {
		t.Fatalf("quota decision omitted exhausted/reset/stale reasons: %+v", first.Decision)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("quota Release() error = %v", err)
	}

	affinityRequest := Request{Resolution: fixture.resolution, AffinityKey: "session-key"}
	if err := coordinator.SetAffinity(ctx, scopedAffinityKey(affinityRequest), fixture.credentials[0], time.Minute); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}
	if _, err := fixture.store.Queryer().Exec(ctx, `UPDATE credentials SET status = 'disabled' WHERE id = $1`, fixture.credentials[0]); err != nil {
		t.Fatalf("disable affinity credential: %v", err)
	}
	second, err := service.Select(ctx, Request{
		RequestID:   "scheduler-affinity-failover",
		Resolution:  fixture.resolution,
		Strategy:    StrategyFillFirst,
		AffinityKey: "session-key",
		Now:         now,
	})
	if err != nil {
		t.Fatalf("affinity Select() error = %v", err)
	}
	if second.CredentialID != fixture.credentials[1] && second.CredentialID != fixture.credentials[2] && second.CredentialID != fixture.credentials[3] {
		t.Fatalf("affinity fallback credential = %s", second.CredentialID)
	}
	if !hasFallbackReason(second.Decision, "affinity_unavailable") {
		t.Fatalf("affinity fallback reason missing: %+v", second.Decision)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatalf("affinity Release() error = %v", err)
	}
	if _, err := fixture.store.Queryer().Exec(ctx, `UPDATE credentials SET status = 'disabled' WHERE id IN ($1, $2, $3)`, fixture.credentials[1], fixture.credentials[2], fixture.credentials[3]); err != nil {
		t.Fatalf("disable remaining credentials: %v", err)
	}
	if _, err := service.Select(ctx, Request{
		RequestID:  "scheduler-stale-quota",
		Resolution: fixture.resolution,
		Strategy:   StrategyQuotaAware,
		Quota:      QuotaPolicy{Enabled: true, WindowKind: "minute", RequiredTokens: 10, RequireFresh: true},
		Now:        now,
	}); !errors.Is(err, ErrNoEligible) {
		t.Fatalf("stale quota Select() error = %v, want ErrNoEligible", err)
	}
}

func TestRedisCoordinatorOptionalIntegration(t *testing.T) {
	rawURL := os.Getenv("BABLO_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("BABLO_TEST_REDIS_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coordinator, err := NewRedisCoordinator(ctx, rawURL, CoordinatorOptions{LeaseTTL: time.Second, CursorTTL: time.Second, AffinityTTL: time.Second})
	if err != nil {
		t.Fatalf("NewRedisCoordinator() error = %v", err)
	}
	defer coordinator.Close()
	coordinatorB, err := NewRedisCoordinator(ctx, rawURL, CoordinatorOptions{LeaseTTL: time.Second, CursorTTL: time.Second, AffinityTTL: time.Second})
	if err != nil {
		t.Fatalf("NewRedisCoordinator(second) error = %v", err)
	}
	defer coordinatorB.Close()
	resource := "integration:" + uuid.NewString()
	lease, err := coordinator.Acquire(ctx, resource, "owner-a", time.Second)
	if err != nil {
		t.Fatalf("Redis Acquire() error = %v", err)
	}
	if _, err := coordinatorB.Acquire(ctx, resource, "owner-b", time.Second); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("Redis duplicate Acquire() error = %v, want ErrLeaseBusy", err)
	}
	otherLease, err := coordinator.Acquire(ctx, resource+":other", "owner-b", time.Second)
	if err != nil {
		t.Fatalf("Redis second resource Acquire() error = %v", err)
	}
	if err := otherLease.Release(ctx); err != nil {
		t.Fatalf("Redis second resource Release() error = %v", err)
	}
	if err := lease.Renew(ctx, time.Second); err != nil {
		t.Fatalf("Redis Renew() error = %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Redis Release() error = %v", err)
	}
	afterRelease, err := coordinatorB.Acquire(ctx, resource, "owner-b", time.Second)
	if err != nil {
		t.Fatalf("Redis Acquire() after release error = %v", err)
	}
	if err := afterRelease.Release(ctx); err != nil {
		t.Fatalf("Redis Release() after reacquire error = %v", err)
	}
	cursorKey := "integration-cursor:" + uuid.NewString()
	first, err := coordinator.Next(ctx, cursorKey, 2, time.Second)
	if err != nil {
		t.Fatalf("Redis first cursor error = %v", err)
	}
	second, err := coordinatorB.Next(ctx, cursorKey, 2, time.Second)
	if err != nil || first != 0 || second != 1 {
		t.Fatalf("Redis shared cursor = %d/%d, err=%v, want 0/1", first, second, err)
	}
	affinityKey := "integration-affinity:" + uuid.NewString()
	credentialID := uuid.New()
	if err := coordinator.SetAffinity(ctx, affinityKey, credentialID, time.Second); err != nil {
		t.Fatalf("Redis SetAffinity() error = %v", err)
	}
	got, found, err := coordinatorB.GetAffinity(ctx, affinityKey)
	if err != nil || !found || got != credentialID {
		t.Fatalf("Redis shared affinity = %s/%v, err=%v, want %s/true", got, found, err, credentialID)
	}
}

type schedulerFixture struct {
	store       *data.Store
	resolution  route.Resolution
	providerID  uuid.UUID
	poolID      uuid.UUID
	credentials []uuid.UUID
}

func (f *schedulerFixture) service(t *testing.T, coordinator Coordinator) *Service {
	t.Helper()
	repository, err := NewRepository(f.store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, coordinator, Options{LeaseTTL: time.Minute, AffinityTTL: time.Minute, CursorTTL: time.Minute})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func newSchedulerFixture(t *testing.T, credentialCount int) *schedulerFixture {
	t.Helper()
	store := schedulerTestStore(t)
	ctx := context.Background()
	actorID, providerID, modelID, providerModelID, poolID, credentials := seedSchedulerGraph(t, store, credentialCount)
	routeRepository, err := route.NewRepository(store)
	if err != nil {
		t.Fatalf("route.NewRepository() error = %v", err)
	}
	routeService, err := route.NewService(routeRepository)
	if err != nil {
		t.Fatalf("route.NewService() error = %v", err)
	}
	created, err := routeService.Create(ctx, actorID, route.CreateInput{
		ModelID:    modelID,
		MatchValue: "scheduler-model",
		Enabled:    true,
		Targets:    []route.TargetInput{{ProviderModelID: providerModelID, CredentialPoolID: poolID, Enabled: true}},
	}, "scheduler-route-create")
	if err != nil {
		t.Fatalf("route Create() error = %v", err)
	}
	resolution, err := routeService.Resolve(ctx, "scheduler-model")
	if err != nil {
		t.Fatalf("route Resolve() error = %v", err)
	}
	if created.ActiveVersion == nil || resolution.Version.ID != created.ActiveVersion.ID {
		t.Fatalf("fixture route version mismatch: created=%v resolved=%v", created.ActiveVersion, resolution.Version.ID)
	}
	return &schedulerFixture{store: store, resolution: resolution, providerID: providerID, poolID: poolID, credentials: credentials}
}

func schedulerTestStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_scheduler_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open scheduler test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create scheduler test schema: %v", err)
	}
	pool.Close()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	databaseURL := parsed.String()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		cleanupPool, err := pgxpool.New(cleanupCtx, baseURL)
		if err != nil {
			t.Errorf("open scheduler cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop scheduler test schema: %v", err)
		}
	})
	if err := data.Migrate(ctx, databaseURL, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 8})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func seedSchedulerGraph(t *testing.T, store *data.Store, credentialCount int) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()
	actorID := uuid.New()
	providerID := uuid.New()
	modelID := uuid.New()
	providerModelID := uuid.New()
	poolID := uuid.New()
	credentials := make([]uuid.UUID, credentialCount)
	for index := range credentials {
		credentials[index] = uuid.New()
	}
	ctx := context.Background()
	err := store.WithTx(ctx, func(q data.Querier) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO users (id, email_normalized, password_hash, password_params_version) VALUES ($1, $2, 'hash', 'test')`, []any{actorID, actorID.String() + "@example.test"}},
			{`INSERT INTO models (id, public_model_id, display_name, capabilities) VALUES ($1, 'scheduler-model', 'Scheduler Model', '{"chat":true,"stream":true}')`, []any{modelID}},
			{`INSERT INTO providers (id, slug, display_name, resource_type, commercial_allowed, enabled) VALUES ($1, 'scheduler-provider', 'Scheduler Provider', 'official_api', true, true)`, []any{providerID}},
			{`INSERT INTO provider_models (id, provider_id, model_id, upstream_model_id, protocol, capabilities, enabled, review_status) VALUES ($1, $2, $3, 'upstream-scheduler', 'openai_chat', '{"chat":true,"stream":true}', true, 'approved')`, []any{providerModelID, providerID, modelID}},
			{`INSERT INTO credential_pools (id, name, provider_id, enabled) VALUES ($1, 'scheduler-pool', $2, true)`, []any{poolID, providerID}},
		}
		for _, statement := range statements {
			if _, err := q.Exec(ctx, statement.sql, statement.args...); err != nil {
				return err
			}
		}
		for _, credentialID := range credentials {
			if _, err := q.Exec(ctx, `INSERT INTO credentials (id, provider_id, external_stable_id, source_kind, status, max_concurrency) VALUES ($1, $2, $3, 'api_key', 'active', 1)`, credentialID, providerID, "scheduler-credential-"+credentialID.String()); err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `INSERT INTO pool_members (pool_id, credential_id, priority, weight, enabled) VALUES ($1, $2, 0, 1, true)`, poolID, credentialID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed scheduler graph: %v", err)
	}
	return actorID, providerID, modelID, providerModelID, poolID, credentials
}

func hasCandidateReason(decision Decision, credentialID uuid.UUID, reason string) bool {
	for _, candidate := range decision.Candidates {
		if candidate.CredentialID != nil && *candidate.CredentialID == credentialID {
			for _, candidateReason := range candidate.Reasons {
				if candidateReason == reason {
					return true
				}
			}
		}
	}
	return false
}

func hasFallbackReason(decision Decision, reason string) bool {
	for _, item := range decision.FallbackChain {
		if item.Reason == reason {
			return true
		}
	}
	return false
}

func isPostgresCode(err error, code string) bool {
	if err == nil {
		return false
	}
	var databaseError interface{ SQLState() string }
	if errors.As(err, &databaseError) {
		return databaseError.SQLState() == code
	}
	return false
}
