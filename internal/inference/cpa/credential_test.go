package cpa

import (
	"context"
	"testing"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/inference"
)

func TestRegisterCredentialMapsRuntimeOnlyCredentialWithoutPersistence(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := newWithManager(manager, nil, nil, inference.Capabilities{})
	credentialID := uuid.New()
	value := credential.RuntimeCredential{
		CredentialID:     credentialID,
		ProviderID:       uuid.New(),
		ProviderSlug:     "codex",
		ExternalStableID: "oauth-account",
		SourceKind:       credential.SourceOAuth,
		Region:           "us-east-1",
		ProxyRef:         "http://proxy.example.test:8080",
		Metadata: map[string]string{
			"account_email": "owner@example.test",
			"base_url":      "https://api.example.test/v1",
			"provider_key":  "codex",
		},
		Secrets: map[string][]byte{
			credential.SecretOAuthAccess:  []byte("access-token"),
			credential.SecretOAuthRefresh: []byte("refresh-token"),
		},
	}
	if err := adapter.RegisterCredential(context.Background(), value); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	registered, ok := manager.GetByID(credentialID.String())
	if !ok || registered == nil {
		t.Fatal("registered credential missing")
	}
	if registered.Attributes[coreauth.AttributeRuntimeOnly] != "true" || registered.Attributes[coreauth.AttributeAuthKind] != credential.SourceOAuth || registered.Attributes[coreauth.AttributeSourceBackend] != babloCredentialSource {
		t.Fatalf("runtime attributes = %#v", registered.Attributes)
	}
	if registered.Attributes["base_url"] != "https://api.example.test/v1" || registered.Attributes["provider_key"] != "codex" || registered.ProxyURL != "http://proxy.example.test:8080" {
		t.Fatalf("runtime endpoint/proxy mapping = %#v / %q", registered.Attributes, registered.ProxyURL)
	}
	if registered.Metadata["access_token"] != "access-token" || registered.Metadata["refresh_token"] != "refresh-token" {
		t.Fatal("registered OAuth token metadata does not match source")
	}
	if registered.Metadata["account_email"] != "owner@example.test" {
		t.Fatalf("non-secret metadata = %#v", registered.Metadata)
	}
}

func TestRegisterCredentialClosesSecretsOnUnsupportedSource(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := newWithManager(manager, nil, nil, inference.Capabilities{})
	secret := []byte("service-account-json")
	value := credential.RuntimeCredential{
		CredentialID: uuid.New(), ProviderSlug: "service", SourceKind: credential.SourceServiceAccount,
		Secrets: map[string][]byte{credential.SecretServiceAcct: secret},
	}
	if err := adapter.RegisterCredential(context.Background(), value); err == nil {
		t.Fatal("RegisterCredential() unexpectedly accepted unsupported source")
	}
	for index, item := range secret {
		if item != 0 {
			t.Fatalf("secret byte %d was not cleared", index)
		}
	}
}

type credentialSourceFunc func(context.Context, func(credential.RuntimeCredential) error) error

func (source credentialSourceFunc) ForEachActive(ctx context.Context, sink func(credential.RuntimeCredential) error) error {
	return source(ctx, sink)
}

func TestReconcileCredentialsRemovesOnlyStaleBabloRuntimeEntries(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := newWithManager(manager, nil, nil, inference.Capabilities{})
	staleID := uuid.New()
	foreignID := uuid.New()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: staleID.String(), Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeRuntimeOnly: "true", coreauth.AttributeSourceBackend: babloCredentialSource}}); err != nil {
		t.Fatalf("register stale auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: foreignID.String(), Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register foreign auth: %v", err)
	}
	currentID := uuid.New()
	source := credentialSourceFunc(func(_ context.Context, sink func(credential.RuntimeCredential) error) error {
		return sink(credential.RuntimeCredential{
			CredentialID: currentID,
			ProviderSlug: "codex",
			SourceKind:   credential.SourceAPIKey,
			Secrets:      map[string][]byte{credential.SecretAPIKey: []byte("current-api-key")},
		})
	})
	if err := adapter.ReconcileCredentials(context.Background(), source); err != nil {
		t.Fatalf("ReconcileCredentials() error = %v", err)
	}
	if _, ok := manager.GetByID(staleID.String()); ok {
		t.Fatal("stale Bablo runtime credential was not removed")
	}
	if _, ok := manager.GetByID(foreignID.String()); !ok {
		t.Fatal("foreign runtime credential was incorrectly removed")
	}
	registered, ok := manager.GetByID(currentID.String())
	if !ok || registered.Attributes[coreauth.AttributeSourceBackend] != babloCredentialSource || registered.Attributes[coreauth.AttributeAPIKey] != "current-api-key" {
		t.Fatal("current runtime credential was not reconciled")
	}
}
