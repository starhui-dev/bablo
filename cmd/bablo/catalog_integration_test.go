package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/config"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/httpapi"
	"github.com/starhui-dev/bablo/internal/secret"
	"github.com/starhui-dev/bablo/migrations"
)

func TestCatalogManagementHTTPRequiresAdminAndPublishesPrices(t *testing.T) {
	store := catalogHTTPStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	box, err := auth.NewSecretBox(bytes.Repeat([]byte{0x31}, 32), "catalog-test-v1")
	if err != nil {
		t.Fatalf("auth.NewSecretBox() error = %v", err)
	}
	credentialKeyring, err := secret.NewKeyring("v1", map[string][]byte{"v1": bytes.Repeat([]byte{0x41}, 32)})
	if err != nil {
		t.Fatalf("secret.NewKeyring() error = %v", err)
	}
	authRepository, err := auth.NewRepository(store)
	if err != nil {
		t.Fatalf("auth.NewRepository() error = %v", err)
	}
	authService, err := auth.NewService(authRepository, auth.ServiceConfig{
		SessionTTL:      12 * time.Hour,
		Issuer:          "Bablo Catalog Test",
		RequireAdminMFA: true,
		SecretBox:       box,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("auth.NewService() error = %v", err)
	}
	admin, err := authService.CreateLocalUser(context.Background(), "catalog-admin@example.test", "catalog admin password", true, "catalog-admin-create")
	if err != nil {
		t.Fatalf("CreateLocalUser(admin) error = %v", err)
	}
	member, err := authService.CreateLocalUser(context.Background(), "catalog-member@example.test", "catalog member password", false, "catalog-member-create")
	if err != nil {
		t.Fatalf("CreateLocalUser(member) error = %v", err)
	}
	adminLogin, err := authService.Login(context.Background(), admin.Email, "catalog admin password", "", auth.LoginMetadata{RequestID: "catalog-admin-login"})
	if err != nil {
		t.Fatalf("admin Login() error = %v", err)
	}
	binding, err := authService.BeginTOTP(context.Background(), adminLogin.Session, "catalog-mfa-begin")
	if err != nil {
		t.Fatalf("BeginTOTP() error = %v", err)
	}
	passcode, err := totp.GenerateCode(binding.Secret, now)
	if err != nil {
		t.Fatalf("totp.GenerateCode() error = %v", err)
	}
	adminBundle, _, err := authService.ConfirmTOTP(context.Background(), adminLogin.Session, passcode, auth.LoginMetadata{RequestID: "catalog-mfa-confirm"})
	if err != nil {
		t.Fatalf("ConfirmTOTP() error = %v", err)
	}
	memberBundle, err := authService.Login(context.Background(), member.Email, "catalog member password", "", auth.LoginMetadata{RequestID: "catalog-member-login"})
	if err != nil {
		t.Fatalf("member Login() error = %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authHandler, err := auth.NewHandler(authService, logger, auth.HandlerConfig{
		AllowedOrigin: "https://console.example",
		CookieSecure:  true,
		SessionTTL:    12 * time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.NewHandler() error = %v", err)
	}
	options, err := catalogServerOptions(store, authHandler, credentialKeyring, logger)
	if err != nil {
		t.Fatalf("catalogServerOptions() error = %v", err)
	}
	server := httpapi.New(config.Config{HTTPAddr: ":0"}, logger, "test", options...)

	modelPayload := map[string]any{
		"public_model_id": "catalog-chat",
		"aliases":         []string{"catalog-latest"},
		"display_name":    "Catalog Chat",
		"visibility":      "public",
		"billing_class":   "token",
		"capabilities": map[string]bool{
			"chat": true, "stream": true, "tools": true,
		},
	}
	forbidden := catalogHTTPRequest(t, server.Handler(), memberBundle, http.MethodPost, "/api/v1/admin/models", modelPayload)
	if forbidden.Code != http.StatusForbidden || !strings.Contains(forbidden.Body.String(), `"code":"permission_denied"`) {
		t.Fatalf("non-admin model create = %d %s", forbidden.Code, forbidden.Body.String())
	}
	createdModel := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPost, "/api/v1/admin/models", modelPayload)
	if createdModel.Code != http.StatusCreated {
		t.Fatalf("admin model create = %d %s", createdModel.Code, createdModel.Body.String())
	}
	modelID := responseUUID(t, createdModel.Body.Bytes(), "model")
	userModels := catalogHTTPRequest(t, server.Handler(), memberBundle, http.MethodGet, "/api/v1/models", nil)
	if userModels.Code != http.StatusOK || !strings.Contains(userModels.Body.String(), `"public_model_id":"catalog-chat"`) {
		t.Fatalf("user model list = %d %s", userModels.Code, userModels.Body.String())
	}

	createdProvider := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPost, "/api/v1/admin/providers", map[string]any{
		"slug": "catalog-official", "display_name": "Catalog Official", "resource_type": "official_api", "commercial_allowed": true,
	})
	if createdProvider.Code != http.StatusCreated {
		t.Fatalf("provider create = %d %s", createdProvider.Code, createdProvider.Body.String())
	}
	providerID := responseUUID(t, createdProvider.Body.Bytes(), "provider")
	createdCredential := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPost, "/api/v1/admin/credentials", map[string]any{
		"provider_id": providerID, "external_stable_id": "catalog-credential", "source_kind": "api_key",
		"metadata": map[string]string{"account_email": "catalog@example.test"},
		"secrets":  []map[string]string{{"kind": "api_key", "value": "catalog-secret-value"}},
	})
	if createdCredential.Code != http.StatusCreated || strings.Contains(createdCredential.Body.String(), "catalog-secret-value") {
		t.Fatalf("credential create = %d %s", createdCredential.Code, createdCredential.Body.String())
	}
	credentialID := responseUUID(t, createdCredential.Body.Bytes(), "credential")
	credentialHealth := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodGet, "/api/v1/admin/credentials/"+credentialID.String()+"/health", nil)
	if credentialHealth.Code != http.StatusOK || strings.Contains(credentialHealth.Body.String(), "catalog-secret-value") {
		t.Fatalf("credential health = %d %s", credentialHealth.Code, credentialHealth.Body.String())
	}
	reconciled := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPost, "/api/v1/admin/providers/"+providerID.String()+"/reconcile", map[string]any{
		"models": []map[string]any{{
			"upstream_model_id": "upstream-catalog-chat",
			"protocol":          "openai_chat",
			"capabilities": map[string]bool{
				"chat": true, "stream": true, "tools": true,
			},
		}},
	})
	if reconciled.Code != http.StatusOK || !strings.Contains(reconciled.Body.String(), `"discovered":1`) {
		t.Fatalf("provider reconcile = %d %s", reconciled.Code, reconciled.Body.String())
	}
	providerModels := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodGet, "/api/v1/admin/provider-models?provider_id="+providerID.String(), nil)
	if providerModels.Code != http.StatusOK {
		t.Fatalf("provider model list = %d %s", providerModels.Code, providerModels.Body.String())
	}
	providerModelID := responseDataUUID(t, providerModels.Body.Bytes())
	approved := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPatch, "/api/v1/admin/provider-models/"+providerModelID.String(), map[string]any{
		"model_id": modelID, "review_status": "approved", "enabled": true,
	})
	if approved.Code != http.StatusOK || !strings.Contains(approved.Body.String(), `"review_status":"approved"`) {
		t.Fatalf("provider model approval = %d %s", approved.Code, approved.Body.String())
	}

	createdPrice := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPost, "/api/v1/admin/prices", map[string]any{
		"scope": "provider_model", "currency": "USD", "effective_from": now.Add(-time.Hour),
		"prices": []map[string]any{
			{"provider_model_id": providerModelID, "dimension": "input_token", "unit_price": "0.000002"},
			{"provider_model_id": providerModelID, "dimension": "output_token", "unit_price": "0.000008"},
		},
	})
	if createdPrice.Code != http.StatusCreated {
		t.Fatalf("price create = %d %s", createdPrice.Code, createdPrice.Body.String())
	}
	priceID := responseUUID(t, createdPrice.Body.Bytes(), "price_version")
	activated := catalogHTTPRequest(t, server.Handler(), adminBundle, http.MethodPost, "/api/v1/admin/prices/"+priceID.String()+"/activate", nil)
	if activated.Code != http.StatusOK || !strings.Contains(activated.Body.String(), `"status":"active"`) {
		t.Fatalf("price activate = %d %s", activated.Code, activated.Body.String())
	}
}

