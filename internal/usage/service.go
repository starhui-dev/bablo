package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service validates domain inputs before delegating durable writes to the
// repository. It is the Recorder implementation used by the inference proxy.
type Service struct {
	repository *Repository
	now        func() time.Time
}

// NewService constructs a usage service. The optional clock exists for
// deterministic unit tests; production uses UTC wall time.
func NewService(repository *Repository, clocks ...func() time.Time) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("usage service requires a repository")
	}
	clock := func() time.Time { return time.Now().UTC() }
	if len(clocks) > 1 {
		return nil, fmt.Errorf("usage service accepts at most one clock")
	}
	if len(clocks) == 1 && clocks[0] != nil {
		clock = func() time.Time { return clocks[0]().UTC() }
	}
	return &Service{repository: repository, now: clock}, nil
}

// BeginRequest creates or reopens an idempotent request record.
func (s *Service) BeginRequest(ctx context.Context, input StartInput) (RequestHandle, error) {
	if s == nil || s.repository == nil {
		return RequestHandle{}, fmt.Errorf("%w: usage service is not initialized", ErrInvalidInput)
	}
	normalized, err := normalizeStartInput(input, s.now())
	if err != nil {
		return RequestHandle{}, err
	}
	return s.repository.BeginRequest(ctx, normalized)
}

// Finalize appends an immutable usage fact and closes its request record.
func (s *Service) Finalize(ctx context.Context, handle RequestHandle, input FinalizeInput) (Event, error) {
	if s == nil || s.repository == nil {
		return Event{}, fmt.Errorf("%w: usage service is not initialized", ErrInvalidInput)
	}
	normalized, err := normalizeFinalizeInput(handle, input, s.now())
	if err != nil {
		return Event{}, err
	}
	return s.repository.Finalize(ctx, handle, normalized)
}

// RecordReconciliation appends a late provider observation without mutating a
// UsageEvent or its original amount.
func (s *Service) RecordReconciliation(ctx context.Context, input ReconciliationInput) (Reconciliation, error) {
	if s == nil || s.repository == nil {
		return Reconciliation{}, fmt.Errorf("%w: usage service is not initialized", ErrInvalidInput)
	}
	normalized, err := normalizeReconciliationInput(input)
	if err != nil {
		return Reconciliation{}, err
	}
	return s.repository.RecordReconciliation(ctx, normalized)
}

// ClaimOutbox atomically leases pending or stale events.
func (s *Service) ClaimOutbox(ctx context.Context, workerID string, limit int, lease time.Duration) ([]OutboxEvent, error) {
	if s == nil || s.repository == nil {
		return nil, fmt.Errorf("%w: usage service is not initialized", ErrInvalidInput)
	}
	workerID, err := normalizeWorkerID(workerID)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 32
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("%w: outbox limit must be between 1 and 100", ErrInvalidInput)
	}
	if lease == 0 {
		lease = 30 * time.Second
	}
	if lease < time.Second || lease > 24*time.Hour {
		return nil, fmt.Errorf("%w: outbox lease must be between 1 second and 24 hours", ErrInvalidInput)
	}
	return s.repository.ClaimOutbox(ctx, workerID, limit, lease)
}

// MarkOutboxPublished acknowledges a leased event.
func (s *Service) MarkOutboxPublished(ctx context.Context, eventID uuid.UUID, workerID string, publishedAt time.Time) error {
	if s == nil || s.repository == nil {
		return fmt.Errorf("%w: usage service is not initialized", ErrInvalidInput)
	}
	workerID, err := normalizeWorkerID(workerID)
	if err != nil {
		return err
	}
	if eventID == uuid.Nil {
		return fmt.Errorf("%w: outbox event id is required", ErrInvalidInput)
	}
	if publishedAt.IsZero() {
		publishedAt = s.now()
	}
	return s.repository.MarkOutboxPublished(ctx, eventID, workerID, publishedAt.UTC())
}

