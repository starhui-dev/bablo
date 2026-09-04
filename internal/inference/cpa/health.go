package cpa

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/starhui-dev/bablo/internal/quota"
)

const runtimeHealthProbeName = "cpa-runtime-health"

var _ quota.HealthProbe = (*runtimeHealthProbe)(nil)

// NewHealthProbe exposes CPA manager lifecycle state through Bablo's health
// contract. It never calls an undocumented upstream endpoint.
func NewHealthProbe(adapter *Adapter) quota.HealthProbe {
	return &runtimeHealthProbe{adapter: adapter}
}

type runtimeHealthProbe struct {
	adapter *Adapter
}

func (p *runtimeHealthProbe) Name() string { return runtimeHealthProbeName }

func (p *runtimeHealthProbe) Supports(provider string) bool {
	if p == nil || p.adapter == nil || p.adapter.manager == nil {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	if coreauth.ProviderSupportsQuotaObservation(provider) {
		return true
	}
	for _, configured := range p.adapter.providers {
		if strings.EqualFold(strings.TrimSpace(configured), provider) {
			return true
		}
	}
	for _, record := range p.adapter.manager.List() {
		if record != nil && strings.EqualFold(strings.TrimSpace(record.Provider), provider) {
			return true
		}
	}
	return false
}

func (p *runtimeHealthProbe) Probe(ctx context.Context, request quota.ProbeRequest) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if p == nil || p.adapter == nil || p.adapter.manager == nil || request.CredentialID == uuid.Nil {
		return &quota.ProbeError{Class: "credential_unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}
	if !p.Supports(request.ProviderSlug) {
		return quota.ErrNoObservation
	}
	record, ok := p.adapter.manager.GetByID(request.CredentialID.String())
	if !ok || record == nil {
		return &quota.ProbeError{Class: "credential_unavailable", HTTPStatus: http.StatusServiceUnavailable, Cause: quota.ErrNotFound}
	}
	if !strings.EqualFold(strings.TrimSpace(record.Provider), strings.TrimSpace(request.ProviderSlug)) {
		return &quota.ProbeError{Class: "credential_provider_mismatch", HTTPStatus: http.StatusBadRequest}
	}
	if record.Disabled || record.Status == coreauth.StatusDisabled {
		return &quota.ProbeError{Class: "credential_disabled", HTTPStatus: http.StatusForbidden}
	}
	if record.Status != coreauth.StatusActive {
		class := "credential_unready"
		if record.Status == coreauth.StatusError {
			class = "credential_error"
		}
		return runtimeAuthError(record, class, http.StatusServiceUnavailable)
	}

	now := time.Now().UTC()
	if record.Unavailable {
		if record.Quota.Exceeded {
			return runtimeAuthError(record, "quota", http.StatusTooManyRequests)
		}
		return runtimeAuthError(record, "credential_unavailable", http.StatusServiceUnavailable)
	}
	if !record.NextRetryAfter.IsZero() && now.Before(record.NextRetryAfter) {
		return runtimeAuthError(record, "credential_retry_cooldown", http.StatusServiceUnavailable)
	}
	if record.Quota.Exceeded && !record.Quota.NextRecoverAt.IsZero() && now.Before(record.Quota.NextRecoverAt) {
		return runtimeAuthError(record, "quota", http.StatusTooManyRequests)
	}
	return nil
}

func runtimeAuthError(record *coreauth.Auth, fallbackClass string, fallbackStatus int) error {
	if record == nil {
		return &quota.ProbeError{Class: fallbackClass, HTTPStatus: fallbackStatus}
	}
	class := strings.ToLower(strings.TrimSpace(fallbackClass))
	status := fallbackStatus
	if record.LastError != nil {
		if value := strings.ToLower(strings.TrimSpace(record.LastError.Code)); value != "" {
			class = value
		}
		if record.LastError.HTTPStatus >= 100 && record.LastError.HTTPStatus <= 599 {
			status = record.LastError.HTTPStatus
		}
	}
	if class == "" {
		class = "credential_unavailable"
	}
	if status < 100 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	var retryAfter time.Duration
	now := time.Now().UTC()
	if !record.NextRetryAfter.IsZero() && record.NextRetryAfter.After(now) {
		retryAfter = record.NextRetryAfter.Sub(now)
	}
	if !record.Quota.NextRecoverAt.IsZero() && record.Quota.NextRecoverAt.After(now) {
		quotaRetry := record.Quota.NextRecoverAt.Sub(now)
		if quotaRetry > retryAfter {
			retryAfter = quotaRetry
		}
	}
	return &quota.ProbeError{Class: class, HTTPStatus: status, RetryAfter: retryAfter}
}
