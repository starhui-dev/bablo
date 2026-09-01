package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/data"
)

// HoldPaymentRefundInTx moves a full payment amount from available to reserved
// before any external refund call. The caller owns the surrounding transaction.
func (s *Service) HoldPaymentRefundInTx(ctx context.Context, q data.Querier, input PaymentRefundInput) (LedgerEntry, error) {
	normalized, err := normalizePaymentRefundInput(input)
	if err != nil || s == nil || s.repository == nil || q == nil {
		return LedgerEntry{}, ErrInvalidInput
	}
	now := s.now()
	wallet, err := getOrCreateWallet(ctx, q, normalized.UserID, normalized.Currency, now)
	if err != nil {
		return LedgerEntry{}, err
	}
	key := paymentRefundLedgerKey("hold", normalized.PaymentOrderID)
	if existing, err := loadLedgerByIdempotency(ctx, q, wallet.ID, key); err == nil {
		if !samePaymentRefundEntry(existing, EntryPaymentRefundHold, normalized, normalized.AmountMinor) {
			return LedgerEntry{}, ErrSettlementConflict
		}
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	if wallet.Status != "active" {
		return LedgerEntry{}, ErrWalletFrozen
	}
	if wallet.AvailableBalanceMinor < normalized.AmountMinor {
		return LedgerEntry{}, ErrInsufficientFunds
	}
	reservedAfter, ok := safeAdd(wallet.ReservedBalanceMinor, normalized.AmountMinor)
	if !ok {
		return LedgerEntry{}, ErrBalanceOverflow
	}
	availableAfter := wallet.AvailableBalanceMinor - normalized.AmountMinor
	if _, err := q.Exec(ctx, `
		UPDATE wallets
		SET available_balance_minor = $2, reserved_balance_minor = $3,
			version = version + 1, updated_at = $4
		WHERE id = $1`, wallet.ID, availableAfter, reservedAfter, now); err != nil {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	entry, err := insertLedger(ctx, q, ledgerInsert{
		WalletID:                   wallet.ID,
		EntryType:                  EntryPaymentRefundHold,
		AmountMinor:                normalized.AmountMinor,
		AvailableDeltaMinor:        -normalized.AmountMinor,
		ReservedDeltaMinor:         normalized.AmountMinor,
		AvailableBalanceAfterMinor: availableAfter,
		ReservedBalanceAfterMinor:  reservedAfter,
		Currency:                   wallet.Currency,
		ReferenceType:              "payment_order",
		ReferenceID:                normalized.PaymentOrderID.String(),
		IdempotencyKey:             key,
		OperatorUserID:             uuidPointer(normalized.OperatorUserID),
		Source:                     normalized.Source,
		CreatedAt:                  now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}
	if err := auditPaymentRefundLedger(ctx, q, normalized, wallet.ID, EntryPaymentRefundHold); err != nil {
		return LedgerEntry{}, err
	}
	if err := insertBillingOutbox(ctx, q, "payment_order", normalized.PaymentOrderID, "billing.payment_refund.held", key, map[string]any{
		"payment_order_id": normalized.PaymentOrderID,
		"wallet_id":        wallet.ID,
		"ledger_id":        entry.ID,
		"amount_minor":     normalized.AmountMinor,
		"currency":         normalized.Currency,
	}); err != nil {
		return LedgerEntry{}, err
	}
	return entry, nil
}

// CompletePaymentRefundInTx consumes a previously held payment amount after the
// provider confirms the refund. No available balance is returned to the user.
func (s *Service) CompletePaymentRefundInTx(ctx context.Context, q data.Querier, input PaymentRefundInput) (LedgerEntry, error) {
	normalized, err := normalizePaymentRefundInput(input)
	if err != nil || s == nil || s.repository == nil || q == nil {
		return LedgerEntry{}, ErrInvalidInput
	}
	now := s.now()
	wallet, err := getOrCreateWallet(ctx, q, normalized.UserID, normalized.Currency, now)
	if err != nil {
		return LedgerEntry{}, err
	}
	key := paymentRefundLedgerKey("complete", normalized.PaymentOrderID)
	if existing, err := loadLedgerByIdempotency(ctx, q, wallet.ID, key); err == nil {
		if !samePaymentRefundEntry(existing, EntryPaymentReversal, normalized, -normalized.AmountMinor) {
			return LedgerEntry{}, ErrSettlementConflict
		}
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	if err := requirePaymentRefundHold(ctx, q, wallet.ID, normalized); err != nil {
		return LedgerEntry{}, err
	}
	if wallet.ReservedBalanceMinor < normalized.AmountMinor {
		return LedgerEntry{}, ErrReservationConflict
	}
	reservedAfter := wallet.ReservedBalanceMinor - normalized.AmountMinor
	if _, err := q.Exec(ctx, `
		UPDATE wallets
		SET reserved_balance_minor = $2, version = version + 1, updated_at = $3
		WHERE id = $1`, wallet.ID, reservedAfter, now); err != nil {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	entry, err := insertLedger(ctx, q, ledgerInsert{
		WalletID:                   wallet.ID,
		EntryType:                  EntryPaymentReversal,
		AmountMinor:                -normalized.AmountMinor,
		AvailableDeltaMinor:        0,
		ReservedDeltaMinor:         -normalized.AmountMinor,
		AvailableBalanceAfterMinor: wallet.AvailableBalanceMinor,
		ReservedBalanceAfterMinor:  reservedAfter,
		Currency:                   wallet.Currency,
		ReferenceType:              "payment_order",
		ReferenceID:                normalized.PaymentOrderID.String(),
		IdempotencyKey:             key,
		OperatorUserID:             uuidPointer(normalized.OperatorUserID),
		Source:                     normalized.Source,
		CreatedAt:                  now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}
	if err := auditPaymentRefundLedger(ctx, q, normalized, wallet.ID, EntryPaymentReversal); err != nil {
		return LedgerEntry{}, err
	}
	if err := insertBillingOutbox(ctx, q, "payment_order", normalized.PaymentOrderID, "billing.payment_refund.completed", key, map[string]any{
		"payment_order_id": normalized.PaymentOrderID,
		"wallet_id":        wallet.ID,
		"ledger_id":        entry.ID,
		"amount_minor":     normalized.AmountMinor,
		"currency":         normalized.Currency,
	}); err != nil {
		return LedgerEntry{}, err
	}
	return entry, nil
}

// ReleasePaymentRefundInTx returns a refund hold after a definitive provider
// rejection. Retryable or ambiguous provider failures must keep the hold.
func (s *Service) ReleasePaymentRefundInTx(ctx context.Context, q data.Querier, input PaymentRefundInput) (LedgerEntry, error) {
	normalized, err := normalizePaymentRefundInput(input)
	if err != nil || s == nil || s.repository == nil || q == nil {
		return LedgerEntry{}, ErrInvalidInput
	}
	now := s.now()
	wallet, err := getOrCreateWallet(ctx, q, normalized.UserID, normalized.Currency, now)
	if err != nil {
		return LedgerEntry{}, err
	}
	key := paymentRefundLedgerKey("release", normalized.PaymentOrderID)
	if existing, err := loadLedgerByIdempotency(ctx, q, wallet.ID, key); err == nil {
		if !samePaymentRefundEntry(existing, EntryPaymentRefundRelease, normalized, -normalized.AmountMinor) {
			return LedgerEntry{}, ErrSettlementConflict
		}
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	if err := requirePaymentRefundHold(ctx, q, wallet.ID, normalized); err != nil {
		return LedgerEntry{}, err
	}
	if wallet.ReservedBalanceMinor < normalized.AmountMinor {
		return LedgerEntry{}, ErrReservationConflict
	}
	availableAfter, ok := safeAdd(wallet.AvailableBalanceMinor, normalized.AmountMinor)
	if !ok {
		return LedgerEntry{}, ErrBalanceOverflow
	}
	reservedAfter := wallet.ReservedBalanceMinor - normalized.AmountMinor
	if _, err := q.Exec(ctx, `
		UPDATE wallets
		SET available_balance_minor = $2, reserved_balance_minor = $3,
			version = version + 1, updated_at = $4
		WHERE id = $1`, wallet.ID, availableAfter, reservedAfter, now); err != nil {
		return LedgerEntry{}, mapRepositoryError(err)
	}
	entry, err := insertLedger(ctx, q, ledgerInsert{
		WalletID:                   wallet.ID,
		EntryType:                  EntryPaymentRefundRelease,
		AmountMinor:                -normalized.AmountMinor,
		AvailableDeltaMinor:        normalized.AmountMinor,
		ReservedDeltaMinor:         -normalized.AmountMinor,
		AvailableBalanceAfterMinor: availableAfter,
		ReservedBalanceAfterMinor:  reservedAfter,
		Currency:                   wallet.Currency,
		ReferenceType:              "payment_order",
		ReferenceID:                normalized.PaymentOrderID.String(),
		IdempotencyKey:             key,
		OperatorUserID:             uuidPointer(normalized.OperatorUserID),
		Source:                     normalized.Source,
		CreatedAt:                  now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}
	if err := auditPaymentRefundLedger(ctx, q, normalized, wallet.ID, EntryPaymentRefundRelease); err != nil {
		return LedgerEntry{}, err
	}
	if err := insertBillingOutbox(ctx, q, "payment_order", normalized.PaymentOrderID, "billing.payment_refund.released", key, map[string]any{
		"payment_order_id": normalized.PaymentOrderID,
		"wallet_id":        wallet.ID,
		"ledger_id":        entry.ID,
		"amount_minor":     normalized.AmountMinor,
		"currency":         normalized.Currency,
	}); err != nil {
		return LedgerEntry{}, err
	}
	return entry, nil
}

func normalizePaymentRefundInput(input PaymentRefundInput) (PaymentRefundInput, error) {
	if input.UserID == uuid.Nil || input.PaymentOrderID == uuid.Nil || input.OperatorUserID == uuid.Nil || input.AmountMinor <= 0 {
		return PaymentRefundInput{}, ErrInvalidInput
	}
	input.Currency = normalizeCurrency(input.Currency)
	input.Source = normalizeText(input.Source, 64)
	if input.Currency == "" || input.Source == "" {
		return PaymentRefundInput{}, ErrInvalidInput
	}
	return input, nil
}

func requirePaymentRefundHold(ctx context.Context, q data.Querier, walletID uuid.UUID, input PaymentRefundInput) error {
	hold, err := loadLedgerByIdempotency(ctx, q, walletID, paymentRefundLedgerKey("hold", input.PaymentOrderID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReservationNotFound
		}
		return mapRepositoryError(err)
	}
	if !samePaymentRefundEntry(hold, EntryPaymentRefundHold, input, input.AmountMinor) {
		return ErrReservationConflict
	}
	return nil
}

func samePaymentRefundEntry(entry LedgerEntry, entryType string, input PaymentRefundInput, amount int64) bool {
	return entry.EntryType == entryType &&
		entry.AmountMinor == amount &&
		entry.Currency == input.Currency &&
		entry.ReferenceType == "payment_order" &&
		entry.ReferenceID == input.PaymentOrderID.String() &&
		entry.Source == input.Source &&
		equalUUID(entry.OperatorUserID, uuidPointer(input.OperatorUserID))
}

func paymentRefundLedgerKey(action string, orderID uuid.UUID) string {
	return "payment:refund:" + action + ":v1:" + orderID.String()
}

func auditPaymentRefundLedger(ctx context.Context, q data.Querier, input PaymentRefundInput, walletID uuid.UUID, action string) error {
	operator := input.OperatorUserID
	if err := audit.Insert(ctx, q, audit.Event{
		ActorUserID: &operator,
		Action:      "wallet." + action,
		TargetType:  "wallet",
		TargetID:    walletID.String(),
		RequestID:   input.PaymentOrderID.String(),
		Result:      "success",
	}); err != nil {
		return fmt.Errorf("audit payment refund ledger: %w", err)
	}
	return nil
}
