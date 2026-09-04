package cpa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/starhui-dev/bablo/internal/quota"
)

const quotaObserverName = "cpa-response-headers"

var (
	_ quota.QuotaProbe       = (*Adapter)(nil)
	_ quota.ResponseObserver = (*Adapter)(nil)
)

// Name identifies the pinned CPA quota integration.
func (a *Adapter) Name() string { return quotaObserverName }

// Supports reports the exact provider allowlist exposed by CPA v7.2.149.
func (a *Adapter) Supports(provider string) bool {
	return coreauth.ProviderSupportsQuotaObservation(strings.ToLower(strings.TrimSpace(provider)))
}

// Probe reads the latest passive quota state held by CPA's public auth manager.
// It does not call an undocumented provider endpoint; idle credentials therefore
// correctly return ErrNoObservation.
func (a *Adapter) Probe(ctx context.Context, request quota.ProbeRequest) (quota.Observation, error) {
	if a == nil || a.manager == nil {
		return quota.Observation{}, quota.ErrProbeUnavailable
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return quota.Observation{}, err
		}
	}
	if request.CredentialID == uuid.Nil {
		return quota.Observation{}, quota.ErrInvalidInput
	}
	if !a.Supports(request.ProviderSlug) {
		return quota.Observation{}, quota.ErrNoObservation
	}
	authRecord, ok := a.manager.GetByID(request.CredentialID.String())
	if !ok || authRecord == nil {
		return quota.Observation{}, &quota.ProbeError{Class: "credential_unavailable", Cause: quota.ErrNotFound}
	}
	return observationFromQuotaState(request, authRecord.Quota)
}

// ObserveResponse converts safe response headers through the pinned CPA public
// allowlist. The observer is passive and never issues an additional request.
func (a *Adapter) ObserveResponse(ctx context.Context, request quota.ProbeRequest, headers map[string][]string, observedAt time.Time) (quota.Observation, error) {
	if a == nil {
		return quota.Observation{}, quota.ErrInvalidInput
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return quota.Observation{}, err
		}
	}
	if request.CredentialID == uuid.Nil {
		return quota.Observation{}, quota.ErrInvalidInput
	}
	if !a.Supports(request.ProviderSlug) {
		return quota.Observation{}, quota.ErrNoObservation
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	} else {
		observedAt = observedAt.UTC()
	}
	var state coreauth.QuotaState
	if !state.ObserveResponseHeadersForProvider(request.ProviderSlug, http.Header(headerValues(headers)), observedAt) {
		return quota.Observation{}, quota.ErrNoObservation
	}
	return observationFromQuotaState(request, state)
}

func observationFromQuotaState(request quota.ProbeRequest, state coreauth.QuotaState) (quota.Observation, error) {
	if state.ObservedAt.IsZero() || len(state.Signals) == 0 {
		return quota.Observation{}, quota.ErrNoObservation
	}
	signals := make(map[string]string, len(state.Signals))
	for key, value := range state.Signals {
		signals[key] = value
	}
	observationKey := strings.TrimSpace(request.ObservationKey)
	if observationKey == "" {
		observationKey = quotaObservationDigest(request, state)
	}
	return quota.Observation{
		ObservedAt:     state.ObservedAt.UTC(),
		Source:         quota.SourceCPAResponseHeaders,
		Confidence:     quota.ConfidenceHigh,
		ObservationKey: observationKey,
		Windows: []quota.Window{{
			Kind:     quota.WindowProviderSpecific,
			ResetAt:  earliestResetAt(signals, state.ObservedAt.UTC()),
			Metadata: signals,
		}},
		Metadata: map[string]string{
			"observer": quotaObserverName,
			"provider": strings.ToLower(strings.TrimSpace(request.ProviderSlug)),
			"model":    strings.TrimSpace(request.Model),
		},
	}, nil
}

func headerValues(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func earliestResetAt(signals map[string]string, observedAt time.Time) *time.Time {
	var earliest time.Time
	for key, value := range signals {
		lower := strings.ToLower(strings.TrimSpace(key))
		raw := strings.TrimSpace(value)
		var candidate time.Time
		if lower == "retry-after" {
			if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 && seconds <= int64((366*24*time.Hour)/time.Second) {
				candidate = observedAt.Add(time.Duration(seconds) * time.Second).UTC()
			} else if parsed, err := http.ParseTime(raw); err == nil {
				candidate = parsed.UTC()
			}
		} else {
			seconds, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || seconds < 0 {
				continue
			}
			switch {
			case strings.HasSuffix(lower, "-reset-after-seconds"):
				if seconds > int64((366*24*time.Hour)/time.Second) {
					continue
				}
				candidate = observedAt.Add(time.Duration(seconds) * time.Second).UTC()
			case strings.HasSuffix(lower, "-reset-at"), strings.HasSuffix(lower, "-reset"):
				candidate = time.Unix(seconds, 0).UTC()
			default:
				continue
			}
		}
		if candidate.Before(observedAt.Add(-time.Minute)) || candidate.After(observedAt.Add(366*24*time.Hour)) {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	if earliest.IsZero() {
		return nil
	}
	return &earliest
}

func quotaObservationDigest(request quota.ProbeRequest, state coreauth.QuotaState) string {
	keys := make([]string, 0, len(state.Signals))
	for key := range state.Signals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	_, _ = hash.Write([]byte(request.CredentialID.String()))
	_, _ = hash.Write([]byte("\x00" + request.ProviderSlug + "\x00" + request.Model))
	_, _ = hash.Write([]byte("\x00" + state.ObservedAt.UTC().Format(time.RFC3339Nano)))
	for _, key := range keys {
		_, _ = hash.Write([]byte("\x00" + key + "=" + state.Signals[key]))
	}
	return "header:" + hex.EncodeToString(hash.Sum(nil))
}
