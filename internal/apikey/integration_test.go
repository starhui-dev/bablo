package apikey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/migrations"
)

type apiKeyTestEnvironment struct {
	service      *Service
	store        *data.Store
	databaseURL  string
	now          time.Time
	userA        uuid.UUID
	userB        uuid.UUID
	modelA       uuid.UUID
	modelB       uuid.UUID
	modelC       uuid.UUID
	modelPrivate uuid.UUID
}

func TestAPIKeyLifecycleOwnershipAndMultiModelAuthorization(t *testing.T) {
	environment := newAPIKeyTestEnvironment(t)
	rpm := int64(20)
	tpm := int64(1_000)
	daily := int64(5_000)
	expiresAt := environment.now.Add(time.Hour)
	created, err := environment.service.Create(context.Background(), environment.userA, CreateInput{
		Name:               "primary",
		ExpiresAt:          &expiresAt,
		AllowedModels:      []string{"model-b", "model-a", "model-a"},
		IPAllowlist:        []string{"203.0.113.9", "::ffff:203.0.113.0/120"},
		RPMLimit:           &rpm,
		TPMLimit:           &tpm,
		DailyBudgetMinor:   &daily,
		MonthlyBudgetMinor: nil,
	}, "request-create")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !strings.HasPrefix(created.Secret, secretMarker) || len(created.Secret) != len(secretMarker)+secretEncodedBytes {
		t.Fatalf("created secret format is invalid")
	}
	if created.Key.Prefix == created.Secret || !strings.HasPrefix(created.Key.Prefix, secretMarker) {
		t.Fatalf("display prefix = %q", created.Key.Prefix)
	}
	if got := strings.Join(created.Key.AllowedModels, ","); got != "model-a,model-b" {
		t.Fatalf("allowed models = %q", got)
	}
	if got := strings.Join(created.Key.IPAllowlist, ","); got != "203.0.113.0/24,203.0.113.9/32" {
		t.Fatalf("IP allowlist = %q", got)
	}

	var storedHash []byte
	var storedPrefix string
	if err := environment.store.Queryer().QueryRow(context.Background(), `
		SELECT secret_hash, key_prefix FROM api_keys WHERE id = $1`, created.Key.ID).Scan(&storedHash, &storedPrefix); err != nil {
		t.Fatalf("query stored secret material: %v", err)
	}
	expectedHash := sha256.Sum256([]byte(created.Secret))
	if !bytes.Equal(storedHash, expectedHash[:]) || storedPrefix != created.Key.Prefix {
		t.Fatalf("stored secret material did not match hash/prefix contract")
	}
	if bytes.Contains(storedHash, []byte(created.Secret)) {
		t.Fatal("database hash contains plaintext secret")
	}

	if _, err := environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("198.51.100.1")); !errors.Is(err, ErrIPDenied) {
		t.Fatalf("wrong IP error = %v, want ErrIPDenied", err)
	}
	principal, err := environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("203.0.113.20"))
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if _, err := environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("::ffff:203.0.113.20")); err != nil {
		t.Fatalf("mapped IPv4 Authenticate() error = %v", err)
	}
	if principal.APIKeyID != created.Key.ID || principal.UserID != environment.userA || principal.KeyPrefix != created.Key.Prefix {
		t.Fatalf("principal = %+v", principal)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-a", 10); err != nil {
		t.Fatalf("Authorize(model-a) error = %v", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-b", 10); err != nil {
		t.Fatalf("Authorize(model-b) error = %v", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-c", 10); !errors.Is(err, ErrModelDenied) {
		t.Fatalf("Authorize(model-c) error = %v, want ErrModelDenied", err)
	}

	assertIdentityRequest(t, environment.service, created.Secret, "model-a", http.StatusOK)
	assertIdentityRequest(t, environment.service, created.Secret, "model-b", http.StatusOK)
	assertIdentityRequest(t, environment.service, created.Secret, "model-c", http.StatusForbidden)

	updatedName := "updated primary"
	onlyModelC := []string{"model-c"}
	openIPs := []string{}
	updated, err := environment.service.Update(context.Background(), environment.userA, created.Key.ID, UpdateInput{
		Name:          &updatedName,
		ExpiresAt:     OptionalTime{Set: true},
		AllowedModels: &onlyModelC,
		IPAllowlist:   &openIPs,
		RPMLimit:      OptionalInt64{Set: true},
	}, "request-update")
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Name != updatedName || updated.ExpiresAt != nil || updated.RPMLimit != nil || len(updated.IPAllowlist) != 0 || strings.Join(updated.AllowedModels, ",") != "model-c" {
		t.Fatalf("updated key = %+v", updated)
	}
	principal, err = environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("198.51.100.1"))
	if err != nil {
		t.Fatalf("Authenticate() after IP clear error = %v", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-a", 1); !errors.Is(err, ErrModelDenied) {
		t.Fatalf("replaced model-a error = %v, want ErrModelDenied", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-c", 1); err != nil {
		t.Fatalf("replaced model-c error = %v", err)
	}
	restoredModels := []string{"model-a", "model-b"}
	if _, err := environment.service.Update(context.Background(), environment.userA, created.Key.ID, UpdateInput{
		AllowedModels: &restoredModels,
		RPMLimit:      OptionalInt64{Set: true, Value: &rpm},
	}, "request-restore"); err != nil {
		t.Fatalf("restore Update() error = %v", err)
	}
	principal, err = environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("198.51.100.1"))
	if err != nil {
		t.Fatalf("Authenticate() after restore error = %v", err)
	}
	forgedPrincipal := principal
	forgedPrincipal.UserID = environment.userB
	if err := environment.service.Authorize(context.Background(), forgedPrincipal, "model-a", 1); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("forged owner Authorize() error = %v, want ErrInvalidKey", err)
	}

	denyPolicyID := uuid.New()
	if _, err := environment.store.Queryer().Exec(context.Background(), `
		INSERT INTO policies (id, name, default_action) VALUES ($1, $2, 'deny')`,
		denyPolicyID, "deny-"+denyPolicyID.String()); err != nil {
		t.Fatalf("seed deny policy: %v", err)
	}
	if _, err := environment.store.Queryer().Exec(context.Background(), `
		INSERT INTO api_key_policies (api_key_id, policy_id) VALUES ($1, $2)`,
		created.Key.ID, denyPolicyID); err != nil {
		t.Fatalf("assign deny policy: %v", err)
	}
	if _, err := environment.store.Queryer().Exec(context.Background(), `
		INSERT INTO policy_model_entitlements (policy_id, model_id, effect) VALUES ($1, $2, 'deny')`,
		denyPolicyID, environment.modelA); err != nil {
		t.Fatalf("seed deny entitlement: %v", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-a", 1); !errors.Is(err, ErrModelDenied) {
		t.Fatalf("deny precedence error = %v, want ErrModelDenied", err)
	}

	newName := "other owner attempt"
	if _, err := environment.service.Update(context.Background(), environment.userB, created.Key.ID, UpdateInput{Name: &newName}, "request-other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner Update() error = %v, want ErrNotFound", err)
	}

	rotated, err := environment.service.Rotate(context.Background(), environment.userA, created.Key.ID, "request-rotate")
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.Secret == created.Secret || rotated.Key.SecretVersion != 2 {
		t.Fatalf("rotation = secret_equal %v, version %d", rotated.Secret == created.Secret, rotated.Key.SecretVersion)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-b", 1); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("pre-rotation Principal error = %v, want ErrInvalidKey", err)
	}
	if _, err := environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("203.0.113.20")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("old secret error = %v, want ErrInvalidKey", err)
	}
	if _, err := environment.service.Authenticate(context.Background(), rotated.Secret, netip.MustParseAddr("203.0.113.20")); err != nil {
		t.Fatalf("rotated secret Authenticate() error = %v", err)
	}

	revoked, err := environment.service.Revoke(context.Background(), environment.userA, created.Key.ID, "request-revoke")
	if err != nil || revoked.Status != "revoked" {
		t.Fatalf("Revoke() = %+v, %v", revoked, err)
	}
	if _, err := environment.service.Revoke(context.Background(), environment.userA, created.Key.ID, "request-revoke-retry"); err != nil {
		t.Fatalf("idempotent Revoke() error = %v", err)
	}
	if _, err := environment.service.Authenticate(context.Background(), rotated.Secret, netip.MustParseAddr("203.0.113.20")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked secret error = %v, want ErrInvalidKey", err)
	}

	keys, err := environment.service.List(context.Background(), environment.userA)
	if err != nil || len(keys) != 1 || keys[0].Status != "revoked" {
		t.Fatalf("List() = %+v, %v", keys, err)
	}
	var auditCount int
	if err := environment.store.Queryer().QueryRow(context.Background(), `
		SELECT count(*) FROM audit_logs
		WHERE target_type = 'api_key' AND target_id = $1`, created.Key.ID.String()).Scan(&auditCount); err != nil {
		t.Fatalf("count API key audit: %v", err)
	}
	if auditCount != 5 {
		t.Fatalf("audit count = %d, want create+2 update+rotate+revoke", auditCount)
	}
}

