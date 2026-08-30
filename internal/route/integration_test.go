package route

import (
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/migrations"
)

func TestCreateResolveAndPublishRouteSnapshots(t *testing.T) {
	store := routeTestStore(t)
	ctx := context.Background()
	actorID, modelID, providerModelA, providerModelB, poolA, poolB := seedRouteGraph(t, store)
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	created, err := service.Create(ctx, actorID, CreateInput{
		ModelID:    modelID,
		MatchValue: "catalog-latest",
		Enabled:    true,
		Targets: []TargetInput{
			{ProviderModelID: providerModelA, CredentialPoolID: poolA, Priority: 1, Weight: 2, Enabled: true},
			{ProviderModelID: providerModelB, CredentialPoolID: poolB, Priority: 2, Weight: 1, Enabled: false},
		},
	}, "route-create")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ActiveVersion == nil || created.ActiveVersion.VersionNo != 1 || len(created.ActiveVersion.Targets) != 2 {
		t.Fatalf("created route snapshot = %+v", created.ActiveVersion)
	}
	if created.ActiveVersion.Targets[0].UpstreamModelID != "upstream-a" || created.ActiveVersion.Targets[0].ProviderID == uuid.Nil {
		t.Fatalf("created target resolution = %+v", created.ActiveVersion.Targets[0])
	}

	resolved, err := service.Resolve(ctx, "CATALOG-LATEST")
	if err != nil {
		t.Fatalf("Resolve(alias) error = %v", err)
	}
	if resolved.RequestedModel != "catalog-latest" || resolved.ModelID != modelID || resolved.Version.VersionNo != 1 || len(resolved.Candidates) != 2 {
		t.Fatalf("resolved route = %+v", resolved)
	}

	published, err := service.PublishVersion(ctx, actorID, created.ID, PublishInput{Targets: []TargetInput{
		{ProviderModelID: providerModelB, CredentialPoolID: poolB, Priority: 0, Weight: 1, Enabled: true},
	}}, "route-publish")
	if err != nil {
		t.Fatalf("PublishVersion() error = %v", err)
	}
	if published.ActiveVersion == nil || published.ActiveVersion.VersionNo != 2 || len(published.ActiveVersion.Targets) != 1 || published.ActiveVersion.Targets[0].UpstreamModelID != "upstream-b" {
		t.Fatalf("published route snapshot = %+v", published.ActiveVersion)
	}
	versions, err := service.ListVersions(ctx, created.ID)
	if err != nil {
		t.Fatalf("ListVersions() error = %v", err)
	}
	if len(versions) != 2 || versions[0].EffectiveTo != nil || versions[1].EffectiveTo == nil {
		t.Fatalf("version history = %+v", versions)
	}

	resolved, err = service.Resolve(ctx, "catalog-latest")
	if err != nil {
		t.Fatalf("Resolve(new version) error = %v", err)
	}
	if resolved.Version.VersionNo != 2 || resolved.Candidates[0].UpstreamModelID != "upstream-b" {
		t.Fatalf("resolved new version = %+v", resolved)
	}

	if _, err := service.PublishVersion(ctx, actorID, created.ID, PublishInput{Targets: []TargetInput{{
		ProviderModelID: providerModelA, CredentialPoolID: poolB, Enabled: true,
	}}}, "route-invalid-target"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cross-provider target error = %v, want ErrInvalidInput", err)
	}
	if _, err := service.Update(ctx, actorID, created.ID, UpdateInput{Enabled: boolPointer(false)}, "route-disable"); err != nil {
		t.Fatalf("Update(disable) error = %v", err)
	}
	if _, err := service.Resolve(ctx, "catalog-latest"); !errors.Is(err, ErrRouteDisabled) {
		t.Fatalf("disabled Resolve() error = %v, want ErrRouteDisabled", err)
	}
}

