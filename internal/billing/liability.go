package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

const liabilityColumns = `id, wallet_id, liability_type, reference_type, reference_id,
	principal_amount_minor, recovered_amount_minor, btrim(currency), status, created_at, updated_at`

func (s *Service) OpenLiabilityInTx(ctx context.Context, q data.Querier, input LiabilityInput) (Liability, error) {
	if s == nil || s.repository == nil || q == nil {
		return Liability{}, ErrInvalidInput
	}
	input, err := normalizeLiabilityInput(input)
	if err != nil {
		return Liability{}, err
	}
	return s.repository.OpenLiabilityInTx(ctx, q, input, s.now())
}

func (s *Service) ReverseLiabilityInTx(ctx context.Context, q data.Querier, liabilityID uuid.UUID) (Liability, error) {
	if s == nil || s.repository == nil || q == nil || liabilityID == uuid.Nil {
		return Liability{}, ErrInvalidInput
	}
	return s.repository.ReverseLiabilityInTx(ctx, q, liabilityID, s.now())
}

func (r *Repository) OpenLiabilityInTx(ctx context.Context, q data.Querier, input LiabilityInput, now time.Time) (Liability, error) {
	if r == nil || q == nil {
		return Liability{}, ErrInvalidInput
	}
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet-liability:' || $1 || ':' || $2, 0))`, input.ReferenceType, input.ReferenceID); err != nil {
		return Liability{}, fmt.Errorf("lock wallet liability reference: %w", err)
	}
	existing, err := loadLiabilityByReference(ctx, q, input.ReferenceType, input.ReferenceID, true)
	switch {
	case err == nil:
		existingWallet, walletErr := loadWalletByID(ctx, q, existing.WalletID, false)
		if walletErr != nil {
			return Liability{}, mapRepositoryError(walletErr)
		}
		if existingWallet.UserID != input.UserID || existingWallet.Currency != input.Currency || !sameLiabilityInput(existing, input) {
			return Liability{}, ErrSettlementConflict
		}
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Liability{}, mapRepositoryError(err)
	}
	wallet, err := getOrCreateWallet(ctx, q, input.UserID, input.Currency, now)
	if err != nil {
		return Liability{}, err
	}
	recovered := input.AmountMinor
	if recovered > wallet.AvailableBalanceMinor {
		recovered = wallet.AvailableBalanceMinor
	}
	availableAfter := wallet.AvailableBalanceMinor - recovered
	status := "settled"
	if recovered < input.AmountMinor {
		status = "open"
	}
	liabilityID, err := id.New()
	if err != nil {
		return Liability{}, fmt.Errorf("generate wallet liability UUIDv7: %w", err)
	}
	value, err := scanLiability(q.QueryRow(ctx, `
		INSERT INTO wallet_liabilities (
			id, wallet_id, liability_type, reference_type, reference_id,
			principal_amount_minor, recovered_amount_minor, currency,
			status, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		RETURNING `+liabilityColumns,
		liabilityID, wallet.ID, input.LiabilityType, input.ReferenceType,
		input.ReferenceID, input.AmountMinor, recovered, input.Currency, status, now,
	))
	if err != nil {
		return Liability{}, mapRepositoryError(err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE wallets
		SET available_balance_minor = $2,
			financial_hold = financial_hold OR $3,
			version = version + 1, updated_at = $4
		WHERE id = $1`, wallet.ID, availableAfter, status == "open", now); err != nil {
		return Liability{}, mapRepositoryError(err)
	}
	if recovered > 0 {
		entry, err := insertLedger(ctx, q, ledgerInsert{
			WalletID: wallet.ID, EntryType: EntryPaymentLiability,
			AmountMinor: -recovered, AvailableDeltaMinor: -recovered,
			ReservedDeltaMinor: 0, AvailableBalanceAfterMinor: availableAfter,
			ReservedBalanceAfterMinor: wallet.ReservedBalanceMinor,
			Currency:                  wallet.Currency, ReferenceType: "wallet_liability",
			ReferenceID: liabilityID.String(), IdempotencyKey: "liability-open:v1:" + liabilityID.String(),
			Source: "payment", CreatedAt: now,
		})
		if err != nil {
			return Liability{}, err
		}
		value.RecoveryLedgerID = uuidPointer(entry.ID)
	}
	if err := insertBillingOutbox(ctx, q, "wallet_liability", value.ID, "billing.liability.opened", "liability-open:v1:"+value.ID.String(), map[string]any{
		"liability_id": value.ID, "wallet_id": value.WalletID,
		"principal_amount_minor": value.PrincipalAmountMinor,
		"recovered_amount_minor": value.RecoveredAmountMinor,
		"currency":               value.Currency, "status": value.Status,
	}); err != nil {
		return Liability{}, err
	}
	return value, nil
}

func (r *Repository) ReverseLiabilityInTx(ctx context.Context, q data.Querier, liabilityID uuid.UUID, now time.Time) (Liability, error) {
	liability, err := loadLiabilityByID(ctx, q, liabilityID, true)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Liability{}, ErrSettlementConflict
		}
		return Liability{}, mapRepositoryError(err)
	}
	if liability.Status == "reversed" {
		return liability, nil
	}
	wallet, err := loadWalletByID(ctx, q, liability.WalletID, true)
	if err != nil {
		return Liability{}, mapRepositoryError(err)
	}
	availableAfter, ok := safeAdd(wallet.AvailableBalanceMinor, liability.RecoveredAmountMinor)
	if !ok {
		return Liability{}, ErrBalanceOverflow
	}
	value, err := scanLiability(q.QueryRow(ctx, `
		UPDATE wallet_liabilities
		SET status = 'reversed', updated_at = $2
		WHERE id = $1
		RETURNING `+liabilityColumns, liability.ID, now))
	if err != nil {
		return Liability{}, mapRepositoryError(err)
	}
	sourceLedgerID := liability.ID
	if liability.RecoveredAmountMinor > 0 {
		entry, err := insertLedger(ctx, q, ledgerInsert{
			WalletID: wallet.ID, EntryType: EntryPaymentLiability,
			AmountMinor:         liability.RecoveredAmountMinor,
			AvailableDeltaMinor: liability.RecoveredAmountMinor,
			ReservedDeltaMinor:  0, AvailableBalanceAfterMinor: availableAfter,
			ReservedBalanceAfterMinor: wallet.ReservedBalanceMinor,
			Currency:                  wallet.Currency, ReferenceType: "wallet_liability",
			ReferenceID: liability.ID.String(), IdempotencyKey: "liability-reversal:v1:" + liability.ID.String(),
			Source: "payment", CreatedAt: now,
		})
		if err != nil {
			return Liability{}, err
		}
		value.RecoveryLedgerID = uuidPointer(entry.ID)
		sourceLedgerID = entry.ID
	}
	if _, err := q.Exec(ctx, `
		UPDATE wallets
		SET available_balance_minor = $2,
			financial_hold = EXISTS (
				SELECT 1 FROM wallet_liabilities
				WHERE wallet_id = $1 AND status = 'open'
			) OR EXISTS (
				SELECT 1
				FROM billing_settlements settlement
				JOIN wallet_reservations reservation ON reservation.id = settlement.reservation_id
				WHERE reservation.wallet_id = $1 AND settlement.status = 'pending'
			), version = version + 1, updated_at = $3
		WHERE id = $1`, wallet.ID, availableAfter, now); err != nil {
		return Liability{}, mapRepositoryError(err)
	}
	wallet.AvailableBalanceMinor = availableAfter
	if liability.RecoveredAmountMinor > 0 {
		if err := r.recoverWalletLiabilities(ctx, q, wallet, availableAfter, sourceLedgerID, now); err != nil {
			return Liability{}, err
		}
	}
	if err := insertBillingOutbox(ctx, q, "wallet_liability", value.ID, "billing.liability.reversed", "liability-reversal:v1:"+value.ID.String(), map[string]any{
		"liability_id": value.ID, "wallet_id": value.WalletID,
		"recovered_amount_minor": value.RecoveredAmountMinor,
		"currency":               value.Currency,
	}); err != nil {
		return Liability{}, err
	}
	return value, nil
}

func (r *Repository) recoverWalletLiabilities(ctx context.Context, q data.Querier, wallet Wallet, available int64, sourceLedgerID uuid.UUID, now time.Time) error {
	rows, err := q.Query(ctx, `
		SELECT `+liabilityColumns+`
		FROM wallet_liabilities
		WHERE wallet_id = $1 AND status = 'open'
		ORDER BY created_at, id
		FOR UPDATE`, wallet.ID)
	if err != nil {
		return mapRepositoryError(err)
	}
	liabilities := make([]Liability, 0)
	for rows.Next() {
		value, scanErr := scanLiability(rows)
		if scanErr != nil {
			rows.Close()
			return mapRepositoryError(scanErr)
		}
		liabilities = append(liabilities, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapRepositoryError(err)
	}
	rows.Close()
	if len(liabilities) == 0 {
		return nil
	}
	for _, liability := range liabilities {
		if available == 0 {
			break
		}
		outstanding := liability.PrincipalAmountMinor - liability.RecoveredAmountMinor
		recovered := outstanding
		if recovered > available {
			recovered = available
		}
		available -= recovered
		recoveredTotal := liability.RecoveredAmountMinor + recovered
		status := "open"
		if recoveredTotal == liability.PrincipalAmountMinor {
			status = "settled"
		}
		if _, err := q.Exec(ctx, `
			UPDATE wallet_liabilities
			SET recovered_amount_minor = $2, status = $3, updated_at = $4
			WHERE id = $1`, liability.ID, recoveredTotal, status, now); err != nil {
			return mapRepositoryError(err)
		}
		if _, err := insertLedger(ctx, q, ledgerInsert{
			WalletID: wallet.ID, EntryType: EntryPaymentLiability,
			AmountMinor: -recovered, AvailableDeltaMinor: -recovered,
			ReservedDeltaMinor: 0, AvailableBalanceAfterMinor: available,
			ReservedBalanceAfterMinor: wallet.ReservedBalanceMinor,
			Currency:                  wallet.Currency, ReferenceType: "wallet_liability",
			ReferenceID:    liability.ID.String(),
			IdempotencyKey: "liability-recovery:v1:" + liability.ID.String() + ":" + sourceLedgerID.String(),
			Source:         "payment", CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	if _, err := q.Exec(ctx, `
		UPDATE wallets
		SET available_balance_minor = $2,
			financial_hold = EXISTS (
				SELECT 1 FROM wallet_liabilities
				WHERE wallet_id = $1 AND status = 'open'
			) OR EXISTS (
				SELECT 1
				FROM billing_settlements settlement
				JOIN wallet_reservations reservation ON reservation.id = settlement.reservation_id
				WHERE reservation.wallet_id = $1 AND settlement.status = 'pending'
			), version = version + 1, updated_at = $3
		WHERE id = $1`, wallet.ID, available, now); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func loadLiabilityByReference(ctx context.Context, q data.Querier, referenceType, referenceID string, forUpdate bool) (Liability, error) {
	query := `SELECT ` + liabilityColumns + ` FROM wallet_liabilities WHERE reference_type = $1 AND reference_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanLiability(q.QueryRow(ctx, query, referenceType, referenceID))
}

func loadLiabilityByID(ctx context.Context, q data.Querier, liabilityID uuid.UUID, forUpdate bool) (Liability, error) {
	query := `SELECT ` + liabilityColumns + ` FROM wallet_liabilities WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanLiability(q.QueryRow(ctx, query, liabilityID))
}

func scanLiability(row rowScanner) (Liability, error) {
	var value Liability
	if err := row.Scan(
		&value.ID, &value.WalletID, &value.LiabilityType,
		&value.ReferenceType, &value.ReferenceID,
		&value.PrincipalAmountMinor, &value.RecoveredAmountMinor,
		&value.Currency, &value.Status, &value.CreatedAt, &value.UpdatedAt,
	); err != nil {
		return Liability{}, err
	}
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	return value, nil
}

func normalizeLiabilityInput(input LiabilityInput) (LiabilityInput, error) {
	input.Currency = normalizeCurrency(input.Currency)
	input.LiabilityType = normalizeText(input.LiabilityType, 64)
	input.ReferenceType = normalizeText(input.ReferenceType, 64)
	input.ReferenceID = normalizeText(input.ReferenceID, 160)
	if input.UserID == uuid.Nil || input.AmountMinor <= 0 || input.Currency == "" ||
		(input.LiabilityType != "payment_refund" && input.LiabilityType != "payment_dispute") ||
		input.ReferenceType == "" || input.ReferenceID == "" {
		return LiabilityInput{}, ErrInvalidInput
	}
	return input, nil
}

func sameLiabilityInput(value Liability, input LiabilityInput) bool {
	return value.LiabilityType == input.LiabilityType && value.ReferenceType == input.ReferenceType &&
		value.ReferenceID == input.ReferenceID && value.PrincipalAmountMinor == input.AmountMinor &&
		value.Currency == input.Currency
}