func TestAPIKeyExpiryAndDefaultAllowPolicy(t *testing.T) {
	environment := newAPIKeyTestEnvironment(t)
	expiresAt := environment.now.Add(time.Minute)
	created, err := environment.service.Create(context.Background(), environment.userA, CreateInput{
		Name: "expiring", ExpiresAt: &expiresAt,
	}, "request-create")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	allowPolicyID := uuid.New()
	if _, err := environment.store.Queryer().Exec(context.Background(), `
		INSERT INTO policies (id, name, default_action) VALUES ($1, $2, 'allow')`,
		allowPolicyID, "allow-"+allowPolicyID.String()); err != nil {
		t.Fatalf("seed default allow policy: %v", err)
	}
	if _, err := environment.store.Queryer().Exec(context.Background(), `
		INSERT INTO api_key_policies (api_key_id, policy_id) VALUES ($1, $2)`,
		created.Key.ID, allowPolicyID); err != nil {
		t.Fatalf("assign default allow policy: %v", err)
	}
	principal, err := environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-c", 0); err != nil {
		t.Fatalf("default allow Authorize() error = %v", err)
	}
	if err := environment.service.Authorize(context.Background(), principal, "model-private", 0); !errors.Is(err, ErrModelDenied) {
		t.Fatalf("default allow private model error = %v, want ErrModelDenied", err)
	}
	environment.service.now = func() time.Time { return expiresAt }
	if _, err := environment.service.Authenticate(context.Background(), created.Secret, netip.MustParseAddr("192.0.2.1")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expired Authenticate() error = %v, want ErrInvalidKey", err)
	}
}

