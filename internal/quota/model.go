// Package quota owns provider quota/health observations and their durable
// PostgreSQL projection. It never exposes upstream SDK types.
package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	WindowMinute           = "minute"
	WindowHour             = "hour"
	WindowDay              = "day"
	WindowMonth            = "month"
	WindowProviderSpecific = "provider_specific"

	ConfidenceHigh    = "high"
	ConfidenceMedium  = "medium"
	ConfidenceLow     = "low"
	ConfidenceUnknown = "unknown"

	ProbeStatusUnknown       = "unknown"
	ProbeStatusSuccess       = "success"
	ProbeStatusNoObservation = "no_observation"
	ProbeStatusError         = "error"
	ProbeStatusUnsupported   = "unsupported"

	SourceCPAResponseHeaders = "cpa_response_headers"
)

var (
	ErrInvalidInput     = errors.New("invalid quota input")
	ErrNotFound         = errors.New("quota credential not found")
	ErrConflict         = errors.New("quota observation conflict")
	ErrUnsupportedProbe = errors.New("quota probe is unsupported")
	ErrNoObservation    = errors.New("quota probe returned no observation")
	ErrProbeBusy        = errors.New("quota probe is already running")
	ErrStateUnavailable = errors.New("quota coordination state unavailable")
	ErrProbeUnavailable = errors.New("quota probe is unavailable")
)

// ProbeRequest identifies the credential and route context for an observation.
// Model and ObservationKey are optional for credential-level active probes;
// passive request observers should populate both when available.
type ProbeRequest struct {
	CredentialID   uuid.UUID
	ProviderSlug   string
	Model          string
	ObservationKey string
}

// Window is one normalized quota window. Nil token fields mean that the
// provider exposed no token count; callers must not infer one from percentages
// or opaque provider signals.
type Window struct {
	Kind            string
	UsedTokens      *int64
	RemainingTokens *int64
	LimitTokens     *int64
	ResetAt         *time.Time
	Metadata        map[string]string
}

// Observation is a bounded, provider-normalized snapshot. It contains only
// metadata and quota watermarks, never prompt or response bodies.
type Observation struct {
	ObservedAt     time.Time
	Source         string
	Confidence     string
	ObservationKey string
	Windows        []Window
	Metadata       map[string]string
	ErrorClass     string
}