// MarkOutboxFailed releases a leased event for retry or records a permanent
// delivery failure. errorClass is a short sanitized classification only.
func (s *Service) MarkOutboxFailed(ctx context.Context, eventID uuid.UUID, workerID, errorClass string, retryAt time.Time, permanent bool) error {
	if s == nil || s.repository == nil {
		return fmt.Errorf("%w: usage service is not initialized", ErrInvalidInput)
	}
	workerID, err := normalizeWorkerID(workerID)
	if err != nil {
		return err
	}
	errorClass = normalizeText(errorClass, 128)
	if errorClass == "" {
		return fmt.Errorf("%w: outbox error class is required", ErrInvalidInput)
	}
	if eventID == uuid.Nil {
		return fmt.Errorf("%w: outbox event id is required", ErrInvalidInput)
	}
	if retryAt.IsZero() {
		retryAt = s.now()
	}
	return s.repository.MarkOutboxFailed(ctx, eventID, workerID, errorClass, retryAt.UTC(), permanent)
}

func normalizeStartInput(input StartInput, now time.Time) (StartInput, error) {
	if input.RequestID != strings.TrimSpace(input.RequestID) || normalizeText(input.RequestID, 128) == "" {
		return StartInput{}, fmt.Errorf("%w: request id is required and must be a single-line value", ErrInvalidInput)
	}
	if input.Endpoint != strings.TrimSpace(input.Endpoint) || normalizeText(input.Endpoint, 128) == "" {
		return StartInput{}, fmt.Errorf("%w: endpoint is required", ErrInvalidInput)
	}
	if input.RequestedModel != strings.TrimSpace(input.RequestedModel) || normalizeText(input.RequestedModel, 128) == "" {
		return StartInput{}, fmt.Errorf("%w: requested model is required", ErrInvalidInput)
	}
	if input.StartedAt.IsZero() {
		input.StartedAt = now
	}
	input.StartedAt = input.StartedAt.UTC()
	return input, nil
}

