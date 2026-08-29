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
