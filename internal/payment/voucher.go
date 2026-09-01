package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
	"github.com/starhui-dev/bablo/internal/secret"
)

const voucherColumns = `id, code_hash, code_prefix, amount_minor, btrim(currency), status,
	expires_at, created_by, redeemed_by, redeemed_at, created_at, updated_at,
	code_ciphertext, code_nonce, COALESCE(code_key_version, '')`

func (r *Repository) CreateVoucher(ctx context.Context, input CreateVoucherInput, code string, codeHash [sha256.Size]byte, prefix string, now time.Time) (CreatedVoucher, error) {
	actualHash := sha256.Sum256([]byte(code))
	if r == nil || r.voucherKeys == nil || !validVoucherCode(code) || !bytes.Equal(actualHash[:], codeHash[:]) || !strings.HasPrefix(code, prefix) || len(prefix) < 8 {
		return CreatedVoucher{}, ErrProviderUnavailable
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result CreatedVoucher
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		existing, err := loadVoucherByIdempotency(dbCtx, q, input.IdempotencyKey, true)
		switch {
		case err == nil:
			return replayCreatedVoucher(r.voucherKeys, existing, input, &result)
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}

		voucherID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate payment voucher UUIDv7: %w", err)
		}
		sealed, err := r.voucherKeys.Seal([]byte(code), voucherAAD(voucherID))
		if err != nil {
			return fmt.Errorf("encrypt payment voucher code: %w", err)
		}
		value, err := scanVoucher(q.QueryRow(dbCtx, `
			INSERT INTO payment_vouchers (
				id, code_hash, code_prefix, amount_minor, currency, idempotency_key,
				status, expires_at, created_by, created_at, updated_at,
				code_ciphertext, code_nonce, code_key_version
			)
			VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9, $9, $10, $11, $12)
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING `+voucherColumns,
			voucherID, codeHash[:], prefix, input.AmountMinor, input.Currency,
			input.IdempotencyKey, input.ExpiresAt, input.OperatorUserID, now,
			sealed.Ciphertext, sealed.Nonce, sealed.KeyVersion,
		))
		if errors.Is(err, pgx.ErrNoRows) {
			existing, loadErr := loadVoucherByIdempotency(dbCtx, q, input.IdempotencyKey, true)
			if loadErr != nil {
				return mapRepositoryError(loadErr)
			}
			return replayCreatedVoucher(r.voucherKeys, existing, input, &result)
		}
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(dbCtx, q, audit.Event{
			ActorUserID: &input.OperatorUserID, Action: "payment.voucher.created",
			TargetType: "payment_voucher", TargetID: value.ID.String(),
			RequestID: input.RequestID, Result: "success",
		}); err != nil {
			return err
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_voucher", value.ID, "payment.voucher.created", "payment:voucher-create:v2:"+value.ID.String(), map[string]any{
			"voucher_id": value.ID, "code_prefix": value.CodePrefix,
			"amount_minor": value.AmountMinor, "currency": value.Currency,
		}); err != nil {
			return err
		}
		result = CreatedVoucher{Voucher: value, Code: code}
		return nil
	})
	if err != nil {
		return CreatedVoucher{}, err
	}
	return result, nil
}

