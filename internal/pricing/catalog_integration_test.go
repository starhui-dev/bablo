package pricing_test

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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	catalogmodel "github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/provider"
	"github.com/starhui-dev/bablo/migrations"
)

func TestCatalogDiscoveryReviewAndVersionedPricing(t *testing.T) {
	ctx := context.Background()
	store := catalogStore(t)
	actorID := uuid.New()
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO users (id, email_normalized, password_hash, password_params_version)
		VALUES ($1, $2, 'not-used', 'test')`, actorID, actorID.String()+"@example.test"); err != nil {
		t.Fatalf("seed catalog actor: %v", err)
	}

	modelRepository, err := catalogmodel.NewRepository(store)
	if err != nil {
		t.Fatalf("model.NewRepository() error = %v", err)
	}
	modelService, err := catalogmodel.NewService(modelRepository)
	if err != nil {
		t.Fatalf("model.NewService() error = %v", err)
	}
	publicModel, err := modelService.Create(ctx, actorID, catalogmodel.CreateInput{
		PublicID:     "bablo-chat",
		Aliases:      []string{"bablo-latest"},
		DisplayName:  "Bablo Chat",
		Visibility:   catalogmodel.VisibilityPublic,
		BillingClass: catalogmodel.BillingToken,
		Capabilities: catalogmodel.Capabilities{Chat: true, Stream: true, Tools: true},
		Enabled:      true,
	}, "catalog-model-create")
	if err != nil {
		t.Fatalf("model Create() error = %v", err)
	}
	resolvedAlias, err := modelService.ResolvePublic(ctx, "BABLO-LATEST")
	if err != nil || resolvedAlias.ID != publicModel.ID {
		t.Fatalf("ResolvePublic(alias) = %+v, %v", resolvedAlias, err)
	}
	promotedID := "bablo-latest"
	publicModel, err = modelService.Update(ctx, actorID, publicModel.ID, catalogmodel.UpdateInput{PublicID: &promotedID}, "catalog-model-promote")
	if err != nil {
		t.Fatalf("promote alias to canonical ID: %v", err)
	}
	if publicModel.PublicID != promotedID || len(publicModel.Aliases) != 0 {
		t.Fatalf("promoted model = %+v", publicModel)
	}
	if _, err := modelService.Create(ctx, actorID, catalogmodel.CreateInput{
		PublicID:     "BABLO-LATEST",
		DisplayName:  "Duplicate",
		Visibility:   catalogmodel.VisibilityPublic,
		BillingClass: catalogmodel.BillingToken,
		Capabilities: catalogmodel.Capabilities{Chat: true},
		Enabled:      true,
	}, "catalog-model-conflict"); !errors.Is(err, catalogmodel.ErrConflict) {
		t.Fatalf("case-insensitive identifier conflict = %v, want ErrConflict", err)
	}

	providerRepository, err := provider.NewRepository(store)
	if err != nil {
		t.Fatalf("provider.NewRepository() error = %v", err)
	}
	providerService, err := provider.NewService(providerRepository)
	if err != nil {
		t.Fatalf("provider.NewService() error = %v", err)
	}
	upstream, err := providerService.Create(ctx, actorID, provider.CreateInput{
		Slug:              "official-example",
		DisplayName:       "Official Example",
		ResourceType:      provider.ResourceOfficialAPI,
		CommercialAllowed: true,
		Enabled:           true,
	}, "provider-create")
	if err != nil {
		t.Fatalf("provider Create() error = %v", err)
	}
	if _, err := providerService.Create(ctx, actorID, provider.CreateInput{
		Slug:              "consumer-subscription",
		DisplayName:       "Consumer Subscription",
		ResourceType:      provider.ResourceSubscription,
		CommercialAllowed: true,
		Enabled:           true,
	}, "provider-subscription"); !errors.Is(err, provider.ErrInvalidInput) {
		t.Fatalf("commercial subscription error = %v, want ErrInvalidInput", err)
	}

	discoveryCapabilities := catalogmodel.Capabilities{Chat: true, Stream: true, Tools: true}
	firstDiscovery, err := providerService.Reconcile(ctx, actorID, upstream.ID, []provider.Discovery{
		{UpstreamModelID: "upstream-chat", Protocol: provider.ProtocolOpenAIChat, Capabilities: discoveryCapabilities},
		{UpstreamModelID: "upstream-old", Protocol: provider.ProtocolOpenAIChat, Capabilities: catalogmodel.Capabilities{Chat: true}},
	}, "provider-discovery-first")
	if err != nil {
		t.Fatalf("provider Reconcile() error = %v", err)
	}
	if firstDiscovery.Discovered != 2 || firstDiscovery.Observed != 2 || firstDiscovery.Missing != 0 {
		t.Fatalf("first discovery result = %+v", firstDiscovery)
	}
	providerModels, err := providerService.ListModels(ctx, upstream.ID, "", 10)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(providerModels.Models) != 2 {
		t.Fatalf("provider model count = %d, want 2", len(providerModels.Models))
	}
	var discovered, disappearing provider.ProviderModel
	for _, value := range providerModels.Models {
		switch value.UpstreamModelID {
		case "upstream-chat":
			discovered = value
		case "upstream-old":
			disappearing = value
		}
	}
	if discovered.ReviewStatus != provider.ReviewPending || discovered.Enabled || discovered.ModelID != nil {
		t.Fatalf("new discovery bypassed review: %+v", discovered)
	}

	reviewApproved := provider.ReviewApproved
	enabled := true
	modelID := publicModel.ID
	discovered, err = providerService.UpdateModel(ctx, actorID, discovered.ID, provider.UpdateModelInput{
		ModelID:      provider.OptionalUUID{Set: true, Value: &modelID},
		ReviewStatus: &reviewApproved,
		Enabled:      &enabled,
	}, "provider-discovery-approve")
	if err != nil {
		t.Fatalf("approve discovered provider model: %v", err)
	}
	unsupported := catalogmodel.Capabilities{Chat: true, Reasoning: true}
	if _, err := providerService.UpdateModel(ctx, actorID, discovered.ID, provider.UpdateModelInput{Capabilities: &unsupported}, "provider-capability-overreach"); !errors.Is(err, provider.ErrInvalidInput) {
		t.Fatalf("provider capability overreach error = %v, want ErrInvalidInput", err)
	}
	narrowed := catalogmodel.Capabilities{Chat: true}
	if _, err := modelService.Update(ctx, actorID, publicModel.ID, catalogmodel.UpdateInput{Capabilities: &narrowed}, "catalog-model-capability-narrow"); !errors.Is(err, catalogmodel.ErrInvalidInput) {
		t.Fatalf("public capability narrowing error = %v, want ErrInvalidInput", err)
	}
	unchanged, err := modelService.Get(ctx, publicModel.ID)
	if err != nil {
		t.Fatalf("get model after rejected capability narrowing: %v", err)
	}
	if !unchanged.Capabilities.Stream || !unchanged.Capabilities.Tools {
		t.Fatalf("rejected capability narrowing mutated public model: %+v", unchanged.Capabilities)
	}

	secondDiscovery, err := providerService.Reconcile(ctx, actorID, upstream.ID, []provider.Discovery{
		{UpstreamModelID: "upstream-chat", Protocol: provider.ProtocolOpenAIChat, Capabilities: catalogmodel.Capabilities{Chat: true}},
	}, "provider-discovery-second")
	if err != nil {
		t.Fatalf("second provider Reconcile() error = %v", err)
	}
	if secondDiscovery.Discovered != 0 || secondDiscovery.Missing != 1 {
		t.Fatalf("second discovery result = %+v", secondDiscovery)
	}
	discovered, err = providerService.GetModel(ctx, discovered.ID)
	if err != nil {
		t.Fatalf("GetModel(approved) error = %v", err)
	}
	if !discovered.Enabled || discovered.ReviewStatus != provider.ReviewApproved || !discovered.Capabilities.Stream || !discovered.Capabilities.Tools {
		t.Fatalf("reconcile overwrote approved business config: %+v", discovered)
	}
	disappearing, err = providerService.GetModel(ctx, disappearing.ID)
	if err != nil {
		t.Fatalf("GetModel(missing) error = %v", err)
	}
	if disappearing.DiscoveryStatus != provider.DiscoveryMissing || disappearing.ReviewStatus != provider.ReviewPending {
		t.Fatalf("missing discovery state = %+v", disappearing)
	}
	reviewRejected := provider.ReviewRejected
	disappearing, err = providerService.UpdateModel(ctx, actorID, disappearing.ID, provider.UpdateModelInput{
		ReviewStatus: &reviewRejected,
		Enabled:      &enabled,
	}, "provider-discovery-reject")
	if err != nil {
		t.Fatalf("reject discovered provider model: %v", err)
	}
	if disappearing.ReviewStatus != provider.ReviewRejected || disappearing.Enabled {
		t.Fatalf("rejected discovery remained enabled: %+v", disappearing)
	}

	pricingRepository, err := pricing.NewRepository(store)
	if err != nil {
		t.Fatalf("pricing.NewRepository() error = %v", err)
	}
	pricingService, err := pricing.NewService(pricingRepository)
	if err != nil {
		t.Fatalf("pricing.NewService() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := pricingService.ResolveSnapshot(ctx, publicModel.ID, &discovered.ID, now); !errors.Is(err, pricing.ErrPriceMissing) {
		t.Fatalf("unpriced resolution error = %v, want ErrPriceMissing", err)
	}
	firstVersion, err := pricingService.Create(ctx, actorID, pricing.CreateInput{
		Scope:         pricing.ScopeProviderModel,
		Currency:      "USD",
		EffectiveFrom: now.Add(-time.Hour),
		Prices: []pricing.EntryInput{
			{ProviderModelID: &discovered.ID, Dimension: pricing.DimensionInputToken, UnitPrice: "0.000002000000"},
			{ProviderModelID: &discovered.ID, Dimension: pricing.DimensionOutputToken, UnitPrice: "0.000008"},
		},
	}, "price-create-first")
	if err != nil {
		t.Fatalf("pricing Create() error = %v", err)
	}
	firstVersion, err = pricingService.Activate(ctx, actorID, firstVersion.ID, "price-activate-first")
	if err != nil {
		t.Fatalf("pricing Activate() error = %v", err)
	}
	snapshot, err := pricingService.ResolveSnapshot(ctx, publicModel.ID, &discovered.ID, now)
	if err != nil {
		t.Fatalf("ResolveSnapshot() error = %v", err)
	}
	if snapshot.VersionID != firstVersion.ID || snapshot.Scope != pricing.ScopeProviderModel || snapshot.Prices[pricing.DimensionInputToken] != "0.000002" {
		t.Fatalf("resolved price snapshot = %+v", snapshot)
	}

	if _, err := store.Queryer().Exec(ctx, `
		UPDATE model_prices SET unit_price = 9 WHERE price_version_id = $1`, firstVersion.ID); postgresCode(err) != "55000" {
		t.Fatalf("published price mutation code = %q, error = %v", postgresCode(err), err)
	}
	overlap, err := pricingService.Create(ctx, actorID, pricing.CreateInput{
		Scope:         pricing.ScopeProviderModel,
		Currency:      "USD",
		EffectiveFrom: now,
		Prices: []pricing.EntryInput{
			{ProviderModelID: &discovered.ID, Dimension: pricing.DimensionInputToken, UnitPrice: "0.000003"},
			{ProviderModelID: &discovered.ID, Dimension: pricing.DimensionOutputToken, UnitPrice: "0.000009"},
		},
	}, "price-create-overlap")
	if err != nil {
		t.Fatalf("create overlapping draft: %v", err)
	}
	if _, err := pricingService.Activate(ctx, actorID, overlap.ID, "price-activate-overlap"); !errors.Is(err, pricing.ErrConflict) {
		t.Fatalf("overlap activation error = %v, want ErrConflict", err)
	}

	cutover := now.Add(time.Hour)
	firstVersion, err = pricingService.Retire(ctx, actorID, firstVersion.ID, cutover, "price-retire-first")
	if err != nil {
		t.Fatalf("pricing Retire() error = %v", err)
	}
	if firstVersion.Status != pricing.StatusRetired || firstVersion.EffectiveTo == nil || !firstVersion.EffectiveTo.Equal(cutover) {
		t.Fatalf("retired price version = %+v", firstVersion)
	}
	historical, err := pricingService.ResolveSnapshot(ctx, publicModel.ID, &discovered.ID, now)
	if err != nil || historical.VersionID != firstVersion.ID {
		t.Fatalf("historical price resolution = %+v, %v", historical, err)
	}
	replacement, err := pricingService.Create(ctx, actorID, pricing.CreateInput{
		Scope:         pricing.ScopeProviderModel,
		Currency:      "USD",
		EffectiveFrom: cutover,
		Prices: []pricing.EntryInput{
			{ProviderModelID: &discovered.ID, Dimension: pricing.DimensionInputToken, UnitPrice: "0.000003"},
			{ProviderModelID: &discovered.ID, Dimension: pricing.DimensionOutputToken, UnitPrice: "0.000009"},
		},
	}, "price-create-replacement")
	if err != nil {
		t.Fatalf("create replacement price: %v", err)
	}
	replacement, err = pricingService.Activate(ctx, actorID, replacement.ID, "price-activate-replacement")
	if err != nil {
		t.Fatalf("activate replacement price: %v", err)
	}
	current, err := pricingService.ResolveSnapshot(ctx, publicModel.ID, &discovered.ID, cutover.Add(time.Second))
	if err != nil || current.VersionID != replacement.ID || current.Prices[pricing.DimensionOutputToken] != "0.000009" {
		t.Fatalf("replacement price resolution = %+v, %v", current, err)
	}

	var auditCount int
	if err := store.Queryer().QueryRow(ctx, `
		SELECT count(*) FROM audit_logs
		WHERE actor_user_id = $1
		  AND action IN ('model.create', 'provider.create', 'provider_model.reconcile', 'provider_model.update', 'price_version.activate')`, actorID).Scan(&auditCount); err != nil {
		t.Fatalf("query catalog audit rows: %v", err)
	}
	if auditCount < 7 {
		t.Fatalf("catalog audit count = %d, want at least 7", auditCount)
	}
}

func catalogStore(t *testing.T) *data.Store {
	t.Helper()
	databaseURL := isolatedCatalogDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := data.Migrate(ctx, databaseURL, migrations.Files, logger); err != nil {
		t.Fatalf("data.Migrate() error = %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 4})
	if err != nil {
		t.Fatalf("data.Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func isolatedCatalogDatabaseURL(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_catalog_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open catalog test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create catalog test schema: %v", err)
	}
	pool.Close()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanupPool, err := pgxpool.New(cleanupCtx, baseURL)
		if err != nil {
			t.Errorf("open catalog cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop catalog test schema: %v", err)
		}
	})
	return parsed.String()
}

func postgresCode(err error) string {
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		return databaseError.Code
	}
	return ""
}