func TestRotateRejectsExpiryReachedWhileWaitingForRowLock(t *testing.T) {
	environment := newAPIKeyTestEnvironment(t)
	created, err := environment.service.Create(context.Background(), environment.userA, CreateInput{Name: "locking"}, "request-create")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, environment.databaseURL)
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		UPDATE api_keys
		SET expires_at = clock_timestamp() + interval '1 second'
		WHERE id = $1`, created.Key.ID); err != nil {
		t.Fatalf("lock expiring API key: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, rotateErr := environment.service.Rotate(context.Background(), environment.userA, created.Key.ID, "request-rotate")
		result <- rotateErr
	}()
	time.Sleep(1500 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit expiring API key: %v", err)
	}
	if err := <-result; !errors.Is(err, ErrConflict) {
		t.Fatalf("Rotate() after lock-crossed expiry error = %v, want ErrConflict", err)
	}
}

func TestAPIKeyProtectedHTTPAndOneTimeSecret(t *testing.T) {
	environment := newAPIKeyTestEnvironment(t)
	box, err := auth.NewSecretBox(bytes.Repeat([]byte{0x42}, 32), "test-v1")
	if err != nil {
		t.Fatalf("auth.NewSecretBox() error = %v", err)
	}
	authRepository, err := auth.NewRepository(environment.store)
	if err != nil {
		t.Fatalf("auth.NewRepository() error = %v", err)
	}
	authService, err := auth.NewService(authRepository, auth.ServiceConfig{
		SessionTTL: time.Hour, Issuer: "Bablo Test", SecretBox: box,
	})
	if err != nil {
		t.Fatalf("auth.NewService() error = %v", err)
	}
	user, err := authService.CreateLocalUser(context.Background(), "key-owner@example.test", "long enough test password", false, "create-user")
	if err != nil {
		t.Fatalf("CreateLocalUser() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authHandler, err := auth.NewHandler(authService, logger, auth.HandlerConfig{
		AllowedOrigin: "https://console.example", SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.NewHandler() error = %v", err)
	}
	apiHandler, err := NewHandler(environment.service, logger)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	protected := authHandler.Protect(apiHandler)

	login := jsonRequest(t, authHandler, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": user.Email, "password": "long enough test password",
	}, nil, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	sessionCookie, csrfCookie := responseAuthCookies(t, login.Result())
	withoutCSRF := jsonRequest(t, protected, http.MethodPost, apiKeyPath, map[string]any{
		"name": "http", "allowed_models": []string{"model-a", "model-b"},
	}, []*http.Cookie{sessionCookie, csrfCookie}, "")
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d, want 403", withoutCSRF.Code)
	}
	created := jsonRequest(t, protected, http.MethodPost, apiKeyPath, map[string]any{
		"name": "http", "allowed_models": []string{"model-a", "model-b"},
	}, []*http.Cookie{sessionCookie, csrfCookie}, csrfCookie.Value)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create = %d headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	var response struct {
		APIKey keyResponse `json:"api_key"`
		Secret string      `json:"secret"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if response.Secret == "" || response.APIKey.ID == uuid.Nil {
		t.Fatalf("create response = %+v", response)
	}

	listed := jsonRequest(t, protected, http.MethodGet, apiKeyPath, nil, []*http.Cookie{sessionCookie}, "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), response.Secret) || strings.Contains(listed.Body.String(), "secret_hash") {
		t.Fatalf("list leaked secret: %d %s", listed.Code, listed.Body.String())
	}
	patched := jsonRequest(t, protected, http.MethodPatch, apiKeyPath+"/"+response.APIKey.ID.String(), map[string]any{
		"name": "http-updated", "expires_at": nil, "rpm_limit": int64(5), "allowed_models": []string{"model-b"},
	}, []*http.Cookie{sessionCookie, csrfCookie}, csrfCookie.Value)
	if patched.Code != http.StatusOK || !strings.Contains(patched.Body.String(), `"name":"http-updated"`) || !strings.Contains(patched.Body.String(), `"allowed_models":["model-b"]`) {
		t.Fatalf("patch = %d %s", patched.Code, patched.Body.String())
	}
	rotated := jsonRequest(t, protected, http.MethodPost, apiKeyPath+"/"+response.APIKey.ID.String()+"/rotate", nil, []*http.Cookie{sessionCookie, csrfCookie}, csrfCookie.Value)
	if rotated.Code != http.StatusOK || strings.Contains(rotated.Body.String(), response.Secret) {
		t.Fatalf("rotate = %d %s", rotated.Code, rotated.Body.String())
	}
	revoked := jsonRequest(t, protected, http.MethodPost, apiKeyPath+"/"+response.APIKey.ID.String()+"/revoke", nil, []*http.Cookie{sessionCookie, csrfCookie}, csrfCookie.Value)
	if revoked.Code != http.StatusOK || !strings.Contains(revoked.Body.String(), `"status":"revoked"`) {
		t.Fatalf("revoke = %d %s", revoked.Code, revoked.Body.String())
	}
}