func (r *Repository) RedeemVoucher(ctx context.Context, input RedeemVoucherInput, codeHash [sha256.Size]byte, now time.Time) (VoucherRedemption, error) {
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result VoucherRedemption
	var committedError error
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		voucher, err := loadVoucherByHash(dbCtx, q, codeHash, true)
		if errors.Is(err, pgx.ErrNoRows) {
			committedError = ErrVoucherInvalid
			return nil
		}
		if err != nil {
			return mapRepositoryError(err)
		}
		if voucher.Status == VoucherRedeemed && voucher.RedeemedBy != nil && *voucher.RedeemedBy == input.UserID {
			ledger, ledgerErr := loadVoucherLedger(dbCtx, q, voucher.ID, input.UserID, voucher.Currency)
			if ledgerErr != nil {
				return ledgerErr
			}
			result = VoucherRedemption{Voucher: voucher, Ledger: ledger}
			return nil
		}
		if voucher.Status != VoucherActive {
			committedError = ErrVoucherUnavailable
			return nil
		}
		if voucher.ExpiresAt != nil && !voucher.ExpiresAt.After(now) {
			if _, err := q.Exec(dbCtx, `
				UPDATE payment_vouchers
				SET status = 'expired', code_ciphertext = NULL, code_nonce = NULL,
					code_key_version = NULL, updated_at = $2
				WHERE id = $1`, voucher.ID, now); err != nil {
				return mapRepositoryError(err)
			}
			committedError = ErrVoucherUnavailable
			return nil
		}
		ledger, err := r.billing.CreditInTx(dbCtx, q, billing.CreditInput{
			UserID: input.UserID, Currency: voucher.Currency, EntryType: billing.EntryGrant,
			AmountMinor: voucher.AmountMinor, ReferenceType: "payment_voucher",
			ReferenceID: voucher.ID.String(), IdempotencyKey: "payment:voucher:v1:" + voucher.ID.String(),
			Source: "payment",
		})
		if err != nil {
			return err
		}
		redeemedAt := now
		value, err := scanVoucher(q.QueryRow(dbCtx, `
			UPDATE payment_vouchers
			SET status = 'redeemed', redeemed_by = $2, redeemed_at = $3,
				code_ciphertext = NULL, code_nonce = NULL, code_key_version = NULL, updated_at = $3
			WHERE id = $1
			RETURNING `+voucherColumns, voucher.ID, input.UserID, redeemedAt))
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(dbCtx, q, audit.Event{
			ActorUserID: &input.UserID, Action: "payment.voucher.redeemed",
			TargetType: "payment_voucher", TargetID: voucher.ID.String(),
			RequestID: input.RequestID, Result: "success",
		}); err != nil {
			return err
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_voucher", voucher.ID, "payment.voucher.redeemed", "payment:voucher-redeem:v1:"+voucher.ID.String(), map[string]any{
			"voucher_id": voucher.ID, "user_id": input.UserID, "wallet_id": ledger.WalletID,
			"ledger_id": ledger.ID, "amount_minor": voucher.AmountMinor, "currency": voucher.Currency,
		}); err != nil {
			return err
		}
		result = VoucherRedemption{Voucher: value, Ledger: ledger}
		return nil
	})
	if err != nil {
		return VoucherRedemption{}, err
	}
	if committedError != nil {
		return VoucherRedemption{}, committedError
	}
	return result, nil
}

func (r *Repository) RevokeVoucher(ctx context.Context, operatorUserID, voucherID uuid.UUID, requestID string, now time.Time) (Voucher, error) {
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Voucher
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		voucher, err := loadVoucherByID(dbCtx, q, voucherID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if voucher.Status == VoucherRevoked {
			result = voucher
			return nil
		}
		if voucher.Status != VoucherActive {
			return ErrVoucherUnavailable
		}
		value, err := scanVoucher(q.QueryRow(dbCtx, `
			UPDATE payment_vouchers
			SET status = 'revoked', code_ciphertext = NULL, code_nonce = NULL,
				code_key_version = NULL, updated_at = $2
			WHERE id = $1
			RETURNING `+voucherColumns, voucher.ID, now))
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(dbCtx, q, audit.Event{
			ActorUserID: &operatorUserID, Action: "payment.voucher.revoked",
			TargetType: "payment_voucher", TargetID: voucher.ID.String(),
			RequestID: requestID, Result: "success",
		}); err != nil {
			return err
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_voucher", voucher.ID, "payment.voucher.revoked", "payment:voucher-revoke:v1:"+voucher.ID.String(), map[string]any{"voucher_id": voucher.ID}); err != nil {
			return err
		}
		result = value
		return nil
	})
	if err != nil {
		return Voucher{}, err
	}
	return result, nil
}

func loadVoucherByHash(ctx context.Context, q data.Querier, hash [sha256.Size]byte, forUpdate bool) (Voucher, error) {
	query := `SELECT ` + voucherColumns + ` FROM payment_vouchers WHERE code_hash = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanVoucher(q.QueryRow(ctx, query, hash[:]))
}

func loadVoucherByID(ctx context.Context, q data.Querier, voucherID uuid.UUID, forUpdate bool) (Voucher, error) {
	query := `SELECT ` + voucherColumns + ` FROM payment_vouchers WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanVoucher(q.QueryRow(ctx, query, voucherID))
}

