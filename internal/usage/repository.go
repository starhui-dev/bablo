package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

// Repository persists usage facts and owns their transaction boundaries.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a usage repository backed by PostgreSQL.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("usage repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

// BeginRequest inserts a request record once and returns the existing record on
// an idempotent retry. Metadata mismatches for a reused request_id are rejected.
func (r *Repository) BeginRequest(ctx context.Context, input StartInput) (RequestHandle, error) {
	startedAt := input.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	recordID, err := id.New()
	if err != nil {
		return RequestHandle{}, fmt.Errorf("generate request UUIDv7: %w", err)
	}

	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	var result RequestHandle
	err = r.store.WithTx(dbCtx, func(q data.Querier) error {
		_, err := q.Exec(dbCtx, `
			INSERT INTO request_records (
				id, request_id, user_id, api_key_id, endpoint, requested_model, stream, started_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (request_id) DO NOTHING`,
			recordID,
			input.RequestID,
			uuidArg(input.UserID),
			uuidArg(input.APIKeyID),
			input.Endpoint,
			input.RequestedModel,
			input.Stream,
			startedAt,
		)
		if err != nil {
			return mapRepositoryError(err)
		}
		record, err := loadRequestRecord(dbCtx, q, input.RequestID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if !sameRequestMetadata(record, input) {
			return fmt.Errorf("%w: request_id metadata differs", ErrConflict)
		}
		result = record
		return nil
	})
	if err != nil {
		return RequestHandle{}, err
	}
	return result, nil
}

// Finalize appends one immutable UsageEvent, closes its request record, and
// inserts the corresponding transactional-outbox event in the same tx.
func (r *Repository) Finalize(ctx context.Context, handle RequestHandle, input FinalizeInput) (Event, error) {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	var result Event
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		record, err := loadRequestRecordByID(dbCtx, q, handle.RecordID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if record.RequestID != handle.RequestID {
			return fmt.Errorf("%w: request handle does not match record", ErrConflict)
		}
		if record.TerminalStatus != "" && record.TerminalStatus != input.TerminalStatus {
			return fmt.Errorf("%w: request already finalized as %s", ErrRequestAlreadyClosed, record.TerminalStatus)
		}

		settlementKey := settlementKey(handle.RequestID)
		existing, err := loadUsageEventBySettlementKey(dbCtx, q, settlementKey)
		switch {
		case err == nil:
			if !sameEventInput(existing, handle, input) {
				return fmt.Errorf("%w: settlement payload differs for request %s", ErrConflict, handle.RequestID)
			}
			if existing.RequestRecordID == nil || *existing.RequestRecordID != handle.RecordID {
				return fmt.Errorf("%w: settlement key belongs to another request", ErrConflict)
			}
			if err := closeRequestRecord(dbCtx, q, handle.RecordID, input.TerminalStatus, input.FinishedAt); err != nil {
				return err
			}
			if err := insertUsageOutbox(dbCtx, q, existing); err != nil {
				return err
			}
			result = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}

		// request_id is independently checked so a corrupted or legacy row
		// cannot produce two billable facts under different settlement keys.
		existingByRequest, err := loadUsageEventByRequestID(dbCtx, q, handle.RequestID)
		if err == nil {
			return fmt.Errorf("%w: request already has settlement %s", ErrConflict, existingByRequest.SettlementKey)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return mapRepositoryError(err)
		}

		eventID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate usage event UUIDv7: %w", err)
		}
		createdAt := time.Now().UTC()
		if !input.FinishedAt.IsZero() && input.FinishedAt.After(createdAt) {
			createdAt = input.FinishedAt
		}
		event := eventFromFinalize(eventID, settlementKey, handle, input, createdAt)
		var returnedCreatedAt pgtype.Timestamptz
		err = q.QueryRow(dbCtx, `
            INSERT INTO usage_events (
                id, settlement_key, request_record_id, request_id, user_id, api_key_id,
                requested_model, started_at, finished_at, resolved_model_id, provider_id, provider_model_id,
                route_version_id, credential_id, price_version_id, wallet_id, input_tokens,
                output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
                amount_minor, currency, estimated, provenance, terminal_status,
                upstream_status, error_class, latency_ms, ttft_ms, created_at
            )
            VALUES (
                $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
                $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
                $25, $26, $27, $28, $29, $30, $31
            )
            RETURNING created_at`,
			event.ID,
			event.SettlementKey,
			uuidPtrArg(event.RequestRecordID),
			event.RequestID,
			uuidPtrArg(event.UserID),
			uuidPtrArg(event.APIKeyID),
			event.RequestedModel,
			event.StartedAt,
			event.FinishedAt,
			uuidPtrArg(event.ResolvedModelID),
			uuidPtrArg(event.ProviderID),
			uuidPtrArg(event.ProviderModelID),
			uuidPtrArg(event.RouteVersionID),
			uuidPtrArg(event.CredentialID),
			uuidPtrArg(event.PriceVersionID),
			uuidPtrArg(event.WalletID),
			event.Usage.InputTokens,
			event.Usage.OutputTokens,
			event.Usage.CacheReadTokens,
			event.Usage.CacheWriteTokens,
			event.Usage.ReasoningTokens,
			intPtr64Arg(event.AmountMinor),
			stringArg(event.Currency),
			event.Estimated,
			event.Provenance,
			event.TerminalStatus,
			intPtrArg(event.UpstreamStatus),
			stringArg(event.ErrorClass),
			intPtr64Arg(event.LatencyMS),
			intPtr64Arg(event.TTFTMS),
			event.CreatedAt,
		).Scan(&returnedCreatedAt)
		if err != nil {
			return mapRepositoryError(err)
		}
		if returnedCreatedAt.Valid {
			event.CreatedAt = returnedCreatedAt.Time.UTC()
		}

		if input.Attempt != nil {
			if err := upsertAttempt(dbCtx, q, handle, input); err != nil {
				return err
			}
		}
		if err := closeRequestRecord(dbCtx, q, handle.RecordID, input.TerminalStatus, input.FinishedAt); err != nil {
			return err
		}
		if err := insertUsageOutbox(dbCtx, q, event); err != nil {
			return err
		}
		result = event
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return result, nil
}

// RecordReconciliation appends a late observation and emits a separate outbox
// fact. It never updates or deletes the original UsageEvent.
func (r *Repository) RecordReconciliation(ctx context.Context, input ReconciliationInput) (Reconciliation, error) {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	var result Reconciliation
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		var exists uuid.UUID
		if err := q.QueryRow(dbCtx, `SELECT id FROM usage_events WHERE id = $1`, input.UsageEventID).Scan(&exists); err != nil {
			return mapRepositoryError(err)
		}
		reconciliationID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate reconciliation UUIDv7: %w", err)
		}
		_, err = q.Exec(dbCtx, `
			INSERT INTO usage_reconciliations (
				id, usage_event_id, source, source_event_key,
				input_tokens_delta, output_tokens_delta, cache_read_tokens_delta,
				cache_write_tokens_delta, reasoning_tokens_delta, amount_minor_delta
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (source, source_event_key) DO NOTHING`,
			reconciliationID,
			input.UsageEventID,
			input.Source,
			input.SourceEventKey,
			input.InputTokensDelta,
			input.OutputTokensDelta,
			input.CacheReadDelta,
			input.CacheWriteDelta,
			input.ReasoningDelta,
			input.AmountMinorDelta,
		)
		if err != nil {
			return mapRepositoryError(err)
		}
		stored, err := loadReconciliation(dbCtx, q, input.Source, input.SourceEventKey)
		if err != nil {
			return mapRepositoryError(err)
		}
		if stored.UsageEventID != input.UsageEventID {
			return fmt.Errorf("%w: reconciliation key belongs to another usage event", ErrConflict)
		}
		if !sameReconciliationInput(stored, input) {
			return fmt.Errorf("%w: reconciliation payload differs for %s/%s", ErrConflict, input.Source, input.SourceEventKey)
		}
		if err := insertReconciliationOutbox(dbCtx, q, stored); err != nil {
			return err
		}
		result = stored
		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}
	return result, nil
}

