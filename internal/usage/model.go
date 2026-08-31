// Package usage owns durable request, usage, reconciliation, and outbox facts.
// It never stores prompt or response bodies.
package usage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TerminalStatus is the durable terminal state of one logical request's usage
// fact. reconcile_needed is deliberately distinct from succeeded: the request
// may have completed, but token usage was not verified.
type TerminalStatus string

const (
	StatusSucceeded       TerminalStatus = "succeeded"
	StatusFailed          TerminalStatus = "failed"
	StatusCancelled       TerminalStatus = "cancelled"
	StatusReconcileNeeded TerminalStatus = "reconcile_needed"
)

const (
	ProvenanceAdapter        = "adapter"
	ProvenanceMissingUsage   = "missing_usage"
	ProvenanceEstimated      = "estimated"
	ProvenanceReconciliation = "reconciliation"
)

const (
	outboxEventUsageRecorded      = "usage.recorded"
	outboxEventUsageReconciled    = "usage.reconciliation.recorded"
	outboxAggregateUsage          = "usage_event"
	outboxAggregateReconciliation = "usage_reconciliation"
)

var (
	ErrInvalidInput         = errors.New("invalid usage input")
	ErrNotFound             = errors.New("usage fact not found")
	ErrConflict             = errors.New("usage fact conflict")
	ErrRequestAlreadyClosed = errors.New("usage request is already finalized")
	ErrOutboxNotOwned       = errors.New("outbox event is not owned by worker")
	ErrOutboxInvalidState   = errors.New("outbox event is in an invalid state")
)

// TokenUsage is the provider-reported token breakdown. A zero value means that
// no verified usage was observed; it must not be silently treated as free.
type TokenUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

// StartInput creates or reopens one request fact using request_id as the
// client-visible idempotency boundary.
type StartInput struct {
	RequestID      string
	UserID         uuid.UUID
	APIKeyID       uuid.UUID
	Endpoint       string
	RequestedModel string
	Stream         bool
	StartedAt      time.Time
}

// RequestHandle identifies the PostgreSQL request record used by later
// scheduler, attempt, and usage writes.
type RequestHandle struct {
	RecordID       uuid.UUID
	RequestID      string
	UserID         uuid.UUID
	APIKeyID       uuid.UUID
	Endpoint       string
	RequestedModel string
	Stream         bool
	StartedAt      time.Time
	TerminalStatus TerminalStatus
}

// RecordIDPointer returns a defensive pointer suitable for scheduler input.
func (h RequestHandle) RecordIDPointer() *uuid.UUID {
	if h.RecordID == uuid.Nil {
		return nil
	}
	value := h.RecordID
	return &value
}

// AttemptInput captures the selected route/credential and final upstream
// observation. Attempt rows are mutable trace state; the UsageEvent remains
// immutable.
type AttemptInput struct {
	AttemptNo       int
	RouteVersionID  *uuid.UUID
	ProviderID      *uuid.UUID
	ProviderModelID *uuid.UUID
	CredentialID    *uuid.UUID
	StartedAt       time.Time
}

// FinalizeInput contains the one immutable terminal usage fact to append.
type FinalizeInput struct {
	Attempt         *AttemptInput
	ResolvedModelID *uuid.UUID
	ProviderID      *uuid.UUID
	ProviderModelID *uuid.UUID
	RouteVersionID  *uuid.UUID
	CredentialID    *uuid.UUID
	PriceVersionID  *uuid.UUID
	WalletID        *uuid.UUID
	Usage           TokenUsage
	AmountMinor     *int64
	Currency        string
	Estimated       bool
	Provenance      string
	TerminalStatus  TerminalStatus
	UpstreamStatus  *int
	ErrorClass      string
	Latency         time.Duration
	TTFT            *time.Duration
	FinishedAt      time.Time
}

