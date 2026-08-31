// Package scheduler selects individual credentials from route targets.
//
// Scheduler owns eligibility, deterministic selection, transient coordination,
// and durable decision records. It never handles credential secret values.
package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/route"
)

// Strategy controls deterministic candidate ordering.
type Strategy string

const (
	StrategyRoundRobin         Strategy = "round_robin"
	StrategyWeightedRoundRobin Strategy = "weighted_round_robin"
	StrategyFillFirst          Strategy = "fill_first"
	StrategyQuotaAware         Strategy = "quota_aware"
)

const (
	defaultLeaseTTL     = 30 * time.Second
	defaultAffinityTTL  = 5 * time.Minute
	defaultCursorTTL    = time.Hour
	defaultQuotaMaxAge  = 2 * time.Minute
	strategyVersionBase = "p0/v1"
)

var (
	ErrInvalidInput     = errors.New("invalid scheduler input")
	ErrConflict         = errors.New("scheduler decision conflict")
	ErrNoEligible       = errors.New("no eligible credential")
	ErrLeaseBusy        = errors.New("credential concurrency lease is busy")
	ErrLeaseNotOwner    = errors.New("credential concurrency lease is not owned")
	ErrStateUnavailable = errors.New("scheduler coordination state unavailable")
)

// QuotaPolicy controls optional quota snapshot enforcement. Disabled policies
// do not query quota snapshots; enabled conservative policies reject missing or
// stale snapshots instead of treating unknown capacity as available.
type QuotaPolicy struct {
	Enabled        bool
	WindowKind     string
	RequiredTokens int64
	MaxAge         time.Duration
	RequireFresh   bool
}

// Request contains a fixed route snapshot and request-level requirements.
// Resolution must be produced before Scheduler is called; Scheduler never
// resolves aliases or changes route versions.
type Request struct {
	RequestID            string
	RequestRecordID      *uuid.UUID
	AttemptNo            int
	DecisionNo           int
	Resolution           route.Resolution
	Protocol             string
	RequiredCapabilities model.Capabilities
	Region               string
	ProxyRef             string
	RequireProxy         bool
	Quota                QuotaPolicy
	Strategy             Strategy
	AffinityKey          string
	Now                  time.Time
}

// CandidateDecision is sanitized metadata persisted in scheduler_decisions.
// Credential IDs are internal identifiers, not secret material.
type CandidateDecision struct {
	TargetID        uuid.UUID  `json:"target_id"`
	CredentialID    *uuid.UUID `json:"credential_id,omitempty"`
	TargetPriority  int        `json:"target_priority"`
	MemberPriority  int        `json:"member_priority,omitempty"`
	Weight          int64      `json:"weight,omitempty"`
	Score           int64      `json:"score,omitempty"`
	Eligible        bool       `json:"eligible"`
	Reasons         []string   `json:"reasons,omitempty"`
	QuotaRemaining  *int64     `json:"quota_remaining,omitempty"`
	QuotaResetAt    *time.Time `json:"quota_reset_at,omitempty"`
	QuotaObservedAt *time.Time `json:"quota_observed_at,omitempty"`
}

// FallbackEntry records candidates that were tried after selection ordering
// required a fallback, or why an affinity binding could not be used.
type FallbackEntry struct {
	TargetID     *uuid.UUID `json:"target_id,omitempty"`
	CredentialID *uuid.UUID `json:"credential_id,omitempty"`
	Reason       string     `json:"reason"`
}

// Decision is the durable, secret-free explanation of one scheduler attempt.
type Decision struct {
	StrategyVersion      string              `json:"strategy_version"`
	Candidates           []CandidateDecision `json:"candidates"`
	SelectedTargetID     *uuid.UUID          `json:"selected_target_id,omitempty"`
	SelectedCredentialID *uuid.UUID          `json:"selected_credential_id,omitempty"`
	SelectedProviderID   *uuid.UUID          `json:"selected_provider_id,omitempty"`
	FallbackChain        []FallbackEntry     `json:"fallback_chain"`
}

// Selection contains the selected route target, credential identity, and an
// acquired lease. The caller must Release the lease in a finally/defer path.
type Selection struct {
	Target       route.Target
	CredentialID uuid.UUID
	Lease        Lease
	Decision     Decision
}

// Release releases the selection lease. Lease implementations are idempotent
// for the owning token; expiry remains the crash-recovery boundary.
func (s *Selection) Release(ctx context.Context) error {
	if s == nil || s.Lease == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.Lease.Release(context.WithoutCancel(ctx))
}

// Renew extends the selected lease for long-running or streaming requests.
func (s *Selection) Renew(ctx context.Context, ttl time.Duration) error {
	if s == nil || s.Lease == nil {
		return ErrLeaseNotOwner
	}
	return s.Lease.Renew(ctx, ttl)
}

// Lease is an ownership-token protected, TTL-bounded concurrency slot.
type Lease interface {
	Renew(context.Context, time.Duration) error
	Release(context.Context) error
}

// Coordinator stores only rebuildable scheduler state.
type Coordinator interface {
	Acquire(context.Context, string, string, time.Duration) (Lease, error)
	Next(context.Context, string, int64, time.Duration) (int64, error)
	GetAffinity(context.Context, string) (uuid.UUID, bool, error)
	SetAffinity(context.Context, string, uuid.UUID, time.Duration) error
	Close() error
}

// DecisionLogger persists an immutable scheduler explanation.
type DecisionLogger interface {
	RecordDecision(context.Context, Request, Decision) error
}

type poolMember struct {
	CredentialID    uuid.UUID
	MemberPriority  int
	MemberWeight    int
	Enabled         bool
	ProviderEnabled bool
	Status          string
	MaxConcurrency  int
	Region          string
	ProxyRef        string
	CooldownUntil   *time.Time
	Quota           quotaSnapshot
}

type quotaSnapshot struct {
	Found      bool
	Remaining  *int64
	ResetAt    *time.Time
	ObservedAt *time.Time
	Confidence string
}

type candidate struct {
	target         route.Target
	member         poolMember
	decision       CandidateDecision
	leaseKey       string
	combinedWeight int64
	decisionIndex  int
}
