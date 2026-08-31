package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
	"github.com/starhui-dev/bablo/internal/usage"
)

const billingDurableTimeout = 30 * time.Second

// Repository owns every wallet row lock and financial transaction boundary.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a PostgreSQL-backed billing repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, fmt.Errorf("billing repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

type rowScanner interface {
	Scan(...any) error
}

const walletColumns = `
	id, user_id, currency, available_balance_minor, reserved_balance_minor,
	status, version, created_at, updated_at`

func scanWallet(row rowScanner) (Wallet, error) {
	var value Wallet
	if err := row.Scan(
		&value.ID,
		&value.UserID,
		&value.Currency,
		&value.AvailableBalanceMinor,
		&value.ReservedBalanceMinor,
		&value.Status,
		&value.Version,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return Wallet{}, err
	}
	value.Currency = strings.TrimSpace(value.Currency)
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	return value, nil
}

const reservationColumns = `
	id, wallet_id, user_id, api_key_id, request_id, request_record_id,
	model_id, provider_model_id, route_version_id, provider_id, credential_id,
	price_version_id, estimated_input_tokens, estimated_output_tokens,
	estimated_cache_read_tokens, estimated_cache_write_tokens, estimated_reasoning_tokens,
	reservation_key, amount_minor, currency, status, settled_amount_minor,
	usage_event_id, reason, expires_at, created_at, updated_at`

func scanReservation(row rowScanner) (Reservation, error) {
	var value Reservation
	var apiKeyID, requestRecordID, modelID, providerModelID, routeVersionID, providerID, credentialID, usageEventID pgtype.UUID
	var settledAmount pgtype.Int8
	var reason pgtype.Text
	var expiresAt pgtype.Timestamptz
	if err := row.Scan(
		&value.ID,
		&value.WalletID,
		&value.UserID,
		&apiKeyID,
		&value.RequestID,
		&requestRecordID,
		&modelID,
		&providerModelID,
		&routeVersionID,
		&providerID,
		&credentialID,
		&value.PriceVersionID,
		&value.EstimatedUsage.InputTokens,
		&value.EstimatedUsage.OutputTokens,
		&value.EstimatedUsage.CacheReadTokens,
		&value.EstimatedUsage.CacheWriteTokens,
		&value.EstimatedUsage.ReasoningTokens,
		&value.ReservationKey,
		&value.AmountMinor,
		&value.Currency,
		&value.Status,
		&settledAmount,
		&usageEventID,
		&reason,
		&expiresAt,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return Reservation{}, err
	}
	value.APIKeyID = nullableUUID(apiKeyID)
	value.RequestRecordID = nullableUUID(requestRecordID)
	value.ModelID = nullableUUID(modelID)
	value.ProviderModelID = nullableUUID(providerModelID)
	value.RouteVersionID = nullableUUID(routeVersionID)
	value.ProviderID = nullableUUID(providerID)
	value.CredentialID = nullableUUID(credentialID)
	value.UsageEventID = nullableUUID(usageEventID)
	value.SettledAmountMinor = nullableInt64(settledAmount)
	if reason.Valid {
		value.Reason = reason.String
	}
	if expiresAt.Valid {
		parsed := expiresAt.Time.UTC()
		value.ExpiresAt = &parsed
	}
	value.Currency = strings.TrimSpace(value.Currency)
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	return value, nil
}

const settlementColumns = `
	id, reservation_id, usage_event_id, idempotency_key,
	reserved_amount_minor, actual_amount_minor, status, estimated,
	error_class, created_at, updated_at`

func scanSettlement(row rowScanner) (Settlement, error) {
	var value Settlement
	var actualAmount pgtype.Int8
	var errorClass pgtype.Text
	if err := row.Scan(
		&value.ID,
		&value.ReservationID,
		&value.UsageEventID,
		&value.IdempotencyKey,
		&value.ReservedAmountMinor,
		&actualAmount,
		&value.Status,
		&value.Estimated,
		&errorClass,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return Settlement{}, err
	}
	value.ActualAmountMinor = nullableInt64(actualAmount)
	if errorClass.Valid {
		value.ErrorClass = errorClass.String
	}
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	return value, nil
}

const ledgerColumns = `
	id, wallet_id, entry_type, amount_minor, available_delta_minor,
	reserved_delta_minor, available_balance_after_minor,
	reserved_balance_after_minor, currency, reference_type, reference_id,
	idempotency_key, usage_event_id, operator_user_id, source, created_at`