// ClaimOutbox leases pending events and stale processing events atomically.
func (r *Repository) ClaimOutbox(ctx context.Context, workerID string, limit int, lease time.Duration) ([]OutboxEvent, error) {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	now := time.Now().UTC()
	cutoff := now.Add(-lease)
	var result []OutboxEvent
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		rows, err := q.Query(dbCtx, `
			WITH candidates AS (
				SELECT id
				FROM outbox_events
				WHERE (
					(status = 'pending' AND next_attempt_at <= $1)
                    OR (status = 'processing' AND (claimed_at IS NULL OR claimed_at < $2))
				)
				ORDER BY next_attempt_at, created_at, id
				FOR UPDATE SKIP LOCKED
				LIMIT $3
			)
			UPDATE outbox_events AS outbox
			SET status = 'processing', attempts = outbox.attempts + 1,
				claimed_at = $1, claimed_by = $4
			FROM candidates
			WHERE outbox.id = candidates.id
			RETURNING outbox.id, outbox.aggregate_type, outbox.aggregate_id,
				outbox.event_type, outbox.idempotency_key, outbox.payload,
				outbox.status, outbox.attempts, outbox.next_attempt_at,
				outbox.claimed_at, outbox.claimed_by, outbox.published_at,
				outbox.last_error_class, outbox.created_at`,
			now, cutoff, limit, workerID)
		if err != nil {
			return mapRepositoryError(err)
		}
		defer rows.Close()
		for rows.Next() {
			event, err := scanOutbox(rows)
			if err != nil {
				return fmt.Errorf("scan claimed outbox event: %w", err)
			}
			result = append(result, event)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate claimed outbox events: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// MarkOutboxPublished completes a lease only when workerID owns it.
func (r *Repository) MarkOutboxPublished(ctx context.Context, eventID uuid.UUID, workerID string, publishedAt time.Time) error {
	if publishedAt.IsZero() {
		publishedAt = time.Now().UTC()
	}
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	return r.store.WithTx(dbCtx, func(q data.Querier) error {
		tag, err := q.Exec(dbCtx, `
			UPDATE outbox_events
			SET status = 'published', published_at = $3, claimed_at = NULL, claimed_by = NULL
			WHERE id = $1 AND status = 'processing' AND claimed_by = $2`,
			eventID, workerID, publishedAt)
		if err != nil {
			return mapRepositoryError(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		return outboxOwnershipError(dbCtx, q, eventID, workerID)
	})
}

// MarkOutboxFailed releases a lease for retry, or permanently marks it failed.
func (r *Repository) MarkOutboxFailed(ctx context.Context, eventID uuid.UUID, workerID, errorClass string, retryAt time.Time, permanent bool) error {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	if retryAt.IsZero() {
		retryAt = time.Now().UTC()
	}
	status := "pending"
	if permanent {
		status = "failed"
	}
	return r.store.WithTx(dbCtx, func(q data.Querier) error {
		tag, err := q.Exec(dbCtx, `
			UPDATE outbox_events
			SET status = $3, next_attempt_at = $4, last_error_class = $5,
				claimed_at = NULL, claimed_by = NULL
			WHERE id = $1 AND status = 'processing' AND claimed_by = $2`,
			eventID, workerID, status, retryAt, stringArg(errorClass))
		if err != nil {
			return mapRepositoryError(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		return outboxOwnershipError(dbCtx, q, eventID, workerID)
	})
}

func loadRequestRecord(ctx context.Context, q data.Querier, requestID string, forUpdate bool) (RequestHandle, error) {
	query := `
		SELECT id, request_id, user_id, api_key_id, endpoint, requested_model,
			stream, started_at, finished_at, terminal_status
		FROM request_records WHERE request_id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanRequestRecord(q.QueryRow(ctx, query, requestID))
}

func loadRequestRecordByID(ctx context.Context, q data.Querier, recordID uuid.UUID, forUpdate bool) (RequestHandle, error) {
	query := `
		SELECT id, request_id, user_id, api_key_id, endpoint, requested_model,
			stream, started_at, finished_at, terminal_status
		FROM request_records WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanRequestRecord(q.QueryRow(ctx, query, recordID))
}

func scanRequestRecord(row interface{ Scan(...any) error }) (RequestHandle, error) {
	var result RequestHandle
	var userID, apiKeyID pgtype.UUID
	var startedAt, finishedAt pgtype.Timestamptz
	var terminal pgtype.Text
	if err := row.Scan(
		&result.RecordID,
		&result.RequestID,
		&userID,
		&apiKeyID,
		&result.Endpoint,
		&result.RequestedModel,
		&result.Stream,
		&startedAt,
		&finishedAt,
		&terminal,
	); err != nil {
		return RequestHandle{}, err
	}
	if userID.Valid {
		result.UserID = userID.Bytes
	}
	if apiKeyID.Valid {
		result.APIKeyID = apiKeyID.Bytes
	}
	if startedAt.Valid {
		result.StartedAt = startedAt.Time.UTC()
	}
	if terminal.Valid {
		result.TerminalStatus = TerminalStatus(terminal.String)
	}
	return result, nil
}

const usageEventColumns = `
 id, settlement_key, request_record_id, request_id, user_id, api_key_id,
 requested_model, started_at, finished_at, resolved_model_id, provider_id, provider_model_id,
 route_version_id, credential_id, price_version_id, wallet_id, input_tokens,
 output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
 amount_minor, currency, estimated, provenance, terminal_status,
 upstream_status, error_class, latency_ms, ttft_ms, created_at`

func loadUsageEventBySettlementKey(ctx context.Context, q data.Querier, key string) (Event, error) {
	return scanUsageEvent(q.QueryRow(ctx, `SELECT `+usageEventColumns+` FROM usage_events WHERE settlement_key = $1`, key))
}

func loadUsageEventByRequestID(ctx context.Context, q data.Querier, requestID string) (Event, error) {
	return scanUsageEvent(q.QueryRow(ctx, `SELECT `+usageEventColumns+` FROM usage_events WHERE request_id = $1`, requestID))
}

func scanUsageEvent(row interface{ Scan(...any) error }) (Event, error) {
	var result Event
	var requestRecordID, userID, apiKeyID, resolvedModelID, providerID, providerModelID, routeVersionID, credentialID, priceVersionID, walletID pgtype.UUID
	var startedAt, finishedAt pgtype.Timestamptz
	var amountMinor, latencyMS, ttftMS pgtype.Int8
	var currency, errorClass pgtype.Text
	var upstreamStatus pgtype.Int4
	if err := row.Scan(
		&result.ID,
		&result.SettlementKey,
		&requestRecordID,
		&result.RequestID,
		&userID,
		&apiKeyID,
		&result.RequestedModel,
		&startedAt,
		&finishedAt,
		&resolvedModelID,
		&providerID,
		&providerModelID,
		&routeVersionID,
		&credentialID,
		&priceVersionID,
		&walletID,
		&result.Usage.InputTokens,
		&result.Usage.OutputTokens,
		&result.Usage.CacheReadTokens,
		&result.Usage.CacheWriteTokens,
		&result.Usage.ReasoningTokens,
		&amountMinor,
		&currency,
		&result.Estimated,
		&result.Provenance,
		&result.TerminalStatus,
		&upstreamStatus,
		&errorClass,
		&latencyMS,
		&ttftMS,
		&result.CreatedAt,
	); err != nil {
		return Event{}, err
	}
	if startedAt.Valid {
		result.StartedAt = startedAt.Time.UTC()
	}
	if finishedAt.Valid {
		result.FinishedAt = finishedAt.Time.UTC()
	}
	result.RequestRecordID = nullableUUID(requestRecordID)
	result.UserID = nullableUUID(userID)
	result.APIKeyID = nullableUUID(apiKeyID)
	result.ResolvedModelID = nullableUUID(resolvedModelID)
	result.ProviderID = nullableUUID(providerID)
	result.ProviderModelID = nullableUUID(providerModelID)
	result.RouteVersionID = nullableUUID(routeVersionID)
	result.CredentialID = nullableUUID(credentialID)
	result.PriceVersionID = nullableUUID(priceVersionID)
	result.WalletID = nullableUUID(walletID)
	result.AmountMinor = nullableInt64(amountMinor)
	if currency.Valid {
		result.Currency = currency.String
	}
	if upstreamStatus.Valid {
		value := int(upstreamStatus.Int32)
		result.UpstreamStatus = &value
	}
	if errorClass.Valid {
		result.ErrorClass = errorClass.String
	}
	if latencyMS.Valid {
		value := latencyMS.Int64
		result.LatencyMS = &value
		result.Latency = time.Duration(value) * time.Millisecond
	}
	if ttftMS.Valid {
		value := ttftMS.Int64
		result.TTFTMS = &value
		result.TTFT = time.Duration(value) * time.Millisecond
	}
	result.CreatedAt = result.CreatedAt.UTC()
	return result, nil
}

func loadReconciliation(ctx context.Context, q data.Querier, source, sourceEventKey string) (Reconciliation, error) {
	var result Reconciliation
	if err := q.QueryRow(ctx, `
		SELECT id, usage_event_id, source, source_event_key,
			input_tokens_delta, output_tokens_delta, cache_read_tokens_delta,
			cache_write_tokens_delta, reasoning_tokens_delta, amount_minor_delta,
			status, created_at
		FROM usage_reconciliations
		WHERE source = $1 AND source_event_key = $2`, source, sourceEventKey).Scan(
		&result.ID,
		&result.UsageEventID,
		&result.Source,
		&result.SourceEventKey,
		&result.InputTokensDelta,
		&result.OutputTokensDelta,
		&result.CacheReadDelta,
		&result.CacheWriteDelta,
		&result.ReasoningDelta,
		&result.AmountMinorDelta,
		&result.Status,
		&result.CreatedAt,
	); err != nil {
		return Reconciliation{}, err
	}
	result.CreatedAt = result.CreatedAt.UTC()
	return result, nil
}

const outboxColumns = `
	id, aggregate_type, aggregate_id, event_type, idempotency_key, payload,
	status, attempts, next_attempt_at, claimed_at, claimed_by, published_at,
	last_error_class, created_at`

func scanOutbox(row interface{ Scan(...any) error }) (OutboxEvent, error) {
	var result OutboxEvent
	var claimedAt, publishedAt pgtype.Timestamptz
	var claimedBy, lastError pgtype.Text
	if err := row.Scan(
		&result.ID,
		&result.AggregateType,
		&result.AggregateID,
		&result.EventType,
		&result.IdempotencyKey,
		&result.Payload,
		&result.Status,
		&result.Attempts,
		&result.NextAttemptAt,
		&claimedAt,
		&claimedBy,
		&publishedAt,
		&lastError,
		&result.CreatedAt,
	); err != nil {
		return OutboxEvent{}, err
	}
	if claimedAt.Valid {
		value := claimedAt.Time.UTC()
		result.ClaimedAt = &value
	}
	if claimedBy.Valid {
		result.ClaimedBy = claimedBy.String
	}
	if publishedAt.Valid {
		value := publishedAt.Time.UTC()
		result.PublishedAt = &value
	}
	if lastError.Valid {
		result.LastErrorClass = lastError.String
	}
	result.NextAttemptAt = result.NextAttemptAt.UTC()
	result.CreatedAt = result.CreatedAt.UTC()
	return result, nil
}

func sameEventInput(existing Event, handle RequestHandle, input FinalizeInput) bool {
	expected := eventFromFinalize(existing.ID, existing.SettlementKey, handle, input, existing.CreatedAt)
	return equalUUIDPtr(existing.RequestRecordID, expected.RequestRecordID) &&
		existing.RequestID == expected.RequestID &&
		equalUUIDPtr(existing.UserID, expected.UserID) &&
		equalUUIDPtr(existing.APIKeyID, expected.APIKeyID) &&
		existing.RequestedModel == expected.RequestedModel &&
		sameInstant(existing.StartedAt, expected.StartedAt) &&
		sameInstant(existing.FinishedAt, expected.FinishedAt) &&
		equalUUIDPtr(existing.ResolvedModelID, expected.ResolvedModelID) &&
		equalUUIDPtr(existing.ProviderID, expected.ProviderID) &&
		equalUUIDPtr(existing.ProviderModelID, expected.ProviderModelID) &&
		equalUUIDPtr(existing.RouteVersionID, expected.RouteVersionID) &&
		equalUUIDPtr(existing.CredentialID, expected.CredentialID) &&
		equalUUIDPtr(existing.PriceVersionID, expected.PriceVersionID) &&
		equalUUIDPtr(existing.WalletID, expected.WalletID) &&
		existing.Usage == expected.Usage &&
		equalInt64Ptr(existing.AmountMinor, expected.AmountMinor) &&
		existing.Currency == expected.Currency &&
		existing.Estimated == expected.Estimated &&
		existing.Provenance == expected.Provenance &&
		existing.TerminalStatus == expected.TerminalStatus &&
		equalIntPtr(existing.UpstreamStatus, expected.UpstreamStatus) &&
		existing.ErrorClass == expected.ErrorClass &&
		equalInt64Ptr(existing.LatencyMS, expected.LatencyMS) &&
		equalInt64Ptr(existing.TTFTMS, expected.TTFTMS)
}

func sameReconciliationInput(stored Reconciliation, input ReconciliationInput) bool {
	return stored.UsageEventID == input.UsageEventID &&
		stored.Source == input.Source &&
		stored.SourceEventKey == input.SourceEventKey &&
		stored.InputTokensDelta == input.InputTokensDelta &&
		stored.OutputTokensDelta == input.OutputTokensDelta &&
		stored.CacheReadDelta == input.CacheReadDelta &&
		stored.CacheWriteDelta == input.CacheWriteDelta &&
		stored.ReasoningDelta == input.ReasoningDelta &&
		stored.AmountMinorDelta == input.AmountMinorDelta
}

func sameInstant(left, right time.Time) bool {
	if left.IsZero() || right.IsZero() {
		return left.IsZero() && right.IsZero()
	}
	return left.UTC().Truncate(time.Microsecond).Equal(right.UTC().Truncate(time.Microsecond))
}

func equalUUIDPtr(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalInt64Ptr(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalIntPtr(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameRequestMetadata(record RequestHandle, input StartInput) bool {
	return record.RequestID == input.RequestID &&
		record.UserID == input.UserID &&
		record.APIKeyID == input.APIKeyID &&
		record.Endpoint == input.Endpoint &&
		record.RequestedModel == input.RequestedModel &&
		record.Stream == input.Stream
}

func eventFromFinalize(eventID uuid.UUID, key string, handle RequestHandle, input FinalizeInput, createdAt time.Time) Event {
	return Event{
		ID:              eventID,
		SettlementKey:   key,
		RequestRecordID: cloneUUIDPtr(handle.RecordIDPointer()),
		RequestID:       handle.RequestID,
		UserID:          uuidPtrOrNil(handle.UserID),
		APIKeyID:        uuidPtrOrNil(handle.APIKeyID),
		RequestedModel:  handle.RequestedModel,
		StartedAt:       handle.StartedAt,
		FinishedAt:      input.FinishedAt,
		ResolvedModelID: cloneUUIDPtr(input.ResolvedModelID),
		ProviderID:      cloneUUIDPtr(input.ProviderID),
		ProviderModelID: cloneUUIDPtr(input.ProviderModelID),
		RouteVersionID:  cloneUUIDPtr(input.RouteVersionID),
		CredentialID:    cloneUUIDPtr(input.CredentialID),
		PriceVersionID:  cloneUUIDPtr(input.PriceVersionID),
		WalletID:        cloneUUIDPtr(input.WalletID),
		Usage:           input.Usage,
		AmountMinor:     cloneInt64Ptr(input.AmountMinor),
		Currency:        input.Currency,
		Estimated:       input.Estimated,
		Provenance:      input.Provenance,
		TerminalStatus:  input.TerminalStatus,
		UpstreamStatus:  cloneIntPtr(input.UpstreamStatus),
		ErrorClass:      input.ErrorClass,
		Latency:         input.Latency,
		LatencyMS:       durationMillisPtr(input.Latency),
		TTFT:            durationValue(input.TTFT),
		TTFTMS:          durationPtrMillis(input.TTFT),
		CreatedAt:       createdAt.UTC(),
	}
}

func closeRequestRecord(ctx context.Context, q data.Querier, recordID uuid.UUID, status TerminalStatus, finishedAt time.Time) error {
	_, err := q.Exec(ctx, `
		UPDATE request_records
		SET finished_at = COALESCE(finished_at, $2), terminal_status = COALESCE(terminal_status, $3)
		WHERE id = $1`, recordID, finishedAt, status)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func upsertAttempt(ctx context.Context, q data.Querier, handle RequestHandle, input FinalizeInput) error {
	attempt := input.Attempt
	if attempt == nil {
		return nil
	}
	startedAt := attempt.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = handle.StartedAt
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	attemptID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate request attempt UUIDv7: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO request_attempts (
			id, request_record_id, attempt_no, route_version_id, provider_id,
			provider_model_id, credential_id, started_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (request_record_id, attempt_no) DO NOTHING`,
		attemptID,
		handle.RecordID,
		attempt.AttemptNo,
		uuidPtrArg(attempt.RouteVersionID),
		uuidPtrArg(attempt.ProviderID),
		uuidPtrArg(attempt.ProviderModelID),
		uuidPtrArg(attempt.CredentialID),
		startedAt)
	if err != nil {
		return mapRepositoryError(err)
	}
	_, err = q.Exec(ctx, `
		UPDATE request_attempts
		SET route_version_id = COALESCE(route_version_id, $3),
			provider_id = COALESCE(provider_id, $4),
			provider_model_id = COALESCE(provider_model_id, $5),
			credential_id = COALESCE(credential_id, $6),
			upstream_status = $7, error_class = $8,
			finished_at = $9, latency_ms = $10, ttft_ms = $11
		WHERE request_record_id = $1 AND attempt_no = $2`,
		handle.RecordID,
		attempt.AttemptNo,
		uuidPtrArg(attempt.RouteVersionID),
		uuidPtrArg(attempt.ProviderID),
		uuidPtrArg(attempt.ProviderModelID),
		uuidPtrArg(attempt.CredentialID),
		intPtrArg(input.UpstreamStatus),
		stringArg(input.ErrorClass),
		input.FinishedAt,
		intPtr64Arg(durationMillisPtr(input.Latency)),
		intPtr64Arg(durationPtrMillis(input.TTFT)),
	)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func insertUsageOutbox(ctx context.Context, q data.Querier, event Event) error {
	payload, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Event         Event `json:"event"`
	}{SchemaVersion: 1, Event: event})
	if err != nil {
		return fmt.Errorf("encode usage outbox payload: %w", err)
	}
	outboxID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate usage outbox UUIDv7: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_events (
			id, aggregate_type, aggregate_id, event_type, idempotency_key, payload
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (aggregate_type, aggregate_id, event_type, idempotency_key) DO NOTHING`,
		outboxID,
		outboxAggregateUsage,
		event.ID,
		outboxEventUsageRecorded,
		"usage:"+event.SettlementKey,
		payload)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func insertReconciliationOutbox(ctx context.Context, q data.Querier, reconciliation Reconciliation) error {
	payload, err := json.Marshal(struct {
		SchemaVersion  int            `json:"schema_version"`
		Reconciliation Reconciliation `json:"reconciliation"`
	}{SchemaVersion: 1, Reconciliation: reconciliation})
	if err != nil {
		return fmt.Errorf("encode reconciliation outbox payload: %w", err)
	}
	outboxID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate reconciliation outbox UUIDv7: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_events (
			id, aggregate_type, aggregate_id, event_type, idempotency_key, payload
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (aggregate_type, aggregate_id, event_type, idempotency_key) DO NOTHING`,
		outboxID,
		outboxAggregateReconciliation,
		reconciliation.ID,
		outboxEventUsageReconciled,
		"reconciliation:"+reconciliation.Source+":"+reconciliation.SourceEventKey,
		payload)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func outboxOwnershipError(ctx context.Context, q data.Querier, eventID uuid.UUID, workerID string) error {
	var status string
	var claimedBy pgtype.Text
	err := q.QueryRow(ctx, `SELECT status, claimed_by FROM outbox_events WHERE id = $1`, eventID).Scan(&status, &claimedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return mapRepositoryError(err)
	}
	if status != "processing" || !claimedBy.Valid || claimedBy.String != workerID {
		return ErrOutboxNotOwned
	}
	return ErrOutboxInvalidState
}

func durableContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

func settlementKey(requestID string) string {
	return "usage:v1:" + requestID
}

func uuidArg(value uuid.UUID) any {
	if value == uuid.Nil {
		return nil
	}
	return value
}

func uuidPtrArg(value *uuid.UUID) any {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	return *value
}

func uuidPtrOrNil(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	copy := value
	return &copy
}

func cloneUUIDPtr(value *uuid.UUID) *uuid.UUID {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func nullableUUID(value pgtype.UUID) *uuid.UUID {
	if !value.Valid {
		return nil
	}
	result := uuid.UUID(value.Bytes)
	return &result
}

func nullableInt64(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func intPtr64Arg(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func intPtrArg(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func stringArg(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func durationMillis(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

func durationMillisPtr(value time.Duration) *int64 {
	result := durationMillis(value)
	return &result
}

func durationPtrMillis(value *time.Duration) *int64 {
	if value == nil {
		return nil
	}
	result := durationMillis(*value)
	return &result
}

func durationValue(value *time.Duration) time.Duration {
	if value == nil {
		return 0
	}
	return *value
}

func mapRepositoryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23505", "55000":
			return fmt.Errorf("%w: %v", ErrConflict, err)
		case "23503", "23514", "22P02":
			return fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
	}
	return err
}
