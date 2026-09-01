package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/starhui-dev/bablo/internal/usage"
)

const settlementRecoveryBatchSize = 100

type dueSettlement struct {
	SettlementID  uuid.UUID
	ReservationID uuid.UUID
	UsageEventID  uuid.UUID
}

func (r *Repository) ClaimPendingSettlements(ctx context.Context, limit int, now time.Time) ([]dueSettlement, error) {
	if r == nil || limit < 1 || limit > 1000 || now.IsZero() {
		return nil, ErrInvalidInput
	}
	ownerToken, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate settlement recovery owner: %w", err)
	}
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	rows, err := r.store.Queryer().Query(dbCtx, `
		WITH candidates AS (
			SELECT id
			FROM billing_settlements
			WHERE status = 'pending'
			  AND next_attempt_at <= $1
			  AND (lease_expires_at IS NULL OR lease_expires_at <= $1)
			ORDER BY next_attempt_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE billing_settlements settlement
		SET owner_token = $3, lease_expires_at = $4,
			attempts = settlement.attempts + 1, updated_at = $1
		FROM candidates
		WHERE settlement.id = candidates.id
		RETURNING settlement.id, settlement.reservation_id, settlement.usage_event_id`,
		now.UTC(), limit, ownerToken, now.UTC().Add(settlementRecoveryLease))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	defer rows.Close()
	result := make([]dueSettlement, 0, limit)
	for rows.Next() {
		var value dueSettlement
		if err := rows.Scan(&value.SettlementID, &value.ReservationID, &value.UsageEventID); err != nil {
			return nil, mapRepositoryError(err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, mapRepositoryError(err)
	}
	return result, nil
}

func (r *Repository) LoadSettlementUsageEvent(ctx context.Context, settlementID uuid.UUID) (usage.Event, error) {
	if r == nil || settlementID == uuid.Nil {
		return usage.Event{}, ErrInvalidInput
	}
	var event usage.Event
	var userID, apiKeyID, priceVersionID, walletID pgtype.UUID
	var amountMinor pgtype.Int8
	err := r.store.Queryer().QueryRow(ctx, `
		SELECT event.id, event.request_id, event.user_id, event.api_key_id,
			event.price_version_id, event.wallet_id, event.amount_minor,
			btrim(event.currency), event.estimated
		FROM billing_settlements settlement
		JOIN usage_events event ON event.id = settlement.usage_event_id
		WHERE settlement.id = $1`, settlementID).Scan(
		&event.ID, &event.RequestID, &userID, &apiKeyID,
		&priceVersionID, &walletID, &amountMinor, &event.Currency, &event.Estimated,
	)
	if err != nil {
		return usage.Event{}, mapRepositoryError(err)
	}
	if !amountMinor.Valid || amountMinor.Int64 < 0 {
		return usage.Event{}, ErrSettlementConflict
	}
	event.UserID = nullableUUID(userID)
	event.APIKeyID = nullableUUID(apiKeyID)
	event.PriceVersionID = nullableUUID(priceVersionID)
	event.WalletID = nullableUUID(walletID)
	amount := amountMinor.Int64
	event.AmountMinor = &amount
	return event, nil
}

func (r *Repository) DeferPendingSettlement(ctx context.Context, settlementID uuid.UUID, now time.Time, errorClass string) error {
	if r == nil || settlementID == uuid.Nil || now.IsZero() {
		return ErrInvalidInput
	}
	errorClass = normalizeText(errorClass, 128)
	if errorClass == "" {
		errorClass = "recovery_failed"
	}
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	_, err := r.store.Queryer().Exec(dbCtx, `
		UPDATE billing_settlements
		SET owner_token = NULL, lease_expires_at = NULL,
			next_attempt_at = $2, error_class = $3, updated_at = $4
		WHERE id = $1 AND status = 'pending'`,
		settlementID, now.UTC().Add(settlementRetryDelay), errorClass, now.UTC())
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func (s *Service) RecoverPendingSettlements(ctx context.Context, limit int) (int, error) {
	if s == nil || s.repository == nil || limit < 1 || limit > 1000 {
		return 0, ErrInvalidInput
	}
	now := s.now()
	due, err := s.repository.ClaimPendingSettlements(ctx, limit, now)
	if err != nil {
		return 0, err
	}
	recovered := 0
	failures := make([]error, 0)
	for _, candidate := range due {
		event, loadErr := s.repository.LoadSettlementUsageEvent(ctx, candidate.SettlementID)
		if loadErr != nil {
			if deferErr := s.repository.DeferPendingSettlement(ctx, candidate.SettlementID, s.now(), "usage_event_unavailable"); deferErr != nil {
				loadErr = errors.Join(loadErr, deferErr)
			}
			failures = append(failures, fmt.Errorf("load pending settlement %s: %w", candidate.SettlementID, loadErr))
			continue
		}
		_, settleErr := s.Settle(ctx, SettleInput{ReservationID: candidate.ReservationID, Event: event})
		switch {
		case settleErr == nil:
			recovered++
		case errors.Is(settleErr, ErrSettlementPending):
			// Settle advanced next_attempt_at and released the recovery lease.
		default:
			if deferErr := s.repository.DeferPendingSettlement(ctx, candidate.SettlementID, s.now(), "settlement_recovery_failed"); deferErr != nil {
				settleErr = errors.Join(settleErr, deferErr)
			}
			failures = append(failures, fmt.Errorf("recover pending settlement %s: %w", candidate.SettlementID, settleErr))
		}
	}
	return recovered, errors.Join(failures...)
}

func (s *Service) RunSettlementRecoveryWorker(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if s == nil || interval <= 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	run := func() {
		count, err := s.RecoverPendingSettlements(ctx, settlementRecoveryBatchSize)
		if err != nil {
			logger.Error("billing_settlement_recovery_error", "error", err)
		}
		if count > 0 {
			logger.Info("billing_settlements_recovered", "count", count)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