// Snapshot is the durable immutable observation returned to administrators and
// consumed indirectly by Scheduler through quota_snapshots.
type Snapshot struct {
	ID              uuid.UUID         `json:"id"`
	CredentialID    uuid.UUID         `json:"credential_id"`
	ProviderSlug    string            `json:"provider_slug"`
	Model           string            `json:"model"`
	ObservationKey  string            `json:"observation_key"`
	WindowKind      string            `json:"window_kind"`
	UsedTokens      *int64            `json:"used_tokens,omitempty"`
	RemainingTokens *int64            `json:"remaining_tokens,omitempty"`
	LimitTokens     *int64            `json:"limit_tokens,omitempty"`
	ResetAt         *time.Time        `json:"reset_at,omitempty"`
	ObservedAt      time.Time         `json:"observed_at"`
	Source          string            `json:"source"`
	Confidence      string            `json:"confidence"`
	ErrorClass      string            `json:"error_class,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Stale           bool              `json:"stale"`
}

// ProbeState is rebuildable scheduling state for one credential's probe worker.
type ProbeState struct {
	CredentialID   uuid.UUID  `json:"credential_id"`
	ProviderSlug   string     `json:"provider_slug"`
	ProbeName      string     `json:"probe_name"`
	Status         string     `json:"status"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	LastObservedAt *time.Time `json:"last_observed_at,omitempty"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`
	FailureCount   int        `json:"failure_count"`
	LastErrorClass string     `json:"last_error_class,omitempty"`
	LastHTTPStatus int        `json:"last_http_status,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// View combines the latest durable snapshots with probe state for the admin
// endpoint. StateFound distinguishes an absent state row from an unknown state.
type View struct {
	CredentialID    uuid.UUID  `json:"credential_id"`
	ProviderSlug    string     `json:"provider_slug"`
	State           ProbeState `json:"state"`
	StateFound      bool       `json:"state_found"`
	Snapshots       []Snapshot `json:"snapshots"`
	SupportedProbes []string   `json:"supported_probes"`
}

// DueCredential is a credential eligible for one bounded worker attempt.
type DueCredential struct {
	Request       ProbeRequest
	NextAttemptAt *time.Time
}

// QuotaProbe performs a provider-specific, legal quota observation. A probe
// must honor context cancellation and return ErrNoObservation when no verified
// quota signal is available; it must not guess undocumented endpoints.
type QuotaProbe interface {
	Name() string
	Supports(provider string) bool
	Probe(context.Context, ProbeRequest) (Observation, error)
}

// HealthProbe performs an explicit provider health check within the same legal
// integration boundary as QuotaProbe.
type HealthProbe interface {
	Name() string
	Supports(provider string) bool
	Probe(context.Context, ProbeRequest) error
}

// ResponseObserver converts safe upstream response headers into an Observation.
// It is called after a real inference response and must be passive: it may not
// issue another upstream request.
type ResponseObserver interface {
	Name() string
	Supports(provider string) bool
	ObserveResponse(context.Context, ProbeRequest, map[string][]string, time.Time) (Observation, error)
}

// Viewer exposes read-only quota facts to the administrator surface.
// Implementations must never return prompt, response, or secret material.
type Viewer interface {
	View(context.Context, uuid.UUID, string, int) (View, error)
}

// HealthInput is the sanitized health fact passed to the Credential service.
type HealthInput struct {
	Succeeded     bool
	ErrorClass    string
	CooldownUntil *time.Time
	ObservedAt    time.Time
	Metadata      map[string]string
}

// HealthReporter persists health feedback without coupling this package to the
// credential package.
type HealthReporter interface {
	RecordHealth(context.Context, uuid.UUID, HealthInput) error
}

// Lease is an owner-token protected, TTL-bounded probe lease.
type Lease interface {
	Release(context.Context) error
}

// Locker prevents concurrent probes for the same credential across instances.
type Locker interface {
	Acquire(context.Context, string, string, time.Duration) (Lease, error)
	Close() error
}

// ProbeError carries a stable class/status and optional Retry-After hint. Its
// message deliberately excludes upstream response bodies and credentials.
type ProbeError struct {
	Class      string
	HTTPStatus int
	RetryAfter time.Duration
	Cause      error
}

func (e *ProbeError) Error() string {
	if e == nil {
		return "quota probe failed"
	}
	class := strings.TrimSpace(e.Class)
	if class == "" {
		class = "probe_error"
	}
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("quota probe %s (status %d)", class, e.HTTPStatus)
	}
	return "quota probe " + class
}

func (e *ProbeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func validWindowKind(value string) bool {
	switch strings.TrimSpace(value) {
	case WindowMinute, WindowHour, WindowDay, WindowMonth, WindowProviderSpecific:
		return true
	default:
		return false
	}
}

func validConfidence(value string) bool {
	switch strings.TrimSpace(value) {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow, ConfidenceUnknown:
		return true
	default:
		return false
	}
}

func validProbeStatus(value string) bool {
	switch strings.TrimSpace(value) {
	case ProbeStatusUnknown, ProbeStatusSuccess, ProbeStatusNoObservation, ProbeStatusError, ProbeStatusUnsupported:
		return true
	default:
		return false
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := value.UTC()
	return &copyValue
}

func cloneWindow(value Window) Window {
	result := value
	result.UsedTokens = cloneInt64(value.UsedTokens)
	result.RemainingTokens = cloneInt64(value.RemainingTokens)
	result.LimitTokens = cloneInt64(value.LimitTokens)
	result.ResetAt = cloneTime(value.ResetAt)
	result.Metadata = cloneStringMap(value.Metadata)
	return result
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
