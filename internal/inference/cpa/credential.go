package cpa

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/inference"
)

const babloCredentialSource = "bablo-postgres"

// CredentialSource streams active PostgreSQL credentials without exposing CPA types.
type CredentialSource interface {
	ForEachActive(context.Context, func(credential.RuntimeCredential) error) error
}

// RegisterCredential installs one PostgreSQL-backed credential into the CPA
// runtime manager. The runtime-only marker prevents CPA from treating its auth
// file store as Bablo's source of truth.
func (a *Adapter) RegisterCredential(ctx context.Context, value credential.RuntimeCredential) error {
	if a == nil || a.manager == nil || value.CredentialID == uuid.Nil {
		value.Close()
		return errors.New("cpa adapter: credential runtime is unavailable")
	}
	defer value.Close()

	provider := strings.TrimSpace(value.ProviderSlug)
	if provider == "" {
		return errors.New("cpa adapter: credential provider is required")
	}
	if value.SourceKind != credential.SourceOAuth && value.SourceKind != credential.SourceAPIKey {
		return &inference.UpstreamError{Class: "credential_source_unsupported", HTTPStatus: http.StatusNotImplemented}
	}

	attributes := map[string]string{
		auth.AttributeAuthKind:      value.SourceKind,
		auth.AttributeRuntimeOnly:   "true",
		auth.AttributeSource:        auth.AuthSourcePostgres,
		auth.AttributeSourceBackend: babloCredentialSource,
		"region":                    value.Region,
	}
	// OpenAI-compatible and several native CPA executors read immutable
	// endpoint configuration from Auth.Attributes, while mutable OAuth tokens
	// stay in Auth.Metadata. Credential metadata is non-secret and validated by
	// the credential service; only the reserved endpoint routing fields are
	// promoted here.
	if baseURL := strings.TrimSpace(value.Metadata["base_url"]); baseURL != "" {
		attributes["base_url"] = baseURL
	}
	if providerKey := strings.TrimSpace(value.Metadata["provider_key"]); providerKey != "" {
		attributes["provider_key"] = providerKey
	}
	if compatName := strings.TrimSpace(value.Metadata["compat_name"]); compatName != "" {
		attributes["compat_name"] = compatName
	}
	record := &auth.Auth{
		ID:         value.CredentialID.String(),
		Provider:   provider,
		Label:      value.ExternalStableID,
		Status:     auth.StatusActive,
		ProxyURL:   strings.TrimSpace(value.ProxyRef),
		Attributes: attributes,
		Metadata:   make(map[string]any, len(value.Metadata)+2),
	}
	for key, item := range value.Metadata {
		record.Metadata[key] = item
	}
	if value.SourceKind == credential.SourceAPIKey {
		key := value.Secrets[credential.SecretAPIKey]
		if len(key) == 0 {
			return errors.New("cpa adapter: API key secret is missing")
		}
		record.Attributes[auth.AttributeAPIKey] = string(key)
	} else {
		if access := value.Secrets[credential.SecretOAuthAccess]; len(access) > 0 {
			record.Metadata["access_token"] = string(access)
		}
		if refresh := value.Secrets[credential.SecretOAuthRefresh]; len(refresh) > 0 {
			record.Metadata["refresh_token"] = string(refresh)
		}
		if _, ok := record.Metadata["access_token"]; !ok {
			return errors.New("cpa adapter: OAuth access token is missing")
		}
	}
	return a.registerAuth(ctx, record)
}

// ReconcileCredentials replaces Bablo-owned CPA runtime auths from PostgreSQL.
// Stale runtime entries are removed only after a complete successful source pass.
func (a *Adapter) ReconcileCredentials(ctx context.Context, source CredentialSource) error {
	if a == nil || a.manager == nil || source == nil {
		return errors.New("cpa adapter: credential reconcile source is unavailable")
	}
	seen := make(map[string]struct{})
	if err := source.ForEachActive(ctx, func(value credential.RuntimeCredential) error {
		credentialID := value.CredentialID.String()
		if err := a.RegisterCredential(ctx, value); err != nil {
			return err
		}
		seen[credentialID] = struct{}{}
		return nil
	}); err != nil {
		return err
	}
	for _, record := range a.manager.List() {
		if record == nil || record.Attributes[auth.AttributeRuntimeOnly] != "true" || record.Attributes[auth.AttributeSourceBackend] != babloCredentialSource {
			continue
		}
		if _, ok := seen[record.ID]; !ok {
			a.manager.Remove(ctx, record.ID)
		}
	}
	return nil
}

// RemoveCredential removes a runtime credential without deleting PostgreSQL facts.
func (a *Adapter) RemoveCredential(ctx context.Context, credentialID string) error {
	if a == nil || a.manager == nil || strings.TrimSpace(credentialID) == "" {
		return errors.New("cpa adapter: credential runtime is unavailable")
	}
	a.manager.Remove(ctx, credentialID)
	return nil
}

// MarkCredentialResult accepts only a fully identified result for callers that
// execute outside CPA's Manager. Normal Manager execution already records its
// own result; Proxy does not wire this callback for CPA to avoid double-counting
// and accidentally clearing Manager quota state.
func (a *Adapter) MarkCredentialResult(ctx context.Context, result inference.CredentialResult) {
	if a == nil || a.manager == nil || strings.TrimSpace(result.CredentialID) == "" {
		return
	}
	provider := strings.TrimSpace(result.Provider)
	model := strings.TrimSpace(result.Model)
	if provider == "" || model == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var retryAfter *time.Duration
	if result.CooldownUntil != nil {
		duration := time.Until(result.CooldownUntil.UTC())
		if duration > 0 {
			retryAfter = &duration
		}
	}
	var resultError *auth.Error
	if !result.Succeeded {
		resultError = &auth.Error{
			Code:       strings.TrimSpace(result.ErrorClass),
			Message:    strings.TrimSpace(result.ErrorClass),
			HTTPStatus: result.HTTPStatus,
		}
		if resultError.Code == "" {
			resultError.Code = "credential_failure"
			resultError.Message = "credential execution failed"
		}
	}
	a.manager.MarkResult(ctx, auth.Result{
		AuthID:               result.CredentialID,
		Provider:             provider,
		Model:                model,
		RouteModel:           strings.TrimSpace(result.RouteModel),
		Success:              result.Succeeded,
		RetryAfter:           retryAfter,
		Error:                resultError,
		SkipQuotaObservation: true,
	})
}