func scanLedger(row rowScanner) (LedgerEntry, error) {
	var value LedgerEntry
	var usageEventID, operatorUserID pgtype.UUID
	if err := row.Scan(
		&value.ID,
		&value.WalletID,
		&value.EntryType,
		&value.AmountMinor,
		&value.AvailableDeltaMinor,
		&value.ReservedDeltaMinor,
		&value.AvailableBalanceAfterMinor,
		&value.ReservedBalanceAfterMinor,
		&value.Currency,
		&value.ReferenceType,
		&value.ReferenceID,
		&value.IdempotencyKey,
		&usageEventID,
		&operatorUserID,
		&value.Source,
		&value.CreatedAt,
	); err != nil {
		return LedgerEntry{}, err
	}
	value.UsageEventID = nullableUUID(usageEventID)
	value.OperatorUserID = nullableUUID(operatorUserID)
	value.Currency = strings.TrimSpace(value.Currency)
	value.CreatedAt = value.CreatedAt.UTC()
	return value, nil
}

// Reserve serializes one API-key budget and one wallet before creating a hold.
func (r *Repository) Reserve(ctx context.Context, input ReserveInput, quote Quote, now time.Time) (Reservation, error) {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	reservationID, err := id.New()
	if err != nil {
		return Reservation{}, fmt.Errorf("generate reservation UUIDv7: %w", err)
	}
	var result Reservation
	err = r.store.WithTx(dbCtx, func(q data.Querier) error {
		if err := lockBudget(dbCtx, q, input.APIKeyID); err != nil {
			return err
		}
		existing, err := loadReservationByRequestID(dbCtx, q, input.RequestID, true)
		switch {
		case err == nil:
			if !sameReservationInput(existing, input, quote) {
				return ErrReservationConflict
			}
			result = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}
		if err := validatePublishedPrice(dbCtx, q, quote.PriceVersionID, quote.Currency, now); err != nil {
			return err
		}

		wallet, err := getOrCreateWallet(dbCtx, q, input.UserID, quote.Currency, now)
		if err != nil {
			return err
		}
		if wallet.Status != "active" {
			return ErrWalletFrozen
		}
		if wallet.Currency != quote.Currency {
			return ErrCurrencyMismatch
		}
		if err := checkBudgets(dbCtx, q, input, quote.AmountMinor, now); err != nil {
			return err
		}
		if wallet.AvailableBalanceMinor < quote.AmountMinor {
			return ErrInsufficientFunds
		}
		reservedAfter, ok := safeAdd(wallet.ReservedBalanceMinor, quote.AmountMinor)
		if !ok {
			return ErrBalanceOverflow
		}
		availableAfter := wallet.AvailableBalanceMinor - quote.AmountMinor
		if _, err := q.Exec(dbCtx, `
			UPDATE wallets
			SET available_balance_minor = $2, reserved_balance_minor = $3,
				version = version + 1, updated_at = $4
			WHERE id = $1`,
			wallet.ID, availableAfter, reservedAfter, now); err != nil {
			return mapRepositoryError(err)
		}

		reservation := Reservation{
			ID:              reservationID,
			WalletID:        wallet.ID,
			UserID:          input.UserID,
			APIKeyID:        uuidPointer(input.APIKeyID),
			RequestID:       input.RequestID,
			RequestRecordID: cloneUUID(input.RequestRecordID),
			ModelID:         cloneUUID(input.ModelID),
			ProviderModelID: cloneUUID(input.ProviderModelID),
			RouteVersionID:  cloneUUID(input.RouteVersionID),
			ProviderID:      cloneUUID(input.ProviderID),
			CredentialID:    cloneUUID(input.CredentialID),
			PriceVersionID:  quote.PriceVersionID,
			EstimatedUsage:  input.EstimatedUsage,
			ReservationKey:  reservationKey(input.RequestID),
			AmountMinor:     quote.AmountMinor,
			Currency:        quote.Currency,
			Status:          ReservationReserved,
			Reason:          input.Reason,
			ExpiresAt:       cloneTime(input.ExpiresAt),
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if _, err := q.Exec(dbCtx, `
			INSERT INTO wallet_reservations (
				id, wallet_id, user_id, api_key_id, request_id, request_record_id,
				model_id, provider_model_id, route_version_id, provider_id, credential_id,
				price_version_id, estimated_input_tokens, estimated_output_tokens,
				estimated_cache_read_tokens, estimated_cache_write_tokens, estimated_reasoning_tokens,
				reservation_key, amount_minor, currency, status, reason, expires_at,
				created_at, updated_at
			)
			VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
				$14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $24
			)`,
			reservation.ID,
			reservation.WalletID,
			reservation.UserID,
			uuidPtrArg(reservation.APIKeyID),
			reservation.RequestID,
			uuidPtrArg(reservation.RequestRecordID),
			uuidPtrArg(reservation.ModelID),
			uuidPtrArg(reservation.ProviderModelID),
			uuidPtrArg(reservation.RouteVersionID),
			uuidPtrArg(reservation.ProviderID),
			uuidPtrArg(reservation.CredentialID),
			reservation.PriceVersionID,
			reservation.EstimatedUsage.InputTokens,
			reservation.EstimatedUsage.OutputTokens,
			reservation.EstimatedUsage.CacheReadTokens,
			reservation.EstimatedUsage.CacheWriteTokens,
			reservation.EstimatedUsage.ReasoningTokens,
			reservation.ReservationKey,
			reservation.AmountMinor,
			reservation.Currency,
			reservation.Status,
			reservation.Reason,
			timePtrArg(reservation.ExpiresAt),
			now,
		); err != nil {
			return mapRepositoryError(err)
		}
		entry, err := insertLedger(dbCtx, q, ledgerInsert{
			WalletID:                   wallet.ID,
			EntryType:                  EntryReservation,
			AmountMinor:                quote.AmountMinor,
			AvailableDeltaMinor:        -quote.AmountMinor,
			ReservedDeltaMinor:         quote.AmountMinor,
			AvailableBalanceAfterMinor: availableAfter,
			ReservedBalanceAfterMinor:  reservedAfter,
			Currency:                   quote.Currency,
			ReferenceType:              "request",
			ReferenceID:                input.RequestID,
			IdempotencyKey:             "reservation:v1:" + input.RequestID,
			Source:                     "billing",
			CreatedAt:                  now,
		})
		if err != nil {
			return err
		}
		if err := insertBillingOutbox(dbCtx, q, "wallet_reservation", reservation.ID, "billing.reservation.created", reservation.ReservationKey, map[string]any{
			"reservation_id":   reservation.ID,
			"wallet_id":        reservation.WalletID,
			"request_id":       reservation.RequestID,
			"amount_minor":     reservation.AmountMinor,
			"currency":         reservation.Currency,
			"ledger_id":        entry.ID,
			"price_version_id": reservation.PriceVersionID,
		}); err != nil {
			return err
		}
		result = reservation
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return result, nil
}

// Settle atomically consumes a hold, writes immutable charge/release entries,
// and records a retryable settlement state when the final charge exceeds funds.
func (r *Repository) Settle(ctx context.Context, input SettleInput, now time.Time) (Settlement, error) {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	actual := *input.Event.AmountMinor
	var result Settlement
	pending := false
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		reservation, err := loadReservationByID(dbCtx, q, input.ReservationID, true)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrReservationNotFound
			}
			return mapRepositoryError(err)
		}
		if reservation.RequestID != input.Event.RequestID || reservation.Currency != input.Event.Currency || input.Event.PriceVersionID == nil || *input.Event.PriceVersionID != reservation.PriceVersionID {
			return ErrSettlementConflict
		}
		if input.Event.WalletID == nil || *input.Event.WalletID != reservation.WalletID {
			return ErrSettlementConflict
		}
		if input.Event.UserID != nil && *input.Event.UserID != reservation.UserID {
			return ErrSettlementConflict
		}
		if reservation.APIKeyID != nil && input.Event.APIKeyID != nil && *reservation.APIKeyID != *input.Event.APIKeyID {
			return ErrSettlementConflict
		}

		existing, err := loadSettlementByReservation(dbCtx, q, reservation.ID, true)
		switch {
		case err == nil:
			if existing.UsageEventID != input.Event.ID || existing.ReservedAmountMinor != reservation.AmountMinor || existing.ActualAmountMinor == nil || *existing.ActualAmountMinor != actual || existing.Estimated != input.Event.Estimated {
				return ErrSettlementConflict
			}
			if existing.Status == SettlementSettled {
				result = existing
				return nil
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}
		if reservation.Status == ReservationSettled {
			return ErrSettlementConflict
		}
		if reservation.Status == ReservationReleased {
			return ErrReservationConflict
		}

		wallet, err := loadWalletByID(dbCtx, q, reservation.WalletID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if wallet.Currency != reservation.Currency {
			return ErrCurrencyMismatch
		}
		if wallet.ReservedBalanceMinor < reservation.AmountMinor {
			return ErrSettlementConflict
		}

		extra := int64(0)
		if actual > reservation.AmountMinor {
			extra = actual - reservation.AmountMinor
		}
		if wallet.AvailableBalanceMinor < extra {
			stored, err := upsertSettlement(dbCtx, q, existing, reservation, input.Event, actual, SettlementPending, "insufficient_funds", now)
			if err != nil {
				return err
			}
			if _, err := q.Exec(dbCtx, `
				UPDATE wallet_reservations
				SET status = 'settlement_pending', settled_amount_minor = $2,
					usage_event_id = $3, reason = 'insufficient_funds', updated_at = $4
				WHERE id = $1`,
				reservation.ID, actual, input.Event.ID, now); err != nil {
				return mapRepositoryError(err)
			}
			if err := insertBillingOutbox(dbCtx, q, "billing_settlement", stored.ID, "billing.settlement.pending", stored.IdempotencyKey, map[string]any{
				"settlement_id":  stored.ID,
				"reservation_id": reservation.ID,
				"usage_event_id": input.Event.ID,
				"amount_minor":   actual,
				"currency":       reservation.Currency,
				"error_class":    "insufficient_funds",
			}); err != nil {
				return err
			}
			result = stored
			pending = true
			return nil
		}

		consumedReserved := actual
		if consumedReserved > reservation.AmountMinor {
			consumedReserved = reservation.AmountMinor
		}
		released := reservation.AmountMinor - consumedReserved
		chargeAvailableAfter := wallet.AvailableBalanceMinor - extra
		chargeReservedAfter := wallet.ReservedBalanceMinor - consumedReserved
		finalAvailable := chargeAvailableAfter
		finalReserved := chargeReservedAfter
		if released > 0 {
			var ok bool
			finalAvailable, ok = safeAdd(finalAvailable, released)
			if !ok {
				return ErrBalanceOverflow
			}
			finalReserved -= released
		}
		if finalAvailable < 0 || finalReserved < 0 {
			return ErrSettlementConflict
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE wallets
			SET available_balance_minor = $2, reserved_balance_minor = $3,
				version = version + 1, updated_at = $4
			WHERE id = $1`,
			wallet.ID, finalAvailable, finalReserved, now); err != nil {
			return mapRepositoryError(err)
		}

		if actual > 0 {
			if _, err := insertLedger(dbCtx, q, ledgerInsert{
				WalletID:                   wallet.ID,
				EntryType:                  EntryUsageCharge,
				AmountMinor:                -actual,
				AvailableDeltaMinor:        -extra,
				ReservedDeltaMinor:         -consumedReserved,
				AvailableBalanceAfterMinor: chargeAvailableAfter,
				ReservedBalanceAfterMinor:  chargeReservedAfter,
				Currency:                   wallet.Currency,
				ReferenceType:              "usage_event",
				ReferenceID:                input.Event.ID.String(),
				IdempotencyKey:             "usage-charge:v1:" + input.Event.ID.String(),
				UsageEventID:               uuidPointer(input.Event.ID),
				Source:                     "billing",
				CreatedAt:                  now,
			}); err != nil {
				return err
			}
		}
		if released > 0 {
			if _, err := insertLedger(dbCtx, q, ledgerInsert{
				WalletID:                   wallet.ID,
				EntryType:                  EntryRelease,
				AmountMinor:                -released,
				AvailableDeltaMinor:        released,
				ReservedDeltaMinor:         -released,
				AvailableBalanceAfterMinor: finalAvailable,
				ReservedBalanceAfterMinor:  finalReserved,
				Currency:                   wallet.Currency,
				ReferenceType:              "reservation",
				ReferenceID:                reservation.ID.String(),
				IdempotencyKey:             "settlement-release:v1:" + input.Event.ID.String(),
				Source:                     "billing",
				CreatedAt:                  now,
			}); err != nil {
				return err
			}
		}
		if actual == 0 && released == 0 {
			return ErrSettlementConflict
		}

		stored, err := upsertSettlement(dbCtx, q, existing, reservation, input.Event, actual, SettlementSettled, "", now)
		if err != nil {
			return err
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE wallet_reservations
			SET status = 'settled', settled_amount_minor = $2,
				usage_event_id = $3, reason = NULL, updated_at = $4
			WHERE id = $1`,
			reservation.ID, actual, input.Event.ID, now); err != nil {
			return mapRepositoryError(err)
		}
		if err := insertBillingOutbox(dbCtx, q, "billing_settlement", stored.ID, "billing.settlement.settled", stored.IdempotencyKey, map[string]any{
			"settlement_id":  stored.ID,
			"reservation_id": reservation.ID,
			"usage_event_id": input.Event.ID,
			"amount_minor":   actual,
			"currency":       wallet.Currency,
			"estimated":      input.Event.Estimated,
		}); err != nil {
			return err
		}
		result = stored
		return nil
	})
	if err != nil {
		return Settlement{}, err
	}
	if pending {
		return result, ErrSettlementPending
	}
	return result, nil
}

// Release returns the entire hold and appends a release ledger fact.
func (r *Repository) Release(ctx context.Context, input ReleaseInput, now time.Time) error {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	return r.store.WithTx(dbCtx, func(q data.Querier) error {
		reservation, err := loadReservationByID(dbCtx, q, input.ReservationID, true)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrReservationNotFound
			}
			return mapRepositoryError(err)
		}
		if reservation.Status == ReservationReleased {
			return nil
		}
		if reservation.Status != ReservationReserved {
			return ErrReservationConflict
		}
		wallet, err := loadWalletByID(dbCtx, q, reservation.WalletID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if wallet.ReservedBalanceMinor < reservation.AmountMinor {
			return ErrReservationConflict
		}
		availableAfter, ok := safeAdd(wallet.AvailableBalanceMinor, reservation.AmountMinor)
		if !ok {
			return ErrBalanceOverflow
		}
		reservedAfter := wallet.ReservedBalanceMinor - reservation.AmountMinor
		if _, err := q.Exec(dbCtx, `
			UPDATE wallets
			SET available_balance_minor = $2, reserved_balance_minor = $3,
				version = version + 1, updated_at = $4
			WHERE id = $1`,
			wallet.ID, availableAfter, reservedAfter, now); err != nil {
			return mapRepositoryError(err)
		}
		entry, err := insertLedger(dbCtx, q, ledgerInsert{
			WalletID:                   wallet.ID,
			EntryType:                  EntryRelease,
			AmountMinor:                -reservation.AmountMinor,
			AvailableDeltaMinor:        reservation.AmountMinor,
			ReservedDeltaMinor:         -reservation.AmountMinor,
			AvailableBalanceAfterMinor: availableAfter,
			ReservedBalanceAfterMinor:  reservedAfter,
			Currency:                   wallet.Currency,
			ReferenceType:              "reservation",
			ReferenceID:                reservation.ID.String(),
			IdempotencyKey:             "release:v1:" + reservation.ID.String(),
			Source:                     "billing",
			CreatedAt:                  now,
		})
		if err != nil {
			return err
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE wallet_reservations
			SET status = 'released', reason = $2, updated_at = $3
			WHERE id = $1`, reservation.ID, input.Reason, now); err != nil {
			return mapRepositoryError(err)
		}
		return insertBillingOutbox(dbCtx, q, "wallet_reservation", reservation.ID, "billing.reservation.released", "release:v1:"+reservation.ID.String(), map[string]any{
			"reservation_id": reservation.ID,
			"wallet_id":      wallet.ID,
			"request_id":     reservation.RequestID,
			"amount_minor":   reservation.AmountMinor,
			"currency":       wallet.Currency,
			"ledger_id":      entry.ID,
			"reason":         input.Reason,
		})
	})
}

// Credit serializes one wallet and appends a signed funding/adjustment entry.
func (r *Repository) Credit(ctx context.Context, input CreditInput, now time.Time) (LedgerEntry, error) {
	dbCtx, cancel := durableContext(ctx)
	defer cancel()
	var result LedgerEntry
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		wallet, err := getOrCreateWallet(dbCtx, q, input.UserID, input.Currency, now)
		if err != nil {
			return err
		}
		existing, err := loadLedgerByIdempotency(dbCtx, q, wallet.ID, input.IdempotencyKey)
		switch {
		case err == nil:
			if existing.EntryType != input.EntryType || existing.AmountMinor != input.AmountMinor || existing.ReferenceType != input.ReferenceType || existing.ReferenceID != input.ReferenceID || existing.Source != input.Source || !equalUUID(existing.OperatorUserID, input.OperatorUserID) {
				return ErrSettlementConflict
			}
			result = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}
		if wallet.Status == "closed" || (wallet.Status == "frozen" && input.AmountMinor < 0) {
			return ErrWalletFrozen
		}
		availableAfter, ok := safeAdd(wallet.AvailableBalanceMinor, input.AmountMinor)
		if !ok {
			return ErrBalanceOverflow
		}
		if availableAfter < 0 {
			return ErrInsufficientFunds
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE wallets
			SET available_balance_minor = $2, version = version + 1, updated_at = $3
			WHERE id = $1`, wallet.ID, availableAfter, now); err != nil {
			return mapRepositoryError(err)
		}
		entry, err := insertLedger(dbCtx, q, ledgerInsert{
			WalletID:                   wallet.ID,
			EntryType:                  input.EntryType,
			AmountMinor:                input.AmountMinor,
			AvailableDeltaMinor:        input.AmountMinor,
			ReservedDeltaMinor:         0,
			AvailableBalanceAfterMinor: availableAfter,
			ReservedBalanceAfterMinor:  wallet.ReservedBalanceMinor,
			Currency:                   wallet.Currency,
			ReferenceType:              input.ReferenceType,
			ReferenceID:                input.ReferenceID,
			IdempotencyKey:             input.IdempotencyKey,
			OperatorUserID:             cloneUUID(input.OperatorUserID),
			Source:                     input.Source,
			CreatedAt:                  now,
		})
		if err != nil {
			return err
		}
		if input.OperatorUserID != nil {
			if err := audit.Insert(dbCtx, q, audit.Event{
				ActorUserID: input.OperatorUserID,
				Action:      "wallet." + input.EntryType,
				TargetType:  "wallet",
				TargetID:    wallet.ID.String(),
				RequestID:   input.ReferenceID,
				Result:      "success",
			}); err != nil {
				return err
			}
		}
		if err := insertBillingOutbox(dbCtx, q, "wallet_ledger", entry.ID, "billing.wallet.credited", input.IdempotencyKey, map[string]any{
			"ledger_id":      entry.ID,
			"wallet_id":      wallet.ID,
			"entry_type":     entry.EntryType,
			"amount_minor":   entry.AmountMinor,
			"currency":       entry.Currency,
			"reference_type": entry.ReferenceType,
			"reference_id":   entry.ReferenceID,
		}); err != nil {
			return err
		}
		result = entry
		return nil
	})
	if err != nil {
		return LedgerEntry{}, err
	}
	return result, nil
}

