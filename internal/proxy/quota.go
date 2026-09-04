package proxy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/inference"
	"github.com/starhui-dev/bablo/internal/quota"
	"github.com/starhui-dev/bablo/internal/scheduler"
)

const quotaObservationTimeout = 2 * time.Second

// observeQuota persists only allowlisted provider headers. It deliberately uses
// an independent bounded context so a client disconnect cannot discard a fact
// already returned by the upstream, while a slow database cannot hold a request
// indefinitely.
func (h *Handler) observeQuota(ctx context.Context, selection scheduler.Selection, request inference.Request, headers map[string][]string, observedAt time.Time) {
	if h == nil || h.quota == nil || selection.CredentialID == uuid.Nil {
		return
	}
	provider := strings.TrimSpace(request.ResolvedRoute.ProviderID)
	if provider == "" {
		provider = strings.TrimSpace(selection.Target.ProviderSlug)
	}
	if provider == "" {
		return
	}
	quotaRequest := quota.ProbeRequest{
		CredentialID:   selection.CredentialID,
		ProviderSlug:   provider,
		Model:          strings.TrimSpace(request.ResolvedRoute.ResolvedModel),
		ObservationKey: strings.TrimSpace(request.RequestID),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	observationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaObservationTimeout)
	defer cancel()
	if err := h.quota.ObserveResponse(observationCtx, quotaRequest, headers, observedAt); err != nil && !errors.Is(err, quota.ErrNoObservation) {
		if h.logger != nil {
			h.logger.Warn("proxy_quota_observation_error", "request_id", request.RequestID, "credential_id", selection.CredentialID, "error_class", errorClass(err))
		}
	}
}

func cloneObservedHeaders(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}
