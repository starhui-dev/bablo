package quota

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/migrations"
)

func TestRepositoryPersistObservationIsIdempotentAndDetectsConflict(t *testing.T) {
	store := quotaIntegrationStore(t)
	ctx := context.Background()
	providerID := uuid.New()
	credentialID := uuid.New()
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO providers (id, slug, display_name, resource_type, commercial_allowed, enabled)
		VALUES ($1, 'quota-provider', 'Quota Provider', 'official_api', true, true)`, providerID); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO credentials (id, provider_id, external_stable_id, source_kind, status, max_concurrency)
		VALUES ($1, $2, 'quota-credential', 'api_key', 'active', 1)`, credentialID, providerID); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	used := int64(12)
	observation := Observation{
		ObservedAt:     observedAt,
		Source:         "integration",
		Confidence:     ConfidenceHigh,
		ObservationKey: "request-integration-1",
		Metadata:       map[string]string{"source": "test"},
		Windows: []Window{{
			Kind:       WindowMinute,
			UsedTokens: &used,
			Metadata:   map[string]string{"window": "minute"},
		}},
	}
	request := ProbeRequest{CredentialID: credentialID, ProviderSlug: "quota-provider", Model: "quota-model"}
	state := ProbeState{
		CredentialID:   credentialID,
		ProviderSlug:   "quota-provider",
		ProbeName:      "integration",
		Status:         ProbeStatusSuccess,
		LastAttemptAt:  &observedAt,
		LastObservedAt: &observedAt,
		UpdatedAt:      observedAt,
	}
	if err := repository.PersistObservation(ctx, request, observation, state); err != nil {
		t.Fatalf("first PersistObservation() error = %v", err)
	}
	if err := repository.PersistObservation(ctx, request, observation, state); err != nil {
		t.Fatalf("idempotent PersistObservation() error = %v", err)
	}
	var count int
	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FROM quota_snapshots WHERE credential_id = $1`, credentialID).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if count != 1 {
		t.Fatalf("snapshot count = %d, want 1", count)
	}
	changed := observation
	changed.Windows = []Window{{Kind: WindowMinute, UsedTokens: int64Pointer(13), Metadata: map[string]string{"window": "minute"}}}
	if err := repository.PersistObservation(ctx, request, changed, state); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting PersistObservation() error = %v, want ErrConflict", err)
	}
	if err := store.Queryer().QueryRow(ctx, `SELECT used_tokens FROM quota_snapshots WHERE credential_id = $1`, credentialID).Scan(&used); err != nil {
		t.Fatalf("read snapshot after conflict: %v", err)
	}
	if used != 12 {
		t.Fatalf("snapshot used_tokens = %d, want unchanged 12", used)
	}
}

func TestRepositoryConcurrentObservationAndStateOrdering(t *testing.T) {
	store := quotaIntegrationStore(t)
	ctx := context.Background()
	providerID := uuid.New()
	credentialID := uuid.New()
	if _, err := store.Queryer().Exec(ctx, `INSERT INTO providers (id, slug, display_name, resource_type, commercial_allowed, enabled) VALUES ($1, 'quota-concurrent', 'Quota Concurrent', 'official_api', true, true)`, providerID); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if _, err := store.Queryer().Exec(ctx, `INSERT INTO credentials (id, provider_id, external_stable_id, source_kind, status, max_concurrency) VALUES ($1, $2, 'quota-concurrent-credential', 'api_key', 'active', 1)`, credentialID, providerID); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	observation := Observation{ObservedAt: now, Source: "concurrent", Confidence: ConfidenceMedium, ObservationKey: "concurrent-observation", Windows: []Window{{Kind: WindowHour, RemainingTokens: int64Pointer(100)}}}
	request := ProbeRequest{CredentialID: credentialID, ProviderSlug: "quota-concurrent", Model: "quota-model"}
	state := ProbeState{CredentialID: credentialID, ProviderSlug: request.ProviderSlug, ProbeName: "concurrent", Status: ProbeStatusSuccess, UpdatedAt: now}
	const workers = 12
	errorsCh := make(chan error, workers)
	for range workers {
		go func() { errorsCh <- repository.PersistObservation(ctx, request, observation, state) }()
	}
	for range workers {
		if err := <-errorsCh; err != nil {
			t.Fatalf("concurrent PersistObservation() error = %v", err)
		}
	}
	var count int
	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FROM quota_snapshots WHERE credential_id = $1 AND observation_key = $2`, credentialID, observation.ObservationKey).Scan(&count); err != nil {
		t.Fatalf("count concurrent snapshots: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent snapshot count = %d, want 1", count)
	}
	future := now.Add(time.Minute)
	newer := state
	newer.Status = ProbeStatusError
	newer.UpdatedAt = future
	newer.LastErrorClass = "server"
	if err := repository.UpsertProbeState(ctx, newer); err != nil {
		t.Fatalf("newer state upsert: %v", err)
	}
	older := state
	older.Status = ProbeStatusNoObservation
	older.UpdatedAt = now.Add(-time.Minute)
	if err := repository.UpsertProbeState(ctx, older); err != nil {
		t.Fatalf("older state upsert: %v", err)
	}
	stored, found, err := repository.GetProbeState(ctx, credentialID)
	if err != nil || !found {
		t.Fatalf("GetProbeState() = %+v, found=%v, err=%v", stored, found, err)
	}
	if stored.Status != ProbeStatusError || stored.LastErrorClass != "server" || !stored.UpdatedAt.Equal(future) {
		t.Fatalf("late state overwrote newer state: %+v", stored)
	}
}

func quotaIntegrationStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	schema := "bablo_quota_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open migration database: %v", err)
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
	if err := data.Migrate(ctx, databaseURL, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate quota schema: %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 4})
	if err != nil {
		t.Fatalf("open quota store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, cleanupErr := pgxpool.New(cleanupCtx, baseURL)
		if cleanupErr != nil {
			t.Errorf("open quota cleanup database: %v", cleanupErr)
			return
		}
		defer cleanup.Close()
		if _, cleanupErr := cleanup.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); cleanupErr != nil {
			t.Errorf("drop quota test schema: %v", cleanupErr)
		}
	})
	return store
}

func int64Pointer(value int64) *int64 { return &value }
