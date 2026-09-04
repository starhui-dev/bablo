package credential

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/quota"
)

type quotaViewerFunc func(context.Context, uuid.UUID, string, int) (quota.View, error)

func (f quotaViewerFunc) View(ctx context.Context, credentialID uuid.UUID, windowKind string, limit int) (quota.View, error) {
	return f(ctx, credentialID, windowKind, limit)
}

func TestCredentialQuotaEndpointReturnsBoundedView(t *testing.T) {
	credentialID := uuid.New()
	called := false
	handler := &handler{
		quota: quotaViewerFunc(func(_ context.Context, gotID uuid.UUID, windowKind string, limit int) (quota.View, error) {
			called = true
			if gotID != credentialID || windowKind != quota.WindowMinute || limit != 7 {
				t.Fatalf("quota viewer arguments = %s/%q/%d", gotID, windowKind, limit)
			}
			return quota.View{CredentialID: credentialID, ProviderSlug: "codex", Snapshots: []quota.Snapshot{{WindowKind: quota.WindowMinute, ObservedAt: time.Now().UTC()}}}, nil
		}),
		logger: slog.Default(),
	}
	request := httptest.NewRequest(http.MethodGet, adminCredentialsPath+"/"+credentialID.String()+"/quota?window_kind=minute&limit=7", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !called {
		t.Fatalf("quota response = %d/%v body=%s", response.Code, called, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode quota response: %v", err)
	}
	if len(payload["quota"]) == 0 {
		t.Fatalf("quota response missing quota field: %s", response.Body.String())
	}
}

func TestCredentialQuotaEndpointRejectsInvalidLimit(t *testing.T) {
	handler := &handler{quota: quotaViewerFunc(func(context.Context, uuid.UUID, string, int) (quota.View, error) {
		t.Fatal("quota viewer called for invalid limit")
		return quota.View{}, nil
	}), logger: slog.Default()}
	request := httptest.NewRequest(http.MethodGet, adminCredentialsPath+"/"+uuid.NewString()+"/quota?limit=0", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, body = %s", response.Code, response.Body.String())
	}
}