func normalizeFinalizeInput(handle RequestHandle, input FinalizeInput, now time.Time) (FinalizeInput, error) {
	if handle.RecordID == uuid.Nil || handle.RequestID == "" {
		return FinalizeInput{}, fmt.Errorf("%w: request handle is required", ErrInvalidInput)
	}
	if !validStatus(input.TerminalStatus) {
		return FinalizeInput{}, fmt.Errorf("%w: invalid terminal status %q", ErrInvalidInput, input.TerminalStatus)
	}
	if input.Usage.InputTokens < 0 || input.Usage.OutputTokens < 0 || input.Usage.CacheReadTokens < 0 || input.Usage.CacheWriteTokens < 0 || input.Usage.ReasoningTokens < 0 {
		return FinalizeInput{}, fmt.Errorf("%w: token counts must not be negative", ErrInvalidInput)
	}
	if input.AmountMinor != nil && *input.AmountMinor < 0 {
		return FinalizeInput{}, fmt.Errorf("%w: amount must not be negative", ErrInvalidInput)
	}
	if input.Currency != "" {
		if len(input.Currency) != 3 || input.Currency != strings.ToUpper(input.Currency) || !asciiLetters(input.Currency) {
			return FinalizeInput{}, fmt.Errorf("%w: currency must be three uppercase ASCII letters", ErrInvalidInput)
		}
	}
	if (input.AmountMinor == nil) != (input.Currency == "") {
		return FinalizeInput{}, fmt.Errorf("%w: amount and currency must be provided together", ErrInvalidInput)
	}
	if input.AmountMinor != nil && *input.AmountMinor > 0 && input.WalletID == nil {
		return FinalizeInput{}, fmt.Errorf("%w: a positive charge requires a wallet", ErrInvalidInput)
	}
	if input.Latency < 0 {
		return FinalizeInput{}, fmt.Errorf("%w: latency must not be negative", ErrInvalidInput)
	}
	if input.TTFT != nil && *input.TTFT < 0 {
		return FinalizeInput{}, fmt.Errorf("%w: TTFT must not be negative", ErrInvalidInput)
	}
	input.Provenance = normalizeText(input.Provenance, 64)
	if input.Provenance == "" {
		if input.Estimated {
			input.Provenance = ProvenanceEstimated
		} else {
			input.Provenance = ProvenanceAdapter
		}
	}
	input.ErrorClass = normalizeText(input.ErrorClass, 128)
	if input.FinishedAt.IsZero() {
		input.FinishedAt = now
	}
	input.FinishedAt = input.FinishedAt.UTC()
	if !handle.StartedAt.IsZero() && input.FinishedAt.Before(handle.StartedAt) {
		input.FinishedAt = handle.StartedAt
	}
	input.ResolvedModelID = normalizeUUIDPtr(input.ResolvedModelID)
	input.ProviderID = normalizeUUIDPtr(input.ProviderID)
	input.ProviderModelID = normalizeUUIDPtr(input.ProviderModelID)
	input.RouteVersionID = normalizeUUIDPtr(input.RouteVersionID)
	input.CredentialID = normalizeUUIDPtr(input.CredentialID)
	input.PriceVersionID = normalizeUUIDPtr(input.PriceVersionID)
	input.WalletID = normalizeUUIDPtr(input.WalletID)
	input.AmountMinor = cloneInt64Ptr(input.AmountMinor)
	input.UpstreamStatus = cloneIntPtr(input.UpstreamStatus)
	if input.UpstreamStatus != nil && (*input.UpstreamStatus < 100 || *input.UpstreamStatus > 599) {
		return FinalizeInput{}, fmt.Errorf("%w: upstream status must be between 100 and 599", ErrInvalidInput)
	}
	if input.Attempt != nil {
		attempt := *input.Attempt
		if attempt.AttemptNo < 0 {
			return FinalizeInput{}, fmt.Errorf("%w: attempt number must not be negative", ErrInvalidInput)
		}
		attempt.RouteVersionID = normalizeUUIDPtr(attempt.RouteVersionID)
		attempt.ProviderID = normalizeUUIDPtr(attempt.ProviderID)
		attempt.ProviderModelID = normalizeUUIDPtr(attempt.ProviderModelID)
		attempt.CredentialID = normalizeUUIDPtr(attempt.CredentialID)
		if attempt.StartedAt.IsZero() {
			attempt.StartedAt = handle.StartedAt
		}
		if attempt.StartedAt.IsZero() {
			attempt.StartedAt = input.FinishedAt
		}
		attempt.StartedAt = attempt.StartedAt.UTC()
		if attempt.StartedAt.After(input.FinishedAt) {
			attempt.StartedAt = input.FinishedAt
		}
		input.Attempt = &attempt
	}
	if input.TTFT != nil {
		value := *input.TTFT
		input.TTFT = &value
	}
	return input, nil
}

func normalizeReconciliationInput(input ReconciliationInput) (ReconciliationInput, error) {
	if input.UsageEventID == uuid.Nil {
		return ReconciliationInput{}, fmt.Errorf("%w: usage event id is required", ErrInvalidInput)
	}
	if input.Source != strings.TrimSpace(input.Source) || normalizeText(input.Source, 64) == "" {
		return ReconciliationInput{}, fmt.Errorf("%w: reconciliation source is required", ErrInvalidInput)
	}
	if input.SourceEventKey != strings.TrimSpace(input.SourceEventKey) || normalizeText(input.SourceEventKey, 256) == "" {
		return ReconciliationInput{}, fmt.Errorf("%w: reconciliation source event key is required", ErrInvalidInput)
	}
	return input, nil
}

func normalizeWorkerID(value string) (string, error) {
	if value != strings.TrimSpace(value) || normalizeText(value, 128) == "" {
		return "", fmt.Errorf("%w: worker id is required", ErrInvalidInput)
	}
	return value, nil
}

func normalizeUUIDPtr(value *uuid.UUID) *uuid.UUID {
	return cloneUUIDPtr(value)
}

func asciiLetters(value string) bool {
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}
