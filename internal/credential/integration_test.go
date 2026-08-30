package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/secret"
	"github.com/starhui-dev/bablo/migrations"
)

func TestCredentialLifecycleEncryptsRotatesAndReencrypts(t *testing.T) {
	store := credentialTestStore(t)
	ctx := context.Background()
	actorID, providerID := seedCredentialOwner(t, store)
	keyring, err := secret.NewKeyring("v1", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatalf("secret.NewKeyring() error = %v", err)
	}
	repository, err := NewRepository(store, keyring)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, keyring)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	created, err := service.Create(ctx, actorID, CreateInput{
		ProviderID:       providerID,
		ExternalStableID: "oauth-account-1",
		SourceKind:       SourceOAuth,
		Region:           "us-east-1",
		Metadata:         map[string]string{"account_email": "owner@example.test"},
		Secrets: []SecretInput{
			{Kind: SecretOAuthAccess, Value: "access-token-v1"},
			{Kind: SecretOAuthRefresh, Value: "refresh-token-v1"},
		},
		Enabled: true,
	}, "credential-create")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal metadata response: %v", err)
	}
	if strings.Contains(string(encoded), "access-token-v1") || strings.Contains(string(encoded), "refresh-token-v1") {
		t.Fatalf("credential response leaked secret: %s", encoded)
	}
	var ciphertext []byte
	if err := store.Queryer().QueryRow(ctx, `SELECT ciphertext FROM credential_secrets WHERE credential_id = $1 AND secret_kind = $2 AND rotated_at IS NULL`, created.ID, SecretOAuthAccess).Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(ciphertext, []byte("access-token-v1")) {
		t.Fatal("credential ciphertext contains plaintext")
	}

	runtime, err := service.Reveal(ctx, created.ID, SecretOAuthAccess)
	if err != nil {
		t.Fatalf("Reveal() error = %v", err)
	}
	if string(runtime.Secrets[SecretOAuthAccess]) != "access-token-v1" || runtime.ProviderSlug != "credential-official" {
		t.Fatal("Reveal() returned unexpected runtime credential")
	}
	runtime.Close()
	if len(runtime.Secrets) != 0 {
		t.Fatal("RuntimeCredential.Close() did not clear secrets")
	}
	allRuntime, err := service.RevealAll(ctx, created.ID)
	if err != nil || string(allRuntime.Secrets[SecretOAuthAccess]) != "access-token-v1" || string(allRuntime.Secrets[SecretOAuthRefresh]) != "refresh-token-v1" {
		t.Fatalf("RevealAll() returned incomplete runtime credential: %v", err)
	}
	allRuntime.Close()
	var reconciledIDs []uuid.UUID
	if err := service.ForEachActive(ctx, func(value RuntimeCredential) error {
		reconciledIDs = append(reconciledIDs, value.CredentialID)
		if string(value.Secrets[SecretOAuthAccess]) != "access-token-v1" || string(value.Secrets[SecretOAuthRefresh]) != "refresh-token-v1" {
			return errors.New("runtime source returned incomplete OAuth secret set")
		}
		return nil
	}); err != nil || len(reconciledIDs) != 1 || reconciledIDs[0] != created.ID {
		t.Fatalf("ForEachActive() IDs = %v, error = %v", reconciledIDs, err)
	}

	rotated, err := service.Rotate(ctx, actorID, created.ID, SecretInput{Kind: SecretOAuthAccess, Value: "access-token-v2"}, "credential-rotate")
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	activeVersion := 0
	rotatedVersion := 0
	for _, item := range rotated.Secrets {
		if item.Kind == SecretOAuthAccess && item.RotatedAt == nil {
			activeVersion = int(item.VersionNo)
		}
		if item.Kind == SecretOAuthAccess && item.RotatedAt != nil {
			rotatedVersion = int(item.VersionNo)
		}
	}
	if activeVersion != 2 || rotatedVersion != 1 {
		t.Fatalf("secret versions after rotate: active=%d rotated=%d", activeVersion, rotatedVersion)
	}

	rotatedRuntime, err := service.Reveal(ctx, created.ID, SecretOAuthAccess)
	if err != nil || string(rotatedRuntime.Secrets[SecretOAuthAccess]) != "access-token-v2" {
		t.Fatalf("rotated Reveal() returned unexpected runtime credential: %v", err)
	}
	rotatedRuntime.Close()

	v2Keyring, err := secret.NewKeyring("v2", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatalf("v2 secret.NewKeyring() error = %v", err)
	}
	v2Repository, err := NewRepository(store, v2Keyring)
	if err != nil {
		t.Fatalf("v2 NewRepository() error = %v", err)
	}
	v2Service, err := NewService(v2Repository, v2Keyring)
	if err != nil {
		t.Fatalf("v2 NewService() error = %v", err)
	}
	reencrypted, err := v2Service.Reencrypt(ctx, actorID, created.ID, SecretOAuthAccess, "credential-reencrypt")
	if err != nil {
		t.Fatalf("Reencrypt() error = %v", err)
	}
	foundV2 := false
	for _, item := range reencrypted.Secrets {
		if item.Kind == SecretOAuthAccess && item.RotatedAt == nil && item.KeyVersion == "v2" && item.VersionNo == 3 {
			foundV2 = true
		}
	}
	if !foundV2 {
		t.Fatalf("reencrypted secret descriptor missing: %+v", reencrypted.Secrets)
	}

	if err := v2Service.RecordHealth(ctx, created.ID, HealthInput{Succeeded: false, ErrorClass: "upstream_429", CooldownUntil: timePtr(time.Now().UTC().Add(time.Minute)), ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("RecordHealth(failure) error = %v", err)
	}
	healthy, err := v2Service.Get(ctx, created.ID)
	if err != nil || healthy.Health.LastErrorClass != "upstream_429" || healthy.Health.CooldownUntil == nil {
		t.Fatalf("health after failure = %+v, %v", healthy.Health, err)
	}
	if err := v2Service.RecordHealth(ctx, created.ID, HealthInput{Succeeded: true, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("RecordHealth(success) error = %v", err)
	}
	healthy, err = v2Service.Get(ctx, created.ID)
	if err != nil || healthy.Health.CooldownUntil != nil || healthy.Health.LastSuccessAt == nil {
		t.Fatalf("health after success = %+v, %v", healthy.Health, err)
	}
	disabledStatus := StatusDisabled
	if _, err := v2Service.Update(ctx, actorID, created.ID, UpdateInput{Status: &disabledStatus}, "credential-disable"); err != nil {
		t.Fatalf("disable Credential error = %v", err)
	}
	if _, err := v2Service.Reencrypt(ctx, actorID, created.ID, SecretOAuthRefresh, "credential-reencrypt-disabled"); err != nil {
		t.Fatalf("Reencrypt(disabled) error = %v", err)
	}
	if _, err := v2Service.RevealAll(ctx, created.ID); !errors.Is(err, ErrCredentialInactive) {
		t.Fatalf("RevealAll(disabled) error = %v, want ErrCredentialInactive", err)
	}
}

func TestCredentialConcurrentRotationProducesOneActiveVersionPerKind(t *testing.T) {
	store := credentialTestStore(t)
	ctx := context.Background()
	actorID, providerID := seedCredentialOwner(t, store)
	keyring, err := secret.NewKeyring("v1", map[string][]byte{"v1": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatalf("secret.NewKeyring() error = %v", err)
	}
	repository, _ := NewRepository(store, keyring)
	service, _ := NewService(repository, keyring)
	created, err := service.Create(ctx, actorID, CreateInput{ProviderID: providerID, ExternalStableID: "api-key-account", SourceKind: SourceAPIKey, Secrets: []SecretInput{{Kind: SecretAPIKey, Value: "api-key-v1"}}, Enabled: true}, "rotation-seed")
	if err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	const rotations = 12
	const reads = 12
	errs := make(chan error, rotations+reads)
	var wg sync.WaitGroup
	for index := range rotations {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := service.Rotate(ctx, actorID, created.ID, SecretInput{Kind: SecretAPIKey, Value: "api-key-" + uuid.NewString()}, "rotation-"+uuid.NewString())
			errs <- err
		}(index)
	}
	for range reads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := service.RevealAll(ctx, created.ID)
			if err == nil && len(value.Secrets[SecretAPIKey]) == 0 {
				err = errors.New("runtime read returned an empty API key")
			}
			value.Close()
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Rotate() error = %v", err)
		}
	}
	var activeCount, versionCount int
	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FILTER (WHERE rotated_at IS NULL), count(*) FROM credential_secrets WHERE credential_id = $1 AND secret_kind = $2`, created.ID, SecretAPIKey).Scan(&activeCount, &versionCount); err != nil {
		t.Fatalf("count rotated secrets: %v", err)
	}
	if activeCount != 1 || versionCount != rotations+1 {
		t.Fatalf("rotated secret counts: active=%d versions=%d", activeCount, versionCount)
	}
}

func TestCredentialPoolProviderAndMetadataPolicy(t *testing.T) {
	store := credentialTestStore(t)
	ctx := context.Background()
	actorID, providerID := seedCredentialOwner(t, store)
	otherProviderID := uuid.New()
	if _, err := store.Queryer().Exec(ctx, `INSERT INTO providers (id, slug, display_name, resource_type) VALUES ($1, 'credential-other', 'Other', 'third_party')`, otherProviderID); err != nil {
		t.Fatalf("seed second provider: %v", err)
	}
	keyring, _ := secret.NewKeyring("v1", map[string][]byte{"v1": bytes.Repeat([]byte{4}, 32)})
	repository, _ := NewRepository(store, keyring)
	service, _ := NewService(repository, keyring)
	created, err := service.Create(ctx, actorID, CreateInput{ProviderID: providerID, ExternalStableID: "pool-account", SourceKind: SourceAPIKey, Metadata: map[string]string{"team": "gateway"}, Secrets: []SecretInput{{Kind: SecretAPIKey, Value: "pool-secret"}}, Enabled: true}, "pool-credential")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	pool, err := service.CreatePool(ctx, actorID, PoolInput{ProviderID: providerID, Name: "primary-pool", Metadata: map[string]string{"purpose": "production"}, Enabled: true}, "pool-create")
	if err != nil {
		t.Fatalf("CreatePool() error = %v", err)
	}
	if err := service.AddMember(ctx, actorID, pool.ID, MembershipInput{CredentialID: created.ID, Weight: 2, Enabled: true}, "pool-member"); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	if err := service.AddMember(ctx, actorID, pool.ID, MembershipInput{CredentialID: uuid.New(), Weight: 1, Enabled: true}, "pool-cross-provider"); err == nil {
		t.Fatal("AddMember() accepted unknown credential")
	}
	updated, err := service.Get(ctx, created.ID)
	if err != nil || len(updated.Pools) != 1 || updated.Pools[0].PoolID != pool.ID || updated.Pools[0].Weight != 2 {
		t.Fatalf("credential pool metadata = %+v, %v", updated.Pools, err)
	}
	second, err := service.Create(ctx, actorID, CreateInput{ProviderID: otherProviderID, ExternalStableID: created.ExternalStableID, SourceKind: SourceAPIKey, Secrets: []SecretInput{{Kind: SecretAPIKey, Value: "pool-secret-2"}}, Enabled: true}, "pool-credential-2")
	if err != nil {
		t.Fatalf("Create(second provider) error = %v", err)
	}
	if err := service.AddMember(ctx, actorID, pool.ID, MembershipInput{CredentialID: second.ID, Weight: 1, Enabled: true}, "pool-cross-provider"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cross-provider AddMember() error = %v, want ErrInvalidInput", err)
	}
	firstPage, err := service.List(ctx, "", 1)
	if err != nil || len(firstPage.Credentials) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("first credential page = %+v, %v", firstPage, err)
	}
	secondPage, err := service.List(ctx, firstPage.NextCursor, 10)
	if err != nil || len(secondPage.Credentials) != 1 || secondPage.Credentials[0].ID == firstPage.Credentials[0].ID || secondPage.Credentials[0].ID != second.ID {
		t.Fatalf("second credential page = %+v, %v", secondPage, err)
	}
	if _, err := service.Create(ctx, actorID, CreateInput{ProviderID: providerID, ExternalStableID: "bad-metadata", SourceKind: SourceAPIKey, Metadata: map[string]string{"api_key": "must-not-be-metadata"}, Secrets: []SecretInput{{Kind: SecretAPIKey, Value: "secret"}}, Enabled: true}, "bad-metadata"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sensitive metadata error = %v, want ErrInvalidInput", err)
	}
	if _, err := service.Create(ctx, actorID, CreateInput{ProviderID: providerID, ExternalStableID: "oauth-without-access", SourceKind: SourceOAuth, Secrets: []SecretInput{{Kind: SecretOAuthRefresh, Value: "refresh-only"}}, Enabled: true}, "oauth-without-access"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("OAuth without access token error = %v, want ErrInvalidInput", err)
	}
}

func seedCredentialOwner(t *testing.T, store *data.Store) (uuid.UUID, uuid.UUID) {
	ctx := context.Background()
	actorID := uuid.New()
	providerID := uuid.New()
	if _, err := store.Queryer().Exec(ctx, `INSERT INTO users (id, email_normalized, password_hash, password_params_version) VALUES ($1, $2, 'test', 'test')`, actorID, actorID.String()+"@example.test"); err != nil {
		t.Fatalf("seed credential actor: %v", err)
	}
	if _, err := store.Queryer().Exec(ctx, `INSERT INTO providers (id, slug, display_name, resource_type) VALUES ($1, 'credential-official', 'Official', 'official_api')`, providerID); err != nil {
		t.Fatalf("seed credential provider: %v", err)
	}
	return actorID, providerID
}

func credentialTestStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	schema := "bablo_credential_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := data.Migrate(ctx, parsed.String(), migrations.Files, nil); err != nil {
		t.Fatalf("migrate credential schema: %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: parsed.String(), MaxConns: 4})
	if err != nil {
		t.Fatalf("open credential store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanup, err := pgxpool.New(context.Background(), baseURL)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
			cleanup.Close()
		}
	})
	return store
}

func timePtr(value time.Time) *time.Time { return &value }