func loadVoucherByIdempotency(ctx context.Context, q data.Querier, idempotencyKey string, forUpdate bool) (Voucher, error) {
	query := `SELECT ` + voucherColumns + ` FROM payment_vouchers WHERE idempotency_key = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanVoucher(q.QueryRow(ctx, query, idempotencyKey))
}

func replayCreatedVoucher(keys *secret.Keyring, voucher Voucher, input CreateVoucherInput, result *CreatedVoucher) error {
	if voucher.AmountMinor != input.AmountMinor || voucher.Currency != input.Currency || voucher.CreatedBy != input.OperatorUserID || !sameOptionalTime(voucher.ExpiresAt, input.ExpiresAt) {
		return ErrConflict
	}
	if voucher.Status != VoucherActive || len(voucher.CodeCiphertext) == 0 || len(voucher.CodeNonce) == 0 || voucher.CodeKeyVersion == "" {
		return ErrVoucherUnavailable
	}
	plaintext, err := keys.Open(secret.Sealed{
		Ciphertext: voucher.CodeCiphertext,
		Nonce:      voucher.CodeNonce,
		KeyVersion: voucher.CodeKeyVersion,
	}, voucherAAD(voucher.ID))
	if err != nil {
		return fmt.Errorf("decrypt payment voucher code: %w", err)
	}
	code := string(plaintext)
	hash := sha256.Sum256(plaintext)
	if !validVoucherCode(code) || !bytes.Equal(hash[:], voucher.CodeHash) || !strings.HasPrefix(code, voucher.CodePrefix) {
		return ErrConflict
	}
	*result = CreatedVoucher{Voucher: voucher, Code: code}
	return nil
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func voucherAAD(voucherID uuid.UUID) []byte {
	return []byte("bablo:payment:voucher:" + voucherID.String())
}

func loadVoucherLedger(ctx context.Context, q data.Querier, voucherID, userID uuid.UUID, currency string) (billing.LedgerEntry, error) {
	var walletID uuid.UUID
	if err := q.QueryRow(ctx, `SELECT id FROM wallets WHERE user_id = $1 AND currency = $2`, userID, currency).Scan(&walletID); err != nil {
		return billing.LedgerEntry{}, mapRepositoryError(err)
	}
	var value billing.LedgerEntry
	if err := q.QueryRow(ctx, `
		SELECT id, wallet_id, entry_type, amount_minor,
			available_delta_minor, reserved_delta_minor,
			available_balance_after_minor, reserved_balance_after_minor,
			btrim(currency), reference_type, reference_id, idempotency_key,
			usage_event_id, operator_user_id, source, created_at
		FROM wallet_ledger
		WHERE wallet_id = $1 AND idempotency_key = $2`,
		walletID, "payment:voucher:v1:"+voucherID.String()).Scan(
		&value.ID, &value.WalletID, &value.EntryType, &value.AmountMinor,
		&value.AvailableDeltaMinor, &value.ReservedDeltaMinor,
		&value.AvailableBalanceAfterMinor, &value.ReservedBalanceAfterMinor,
		&value.Currency, &value.ReferenceType, &value.ReferenceID, &value.IdempotencyKey,
		&value.UsageEventID, &value.OperatorUserID, &value.Source, &value.CreatedAt,
	); err != nil {
		return billing.LedgerEntry{}, mapRepositoryError(err)
	}
	return value, nil
}

func scanVoucher(row rowScanner) (Voucher, error) {
	var value Voucher
	if err := row.Scan(
		&value.ID, &value.CodeHash, &value.CodePrefix, &value.AmountMinor, &value.Currency, &value.Status,
		&value.ExpiresAt, &value.CreatedBy, &value.RedeemedBy, &value.RedeemedAt,
		&value.CreatedAt, &value.UpdatedAt,
		&value.CodeCiphertext, &value.CodeNonce, &value.CodeKeyVersion,
	); err != nil {
		return Voucher{}, err
	}
	if len(value.CodeHash) != sha256.Size {
		return Voucher{}, ErrConflict
	}
	return value, nil
}
