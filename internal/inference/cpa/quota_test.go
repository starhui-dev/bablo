package cpa

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/starhui-dev/bablo/internal/quota"
)

func TestEarliestResetAtParsesProviderResetForms(t *testing.T) {
	observedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	epochReset := observedAt.Add(90 * time.Second).Unix()
	reset, ok := earliestResetAt(map[string]string{
		"X-Codex-Primary-Reset-After-Seconds": "30",
		"X-Codex-Primary-Reset-At":            stringInt64(epochReset),
		"Retry-After":                         "60",
	}, observedAt), true
	if !ok || reset == nil {
		t.Fatal("earliestResetAt() returned no reset")
	}
	if want := observedAt.Add(30 * time.Second); !reset.Equal(want) {
		t.Fatalf("reset = %v, want earliest %v", reset, want)
	}
}

func TestEarliestResetAtAcceptsHTTPDateRetryAfter(t *testing.T) {
	observedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	want := observedAt.Add(2 * time.Minute)
	reset := earliestResetAt(map[string]string{"Retry-After": want.Format(http.TimeFormat)}, observedAt)
	if reset == nil || !reset.Equal(want) {
		t.Fatalf("reset = %v, want %v", reset, want)
	}
}

func TestEarliestResetAtRejectsOverflowingRelativeSeconds(t *testing.T) {
	observedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if reset := earliestResetAt(map[string]string{"X-Codex-Primary-Reset-After-Seconds": "9223372036854775807"}, observedAt); reset != nil {
		t.Fatalf("overflowing reset = %v, want nil", reset)
	}
}

func TestAdapterQuotaObservationUsesProviderSpecificWindow(t *testing.T) {
	adapter := &Adapter{}
	request := quota.ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex", Model: "gpt-5"}
	observedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	observation, err := adapter.ObserveResponse(nil, request, map[string][]string{
		"X-Codex-Primary-Used-Percent":        {"50"},
		"X-Codex-Primary-Reset-After-Seconds": {"120"},
	}, observedAt)
	if err != nil {
		t.Fatalf("ObserveResponse() error = %v", err)
	}
	if len(observation.Windows) != 1 || observation.Windows[0].Kind != quota.WindowProviderSpecific {
		t.Fatalf("windows = %#v", observation.Windows)
	}
	if observation.Windows[0].ResetAt == nil || !observation.Windows[0].ResetAt.Equal(observedAt.Add(2*time.Minute)) {
		t.Fatalf("reset_at = %v", observation.Windows[0].ResetAt)
	}
}

func TestObservationDigestChangesWithSignalSnapshot(t *testing.T) {
	request := quota.ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex", Model: "gpt-5"}
	stateA := coreauth.QuotaState{ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Codex-Plan-Type": "pro"}}
	stateB := coreauth.QuotaState{ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Codex-Plan-Type": "team"}}
	if quotaObservationDigest(request, stateA) == quotaObservationDigest(request, stateB) {
		t.Fatal("different quota signal snapshots share an observation key")
	}
}

func stringInt64(value int64) string {
	return fmt.Sprintf("%d", value)
}