func assertIdentityRequest(t *testing.T, service *Service, secret, model string, wantStatus int) {
	t.Helper()
	handler := service.IdentityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		if !ok || principal.APIKeyID == uuid.Nil || principal.UserID == uuid.Nil {
			t.Error("principal missing from context")
			http.Error(w, "principal missing", http.StatusInternalServerError)
			return
		}
		if err := service.Authorize(r.Context(), principal, r.URL.Query().Get("model"), 1); err != nil {
			writeAPIKeyError(w, r, err, slog.New(slog.NewTextHandler(io.Discard, nil)))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodPost, "http://bablo.test/inference?model="+url.QueryEscape(model), nil)
	request.RemoteAddr = "[::ffff:203.0.113.20]:12345"
	request.Header.Set("Authorization", "Bearer "+secret)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != wantStatus {
		t.Fatalf("identity request model %s = %d body=%s, want %d", model, recorder.Code, recorder.Body.String(), wantStatus)
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatal("identity response leaked raw API key")
	}
}

func newAPIKeyTestEnvironment(t *testing.T) apiKeyTestEnvironment {
	t.Helper()
	databaseURL := isolatedAPIKeyDatabaseURL(t)
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
	environment := apiKeyTestEnvironment{
		store:       store,
		databaseURL: databaseURL,
		now:         time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		userA:       uuid.New(), userB: uuid.New(),
		modelA: uuid.New(), modelB: uuid.New(), modelC: uuid.New(), modelPrivate: uuid.New(),
	}
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO users (id, email_normalized, password_hash, password_params_version)
		VALUES ($1, $2, 'hash', 'test'), ($3, $4, 'hash', 'test')`,
		environment.userA, environment.userA.String()+"@example.test",
		environment.userB, environment.userB.String()+"@example.test"); err != nil {
		t.Fatalf("seed API key users: %v", err)
	}
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO models (id, public_model_id, display_name, visibility)
		VALUES ($1, 'model-a', 'Model A', 'public'),
		       ($2, 'model-b', 'Model B', 'public'),
		       ($3, 'model-c', 'Model C', 'public'),
		       ($4, 'model-private', 'Model Private', 'private')`,
		environment.modelA, environment.modelB, environment.modelC, environment.modelPrivate); err != nil {
		t.Fatalf("seed API key models: %v", err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, NewMemoryLimiter())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	service.now = func() time.Time { return environment.now }
	environment.service = service
	return environment
}

func isolatedAPIKeyDatabaseURL(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_apikey_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		pool.Close()
		t.Fatalf("create test schema: %v", err)
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
			t.Errorf("open cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	return parsed.String()
}

func jsonRequest(t *testing.T, handler http.Handler, method, path string, payload any, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Origin", "https://console.example")
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func responseAuthCookies(t *testing.T, response *http.Response) (*http.Cookie, *http.Cookie) {
	t.Helper()
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range response.Cookies() {
		switch cookie.Name {
		case "bablo_session":
			sessionCookie = cookie
		case "bablo_csrf":
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("authentication cookies missing: %v", response.Cookies())
	}
	return sessionCookie, csrfCookie
}
