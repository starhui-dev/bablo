package cpa

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/starhui-dev/bablo/internal/inference"
	"github.com/starhui-dev/bablo/internal/quota"
)

func TestRuntimeHealthProbeReportsManagerState(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := newWithManager(manager, nil, nil, inference.Capabilities{})
	credentialID := uuid.New()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: credentialID.String(), Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	probe := NewHealthProbe(adapter)
	request := quota.ProbeRequest{CredentialID: credentialID, ProviderSlug: "codex"}
	if !probe.Supports("CODEX") {
		t.Fatal("health probe does not support registered provider")
	}
	if err := probe.Probe(context.Background(), request); err != nil {
		t.Fatalf("active Probe() error = %v", err)
	}
}

func TestRuntimeHealthProbeClassifiesUnavailableQuota(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := newWithManager(manager, nil, nil, inference.Capabilities{})
	credentialID := uuid.New()
	recoverAt := time.Now().UTC().Add(time.Minute)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: credentialID.String(), Provider: "codex", Status: coreauth.StatusActive,
		Unavailable: true,
		Quota:       coreauth.QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	probe := NewHealthProbe(adapter)
	err := probe.Probe(context.Background(), quota.ProbeRequest{CredentialID: credentialID, ProviderSlug: "codex"})
	var probeErr *quota.ProbeError
	if !errors.As(err, &probeErr) || probeErr == nil {
		t.Fatalf("Probe() error = %T %v, want ProbeError", err, err)
	}
	if probeErr.Class != "quota" || probeErr.HTTPStatus != http.StatusTooManyRequests || probeErr.RetryAfter <= 0 {
		t.Fatalf("ProbeError = %+v", probeErr)
	}
}

func TestRuntimeHealthProbeRejectsMissingAndDisabledCredential(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := newWithManager(manager, nil, nil, inference.Capabilities{})
	probe := NewHealthProbe(adapter)
	missingErr := probe.Probe(context.Background(), quota.ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex"})
	var missing *quota.ProbeError
	if !errors.As(missingErr, &missing) || missing.Class != "credential_unavailable" {
		t.Fatalf("missing credential error = %T %v", missingErr, missingErr)
	}
	credentialID := uuid.New()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: credentialID.String(), Provider: "codex", Status: coreauth.StatusDisabled,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	disabledErr := probe.Probe(context.Background(), quota.ProbeRequest{CredentialID: credentialID, ProviderSlug: "codex"})
	var disabled *quota.ProbeError
	if !errors.As(disabledErr, &disabled) || disabled.Class != "credential_disabled" || disabled.HTTPStatus != http.StatusForbidden {
		t.Fatalf("disabled credential error = %T %v", disabledErr, disabledErr)
	}
}
