package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/starhui-dev/bablo/internal/config"
)

func testServer() *Server {
	return New(config.Config{HTTPAddr: ":0"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
}

func TestHealthzAssignsRequestID(t *testing.T) {
	server := testServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.HasPrefix(recorder.Header().Get("X-Request-ID"), "req_") {
		t.Fatalf("request id = %q, want generated request id", recorder.Header().Get("X-Request-ID"))
	}
	if !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %q, want health status", recorder.Body.String())
	}
}

func TestRequestIDPreservesSafeValueAndRejectsUnsafeValue(t *testing.T) {
	server := testServer()

	preserved := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "client-123")
	server.Handler().ServeHTTP(preserved, request)
	if got := preserved.Header().Get("X-Request-ID"); got != "client-123" {
		t.Fatalf("safe request id = %q, want client-123", got)
	}

	replaced := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "bad value\n")
	server.Handler().ServeHTTP(replaced, request)
	if got := replaced.Header().Get("X-Request-ID"); got == "bad value\n" || !strings.HasPrefix(got, "req_") {
		t.Fatalf("unsafe request id = %q, want generated id", got)
	}
}

func TestReadyzIsConservativeBeforeDependenciesInitialize(t *testing.T) {
	server := testServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), `"not_ready"`) {
		t.Fatalf("body = %q, want not_ready", recorder.Body.String())
	}
}

func TestReadyzAllowsDocumentedSingleInstanceMemoryLimiter(t *testing.T) {
	server := New(config.Config{HTTPAddr: ":0", DatabaseURL: "configured"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	server.SetDependencyReady("postgres", true)
	server.SetDependencyReady("inference", true)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"redis":"not_configured"`) {
		t.Fatalf("body = %q, want honest Redis fallback state", recorder.Body.String())
	}
}

func TestUnknownPathReturnsJSONNotFound(t *testing.T) {
	server := testServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if !strings.Contains(recorder.Body.String(), `"not_found"`) {
		t.Fatalf("body = %q, want not_found", recorder.Body.String())
	}
}

func TestAuthSurfaceFailsClosedWhenUnavailable(t *testing.T) {
	server := testServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"user@example.test","password":"not-used"}`))

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), `"code":"auth_unavailable"`) || !strings.Contains(recorder.Body.String(), `"request_id":"req_`) {
		t.Fatalf("body = %q, want stable auth_unavailable envelope", recorder.Body.String())
	}
}

func TestAPIKeySurfaceMountsOnlyConfiguredPaths(t *testing.T) {
	called := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	})
	server := New(config.Config{HTTPAddr: ":0"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", WithAPIKeyHandler(handler))
	for _, path := range []string{"/api/v1/me/api-keys", "/api/v1/me/api-keys/key-id/rotate"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("path %s status = %d, want 204", path, recorder.Code)
		}
	}
	if called != 2 {
		t.Fatalf("API key handler calls = %d, want 2", called)
	}

	unconfigured := testServer()
	recorder := httptest.NewRecorder()
	unconfigured.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/me/api-keys", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unconfigured API key status = %d, want 404", recorder.Code)
	}
}

func TestCatalogSurfacesMountOnlyConfiguredPaths(t *testing.T) {
	modelCalls := 0
	adminCalls := 0
	modelHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		adminCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	server := New(
		config.Config{HTTPAddr: ":0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"test",
		WithModelHandler(modelHandler),
		WithAdminCatalogHandler(adminHandler),
	)
	paths := []string{
		"/api/v1/models",
		"/api/v1/admin/models",
		"/api/v1/admin/providers/provider-id/reconcile",
		"/api/v1/admin/provider-models/provider-model-id",
		"/api/v1/admin/prices/price-id/activate",
		"/api/v1/admin/credentials/credential-id/health",
		"/api/v1/admin/credential-pools/pool-id/members",
	}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("path %s status = %d, want 204", path, recorder.Code)
		}
	}
	if modelCalls != 1 || adminCalls != 6 {
		t.Fatalf("catalog handler calls = model:%d admin:%d", modelCalls, adminCalls)
	}

	unconfigured := testServer()
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		unconfigured.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("unconfigured path %s status = %d, want 404", path, recorder.Code)
		}
	}
}