// GetWallet returns the current transaction-maintained projection.
func (r *Repository) GetWallet(ctx context.Context, userID uuid.UUID, currency string) (Wallet, error) {
	value, err := scanWallet(r.store.Queryer().QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE user_id = $1 AND currency = $2`, userID, currency))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrWalletNotFound
	}
	if err != nil {
		return Wallet{}, mapRepositoryError(err)
	}
	return value, nil
}

// RebuildBalance sums immutable deltas; it intentionally ignores wallet cache columns.
func (r *Repository) RebuildBalance(ctx context.Context, walletID uuid.UUID) (int64, int64, error) {
	var availableText, reservedText string
	if err := r.store.Queryer().QueryRow(ctx, `
		SELECT COALESCE(SUM(available_delta_minor), 0)::text,
			COALESCE(SUM(reserved_delta_minor), 0)::text
		FROM wallet_ledger
		WHERE wallet_id = $1`, walletID).Scan(&availableText, &reservedText); err != nil {
		return 0, 0, mapRepositoryError(err)
	}
	available, err := parseInt64Total(availableText)
	if err != nil {
		return 0, 0, err
	}
	reserved, err := parseInt64Total(reservedText)
	if err != nil {
		return 0, 0, err
	}
	return available, reserved, nil
}

func loadWalletByID(ctx context.Context, q data.Querier, walletID uuid.UUID, forUpdate bool) (Wallet, error) {
	query := `SELECT ` + walletColumns + ` FROM wallets WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanWallet(q.QueryRow(ctx, query, walletID))
}

func getOrCreateWallet(ctx context.Context, q data.Querier, userID uuid.UUID, currency string, now time.Time) (Wallet, error) {
	walletID, err := id.New()
	if err != nil {
		return Wallet{}, fmt.Errorf("generate wallet UUIDv7: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO wallets (id, user_id, currency, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (user_id, currency) DO NOTHING`, walletID, userID, currency, now); err != nil {
		return Wallet{}, mapRepositoryError(err)
	}
	wallet, err := scanWallet(q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE user_id = $1 AND currency = $2 FOR UPDATE`, userID, currency))
	if err != nil {
		return Wallet{}, mapRepositoryError(err)
	}
	return wallet, nil
}

func loadReservationByID(ctx context.Context, q data.Querier, reservationID uuid.UUID, forUpdate bool) (Reservation, error) {
	query := `SELECT ` + reservationColumns + ` FROM wallet_reservations WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanReservation(q.QueryRow(ctx, query, reservationID))
}

func loadReservationByRequestID(ctx context.Context, q data.Querier, requestID string, forUpdate bool) (Reservation, error) {
	query := `SELECT ` + reservationColumns + ` FROM wallet_reservations WHERE request_id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanReservation(q.QueryRow(ctx, query, requestID))
}

func loadSettlementByReservation(ctx context.Context, q data.Querier, reservationID uuid.UUID, forUpdate bool) (Settlement, error) {
	query := `SELECT ` + settlementColumns + ` FROM billing_settlements WHERE reservation_id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanSettlement(q.QueryRow(ctx, query, reservationID))
}

func loadLedgerByIdempotency(ctx context.Context, q data.Querier, walletID uuid.UUID, key string) (LedgerEntry, error) {
	return scanLedger(q.QueryRow(ctx, `SELECT `+ledgerColumns+` FROM wallet_ledger WHERE wallet_id = $1 AND idempotency_key = $2`, walletID, key))
}

func lockBudget(ctx context.Context, q data.Querier, apiKeyID uuid.UUID) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing-budget:' || $1::text, 0))`, apiKeyID)
	if err != nil {
		return fmt.Errorf("lock API key budget: %w", err)
	}
	return nil
}

func validatePublishedPrice(ctx context.Context, q data.Querier, priceVersionID uuid.UUID, currency string, at time.Time) error {
	if priceVersionID == uuid.Nil {
		return ErrPriceMissing
	}
	var published bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM price_versions
			WHERE id = $1
			  AND btrim(currency) = $2
			  AND status IN ('active', 'retired')
			  AND effective_from <= $3
			  AND (effective_to IS NULL OR effective_to > $3)
		)`, priceVersionID, currency, at).Scan(&published); err != nil {
		return fmt.Errorf("validate published price version: %w", err)
	}
	if !published {
		return ErrPriceMissing
	}
	return nil
}

func checkBudgets(ctx context.Context, q data.Querier, input ReserveInput, amount int64, now time.Time) error {
	if input.DailyBudgetMinor == nil && input.MonthlyBudgetMinor == nil {
		return nil
	}
	var heldText string
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor), 0)::text
		FROM wallet_reservations
		WHERE api_key_id = $1 AND status IN ('reserved', 'settlement_pending')`, input.APIKeyID).Scan(&heldText); err != nil {
		return fmt.Errorf("sum API key reservations: %w", err)
	}
	held, err := parseBigTotal(heldText)
	if err != nil {
		return err
	}
	checks := []struct {
		limit *int64
		start time.Time
	}{
		{input.DailyBudgetMinor, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)},
		{input.MonthlyBudgetMinor, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, check := range checks {
		if check.limit == nil {
			continue
		}
		var spentText string
		if err := q.QueryRow(ctx, `
			SELECT COALESCE(SUM(-ledger.amount_minor), 0)::text
			FROM wallet_ledger AS ledger
			JOIN usage_events AS usage ON usage.id = ledger.usage_event_id
			WHERE usage.api_key_id = $1
			  AND ledger.entry_type = 'usage_charge'
			  AND ledger.amount_minor < 0
			  AND ledger.created_at >= $2`, input.APIKeyID, check.start).Scan(&spentText); err != nil {
			return fmt.Errorf("sum API key charges: %w", err)
		}
		spent, err := parseBigTotal(spentText)
		if err != nil {
			return err
		}
		total := new(big.Int).Add(spent, held)
		total.Add(total, big.NewInt(amount))
		if total.Cmp(big.NewInt(*check.limit)) > 0 {
			return ErrBudgetExceeded
		}
	}
	return nil
}

type ledgerInsert struct {
	WalletID                   uuid.UUID
	EntryType                  string
	AmountMinor                int64
	AvailableDeltaMinor        int64
	ReservedDeltaMinor         int64
	AvailableBalanceAfterMinor int64
	ReservedBalanceAfterMinor  int64
	Currency                   string
	ReferenceType              string
	ReferenceID                string
	IdempotencyKey             string
	UsageEventID               *uuid.UUID
	OperatorUserID             *uuid.UUID
	Source                     string
	CreatedAt                  time.Time
}

func insertLedger(ctx context.Context, q data.Querier, input ledgerInsert) (LedgerEntry, error) {
	entryID, err := id.New()
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("generate ledger UUIDv7: %w", err)
	}
	createdAt := input.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	value, err := scanLedger(q.QueryRow(ctx, `
		INSERT INTO wallet_ledger (
			id, wallet_id, entry_type, amount_minor, available_delta_minor,
			reserved_delta_minor, available_balance_after_minor,
			reserved_balance_after_minor, currency, reference_type, reference_id,
			idempotency_key, usage_event_id, operator_user_id, source, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING `+ledgerColumns,
		entryID,
		input.WalletID,
		input.EntryType,
		input.AmountMinor,
		input.AvailableDeltaMinor,
		input.ReservedDeltaMinor,
		input.AvailableBalanceAfterMinor,
		input.ReservedBalanceAfterMinor,
		input.Currency,
		input.ReferenceType,
		input.ReferenceID,
		input.IdempotencyKey,
		uuidPtrArg(input.UsageEventID),
		uuidPtrArg(input.OperatorUserID),
		input.Source,
		createdAt,
	))
	if err != nil {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	return value, nil
}

func upsertSettlement(ctx context.Context, q data.Querier, existing Settlement, reservation Reservation, event usage.Event, actual int64, status, errorClass string, now time.Time) (Settlement, error) {
	return persistSettlement(ctx, q, existing, reservation, event.ID, actual, event.Estimated, status, errorClass, now)
}

func persistSettlement(ctx context.Context, q data.Querier, existing Settlement, reservation Reservation, usageEventID uuid.UUID, actual int64, estimated bool, status, errorClass string, now time.Time) (Settlement, error) {
	if existing.ID == uuid.Nil {
		settlementID, err := id.New()
		if err != nil {
			return Settlement{}, fmt.Errorf("generate settlement UUIDv7: %w", err)
		}
		return scanSettlement(q.QueryRow(ctx, `
			INSERT INTO billing_settlements (
				id, reservation_id, usage_event_id, idempotency_key,
				reserved_amount_minor, actual_amount_minor, status, estimated,
				error_class, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
			RETURNING `+settlementColumns,
			settlementID,
			reservation.ID,
			usageEventID,
			settlementKey(usageEventID),
			reservation.AmountMinor,
			actual,
			status,
			estimated,
			stringArg(errorClass),
			now,
		))
	}
	return scanSettlement(q.QueryRow(ctx, `
		UPDATE billing_settlements
		SET status = $2, error_class = $3, updated_at = $4
		WHERE id = $1
		RETURNING `+settlementColumns,
		existing.ID, status, stringArg(errorClass), now,
	))
}

func insertBillingOutbox(ctx context.Context, q data.Querier, aggregateType string, aggregateID uuid.UUID, eventType, key string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode billing outbox payload: %w", err)
	}
	outboxID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate billing outbox UUIDv7: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_events (
			id, aggregate_type, aggregate_id, event_type, idempotency_key, payload
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (aggregate_type, aggregate_id, event_type, idempotency_key) DO NOTHING`,
		outboxID, aggregateType, aggregateID, eventType, key, encoded)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func sameReservationInput(existing Reservation, input ReserveInput, quote Quote) bool {
	return existing.UserID == input.UserID &&
		equalUUID(existing.APIKeyID, uuidPointer(input.APIKeyID)) &&
		existing.RequestID == input.RequestID &&
		equalUUID(existing.RequestRecordID, input.RequestRecordID) &&
		equalUUID(existing.ModelID, input.ModelID) &&
		equalUUID(existing.ProviderModelID, input.ProviderModelID) &&
		equalUUID(existing.RouteVersionID, input.RouteVersionID) &&
		equalUUID(existing.ProviderID, input.ProviderID) &&
		equalUUID(existing.CredentialID, input.CredentialID) &&
		existing.PriceVersionID == quote.PriceVersionID &&
		existing.EstimatedUsage == input.EstimatedUsage &&
		existing.ReservationKey == reservationKey(input.RequestID) &&
		existing.AmountMinor == quote.AmountMinor &&
		existing.Currency == quote.Currency
}

func safeAdd(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, false
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, false
	}
	return left + right, true
}

func parseBigTotal(value string) (*big.Int, error) {
	parsed, ok := new(big.Int).SetString(strings.TrimSpace(value), 10)
	if !ok || parsed.Sign() < 0 {
		return nil, ErrBalanceOverflow
	}
	return parsed, nil
}

func parseInt64Total(value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, ErrBalanceOverflow
	}
	return parsed, nil
}

func mapRepositoryError(err error) error {
	if err == nil {
		return nil
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23503", "23514", "22P02", "22003":
			return ErrInvalidInput
		case "23505":
			return ErrSettlementConflict
		case "55000":
			return ErrSettlementConflict
		}
	}
	return err
}

func durableContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), billingDurableTimeout)
}

func uuidPtrArg(value *uuid.UUID) any {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	return *value
}

func timePtrArg(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC()
}

func stringArg(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullableUUID(value pgtype.UUID) *uuid.UUID {
	if !value.Valid {
		return nil
	}
	parsed := uuid.UUID(value.Bytes)
	return &parsed
}

func nullableInt64(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	parsed := value.Int64
	return &parsed
}

func equalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	copy := value.UTC()
	return &copy
}