func TestConcurrentRoutePublishAllocatesOneActiveVersionPerNumber(t *testing.T) {
	store := routeTestStore(t)
	ctx := context.Background()
	actorID, modelID, providerModelA, providerModelB, poolA, poolB := seedRouteGraph(t, store)
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	created, err := service.Create(ctx, actorID, CreateInput{
		ModelID:    modelID,
		MatchValue: "catalog-latest",
		Targets:    []TargetInput{{ProviderModelID: providerModelA, CredentialPoolID: poolA, Enabled: true}},
	}, "route-concurrent-create")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	inputs := []PublishInput{
		{Targets: []TargetInput{{ProviderModelID: providerModelA, CredentialPoolID: poolA, Enabled: true}}},
		{Targets: []TargetInput{{ProviderModelID: providerModelB, CredentialPoolID: poolB, Enabled: true}}},
	}
	errs := make(chan error, len(inputs))
	var waitGroup sync.WaitGroup
	for _, input := range inputs {
		waitGroup.Add(1)
		go func(input PublishInput) {
			defer waitGroup.Done()
			_, err := service.PublishVersion(ctx, actorID, created.ID, input, "route-concurrent-publish")
			errs <- err
		}(input)
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent PublishVersion() error = %v", err)
		}
	}
	versions, err := service.ListVersions(ctx, created.ID)
	if err != nil {
		t.Fatalf("ListVersions() error = %v", err)
	}
	if len(versions) != 3 || versions[0].VersionNo != 3 || versions[1].VersionNo != 2 || versions[2].VersionNo != 1 {
		t.Fatalf("concurrent version history = %+v", versions)
	}
	active := 0
	for _, version := range versions {
		if version.EffectiveTo == nil {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active version count = %d, want 1", active)
	}
}

func routeTestStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_route_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open route test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create route test schema: %v", err)
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
			t.Errorf("open route cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop route test schema: %v", err)
		}
	})
	if err := data.Migrate(ctx, databaseURL, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 6})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func seedRouteGraph(t *testing.T, store *data.Store) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	actorID := uuid.New()
	modelID := uuid.New()
	providerA := uuid.New()
	providerB := uuid.New()
	providerModelA := uuid.New()
	providerModelB := uuid.New()
	poolA := uuid.New()
	poolB := uuid.New()
	ctx := context.Background()
	err := store.WithTx(ctx, func(q data.Querier) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO users (id, email_normalized, password_hash, password_params_version) VALUES ($1, $2, 'hash', 'test')`, []any{actorID, actorID.String() + "@example.test"}},
			{`INSERT INTO models (id, public_model_id, display_name, capabilities) VALUES ($1, 'catalog-model', 'Catalog Model', '{"chat":true,"stream":true}')`, []any{modelID}},
			{`INSERT INTO model_aliases (id, model_id, alias) VALUES ($1, $2, 'catalog-latest')`, []any{uuid.New(), modelID}},
			{`INSERT INTO providers (id, slug, display_name, resource_type, commercial_allowed) VALUES ($1, 'provider-a', 'Provider A', 'official_api', true), ($2, 'provider-b', 'Provider B', 'official_api', true)`, []any{providerA, providerB}},
			{`INSERT INTO provider_models (id, provider_id, model_id, upstream_model_id, protocol, capabilities, enabled, review_status) VALUES ($1, $2, $3, 'upstream-a', 'openai_chat', '{"chat":true,"stream":true}', true, 'approved'), ($4, $5, $3, 'upstream-b', 'openai_chat', '{"chat":true,"stream":true}', true, 'approved')`, []any{providerModelA, providerA, modelID, providerModelB, providerB}},
			{`INSERT INTO credential_pools (id, name, provider_id, enabled) VALUES ($1, 'pool-a', $2, true), ($3, 'pool-b', $4, true)`, []any{poolA, providerA, poolB, providerB}},
		}
		for _, statement := range statements {
			if _, err := q.Exec(ctx, statement.sql, statement.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed route graph: %v", err)
	}
	return actorID, modelID, providerModelA, providerModelB, poolA, poolB
}

func boolPointer(value bool) *bool {
	return &value
}