// Event is the immutable usage fact returned after a successful append.
type Event struct {
	ID              uuid.UUID      `json:"id"`
	SettlementKey   string         `json:"settlement_key"`
	RequestRecordID *uuid.UUID     `json:"request_record_id,omitempty"`
	RequestID       string         `json:"request_id"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      time.Time      `json:"finished_at"`
	RequestedModel  string         `json:"requested_model"`
	UserID          *uuid.UUID     `json:"user_id,omitempty"`
	APIKeyID        *uuid.UUID     `json:"api_key_id,omitempty"`
	ResolvedModelID *uuid.UUID     `json:"resolved_model_id,omitempty"`
	ProviderID      *uuid.UUID     `json:"provider_id,omitempty"`
	ProviderModelID *uuid.UUID     `json:"provider_model_id,omitempty"`
	RouteVersionID  *uuid.UUID     `json:"route_version_id,omitempty"`
	CredentialID    *uuid.UUID     `json:"credential_id,omitempty"`
	PriceVersionID  *uuid.UUID     `json:"price_version_id,omitempty"`
	WalletID        *uuid.UUID     `json:"wallet_id,omitempty"`
	Usage           TokenUsage     `json:"usage"`
	AmountMinor     *int64         `json:"amount_minor,omitempty"`
	Currency        string         `json:"currency,omitempty"`
	Estimated       bool           `json:"estimated"`
	Provenance      string         `json:"provenance"`
	TerminalStatus  TerminalStatus `json:"terminal_status"`
	UpstreamStatus  *int           `json:"upstream_status,omitempty"`
	ErrorClass      string         `json:"error_class,omitempty"`
	Latency         time.Duration  `json:"-"`
	LatencyMS       *int64         `json:"latency_ms,omitempty"`
	TTFT            time.Duration  `json:"-"`
	TTFTMS          *int64         `json:"ttft_ms,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

// ReconciliationInput records a late provider/CPA observation without
// modifying the original UsageEvent.
type ReconciliationInput struct {
	UsageEventID      uuid.UUID
	Source            string
	SourceEventKey    string
	InputTokensDelta  int64
	OutputTokensDelta int64
	CacheReadDelta    int64
	CacheWriteDelta   int64
	ReasoningDelta    int64
	AmountMinorDelta  int64
}

// Reconciliation is the immutable append-only correction signal.
type Reconciliation struct {
	ID                uuid.UUID `json:"id"`
	UsageEventID      uuid.UUID `json:"usage_event_id"`
	Source            string    `json:"source"`
	SourceEventKey    string    `json:"source_event_key"`
	InputTokensDelta  int64     `json:"input_tokens_delta"`
	OutputTokensDelta int64     `json:"output_tokens_delta"`
	CacheReadDelta    int64     `json:"cache_read_tokens_delta"`
	CacheWriteDelta   int64     `json:"cache_write_tokens_delta"`
	ReasoningDelta    int64     `json:"reasoning_tokens_delta"`
	AmountMinorDelta  int64     `json:"amount_minor_delta"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

// OutboxEvent is a claimable transactional-outbox row. Payloads contain only
// sanitized metadata and never request/response bodies or credentials.
type OutboxEvent struct {
	ID             uuid.UUID  `json:"id"`
	AggregateType  string     `json:"aggregate_type"`
	AggregateID    uuid.UUID  `json:"aggregate_id"`
	EventType      string     `json:"event_type"`
	IdempotencyKey string     `json:"idempotency_key"`
	Payload        []byte     `json:"payload"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	ClaimedAt      *time.Time `json:"claimed_at,omitempty"`
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
	LastErrorClass string     `json:"last_error_class,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Recorder is the narrow boundary consumed by the inference proxy.
type Recorder interface {
	BeginRequest(context.Context, StartInput) (RequestHandle, error)
	Finalize(context.Context, RequestHandle, FinalizeInput) (Event, error)
	RecordReconciliation(context.Context, ReconciliationInput) (Reconciliation, error)
}

func validStatus(value TerminalStatus) bool {
	switch value {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusReconcileNeeded:
		return true
	default:
		return false
	}
}

func normalizeText(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}