func catalogHTTPRequest(t *testing.T, handler http.Handler, bundle auth.SessionBundle, method, path string, payload any) *httptest.ResponseRecorder {
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
	request.AddCookie(&http.Cookie{Name: "bablo_session", Value: bundle.SessionToken})
	request.AddCookie(&http.Cookie{Name: "bablo_csrf", Value: bundle.CSRFToken})
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		request.Header.Set("X-CSRF-Token", bundle.CSRFToken)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func responseUUID(t *testing.T, body []byte, key string) uuid.UUID {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode response envelope: %v", err)
	}
	var resource struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(envelope[key], &resource); err != nil || resource.ID == uuid.Nil {
		t.Fatalf("decode %s ID: %v, body = %s", key, err, body)
	}
	return resource.ID
}

func responseDataUUID(t *testing.T, body []byte) uuid.UUID {
	t.Helper()
	var envelope struct {
		Data []struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Data) != 1 || envelope.Data[0].ID == uuid.Nil {
		t.Fatalf("decode data ID: %v, body = %s", err, body)
	}
	return envelope.Data[0].ID
}

func catalogHTTPStore(t *testing.T) *data.Store {
	t.Helper()
	databaseURL := isolatedCatalogHTTPDatabaseURL(t)
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

func isolatedCatalogHTTPDatabaseURL(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_catalog_http_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open catalog HTTP test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create catalog HTTP test schema: %v", err)
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
			t.Errorf("open catalog HTTP cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop catalog HTTP test schema: %v", err)
		}
	})
	return parsed.String()
}
