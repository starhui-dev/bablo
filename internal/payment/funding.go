package payment

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/data"
)

type fundingOperation struct {
	UserID         uuid.UUID
	Currency       string
	AmountMinor    int64
	OperatorUserID uuid.UUID
	LedgerID       uuid.UUID
}

func (r *Repository) ManualRecharge(ctx context.Context, input ManualRechargeInput, idempotencyKey string, now time.Time) (billing.LedgerEntry, error) {
	if r == nil || idempotencyKey == "" || now.IsZero() {
		return billing.LedgerEntry{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result billing.LedgerEntry
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		existing, err := loadFundingOperation(dbCtx, q, idempotencyKey)
		if err == nil && !sameFundingOperation(existing, input) {
			return ErrConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return mapRepositoryError(err)
		}
		entry, err := r.billing.CreditInTx(dbCtx, q, billing.CreditInput{
			UserID: input.UserID, Currency: input.Currency, EntryType: billing.EntryRecharge,
			AmountMinor: input.AmountMinor, ReferenceType: "admin_recharge",
			ReferenceID: idempotencyKey, IdempotencyKey: idempotencyKey,
			RequestID: input.RequestID, OperatorUserID: &input.OperatorUserID, Source: "admin",
		})
		if err != nil {
			return err
		}
		if existing.LedgerID != uuid.Nil {
			if existing.LedgerID != entry.ID {
				return ErrConflict
			}
			result = entry
			return nil
		}
		command, err := q.Exec(dbCtx, `
			INSERT INTO payment_funding_operations (
				idempotency_key, user_id, currency, amount_minor,
				operator_user_id, ledger_id, created_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (idempotency_key) DO NOTHING`,
			idempotencyKey, input.UserID, input.Currency, input.AmountMinor,
			input.OperatorUserID, entry.ID, now.UTC())
		if err != nil {
			return mapRepositoryError(err)
		}
		if command.RowsAffected() == 0 {
			existing, err = loadFundingOperation(dbCtx, q, idempotencyKey)
			if err != nil {
				return mapRepositoryError(err)
			}
			if !sameFundingOperation(existing, input) || existing.LedgerID != entry.ID {
				return ErrConflict
			}
		}
		result = entry
		return nil
	})
	if err != nil {
		return billing.LedgerEntry{}, err
	}
	return result, nil
}

func loadFundingOperation(ctx context.Context, q data.Querier, idempotencyKey string) (fundingOperation, error) {
	var value fundingOperation
	err := q.QueryRow(ctx, `
		SELECT user_id, btrim(currency), amount_minor, operator_user_id, ledger_id
		FROM payment_funding_operations
		WHERE idempotency_key = $1
		FOR UPDATE`, idempotencyKey).Scan(
		&value.UserID, &value.Currency, &value.AmountMinor,
		&value.OperatorUserID, &value.LedgerID,
	)
	return value, err
}

func sameFundingOperation(existing fundingOperation, input ManualRechargeInput) bool {
	return existing.UserID == input.UserID &&
		existing.Currency == input.Currency &&
		existing.AmountMinor == input.AmountMinor &&
		existing.OperatorUserID == input.OperatorUserID
}
