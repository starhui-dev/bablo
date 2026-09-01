package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

const (
	providerOperationLease     = 30 * time.Second
	providerOperationRetryBase = 5 * time.Second
)

const providerOperationColumns = `id, payment_order_id, operation_type, payload_sha256,
	merchant_id, provider_live_mode, status, owner_token, lease_expires_at, next_attempt_at, attempts,
	COALESCE(provider_reference, ''), COALESCE(last_error_class, ''), created_at, updated_at`

// ClaimProviderOperation serializes an external provider call durably. The
// lease is intentionally short: a crashed caller can be retried, while the
// provider's stable idempotency key prevents a second financial operation.
func ensureProviderOperation(ctx context.Context, q data.Querier, order Order, operationType string, identity ProviderIdentity, now time.Time) error {
	identity, err := normalizeProviderIdentity(identity)
	if err != nil || order.ID == uuid.Nil || (operationType != OperationCreate && operationType != OperationRefund) {
		return ErrInvalidInput
	}
	payload := providerOperationPayload(operationType, order, identity)
	operationID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate provider operation UUIDv7: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO payment_provider_operations (
			id, payment_order_id, operation_type, payload_sha256,
			merchant_id, provider_live_mode, status, next_attempt_at,
			attempts, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, 0, $7, $7)
		ON CONFLICT (payment_order_id, operation_type) DO NOTHING`,
		operationID, order.ID, operationType, payload[:], identity.MerchantID, identity.LiveMode, now); err != nil {
		return mapRepositoryError(err)
	}
	operation, err := loadProviderOperation(ctx, q, order.ID, operationType, true)
	if err != nil {
		return mapRepositoryError(err)
	}
	if !bytes.Equal(operation.PayloadSHA256[:], payload[:]) || operation.MerchantID != identity.MerchantID || operation.ProviderLiveMode != identity.LiveMode {
		return ErrConflict
	}
	return nil
}

func (r *Repository) ClaimProviderOperation(ctx context.Context, orderID uuid.UUID, operationType string, identity ProviderIdentity, payload [32]byte, now time.Time) (ProviderOperationClaim, error) {
	identity, identityErr := normalizeProviderIdentity(identity)
	if r == nil || orderID == uuid.Nil || identityErr != nil || (operationType != OperationCreate && operationType != OperationRefund) {
		return ProviderOperationClaim{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result ProviderOperationClaim
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByID(dbCtx, q, orderID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		operation, err := loadProviderOperation(dbCtx, q, orderID, operationType, true)
		if errors.Is(err, pgx.ErrNoRows) {
			owner, err := id.New()
			if err != nil {
				return fmt.Errorf("generate provider operation owner UUIDv7: %w", err)
			}
			operationID, err := id.New()
			if err != nil {
				return fmt.Errorf("generate provider operation UUIDv7: %w", err)
			}
			operation, err = scanProviderOperation(q.QueryRow(dbCtx, `
				INSERT INTO payment_provider_operations (
					id, payment_order_id, operation_type, payload_sha256,
					merchant_id, provider_live_mode, status, owner_token,
					lease_expires_at, next_attempt_at, attempts, created_at, updated_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, 'processing', $7, $8, $9, 1, $9, $9)
				RETURNING `+providerOperationColumns,
				operationID, order.ID, operationType, payload[:], identity.MerchantID, identity.LiveMode,
				owner, now.Add(providerOperationLease), now))
			if err != nil {
				return mapRepositoryError(err)
			}
			result = ProviderOperationClaim{Order: order, Operation: operation, Claimed: true}
			return nil
		}
		if err != nil {
			return mapRepositoryError(err)
		}
		if !bytes.Equal(operation.PayloadSHA256[:], payload[:]) || operation.MerchantID != identity.MerchantID || operation.ProviderLiveMode != identity.LiveMode {
			return ErrConflict
		}
		if operation.Status == OperationSucceeded || operation.Status == OperationDefinitiveFailed {
			result = ProviderOperationClaim{Order: order, Operation: operation}
			return nil
		}
		if operation.Status == OperationProcessing && operation.LeaseExpiresAt != nil && operation.LeaseExpiresAt.After(now) {
			result = ProviderOperationClaim{Order: order, Operation: operation}
			return nil
		}
		if operation.Status == OperationRetryable && operation.NextAttemptAt.After(now) {
			result = ProviderOperationClaim{Order: order, Operation: operation}
			return nil
		}
		owner, err := id.New()
		if err != nil {
			return fmt.Errorf("generate provider operation retry owner UUIDv7: %w", err)
		}
		operation, err = scanProviderOperation(q.QueryRow(dbCtx, `
			UPDATE payment_provider_operations
			SET status = 'processing', owner_token = $2,
				lease_expires_at = $3, next_attempt_at = $4,
				attempts = attempts + 1, last_error_class = NULL, updated_at = $4
			WHERE id = $1
			RETURNING `+providerOperationColumns,
			operation.ID, owner, now.Add(providerOperationLease), now))
		if err != nil {
			return mapRepositoryError(err)
		}
		result = ProviderOperationClaim{Order: order, Operation: operation, Claimed: true}
		return nil
	})
	if err != nil {
		return ProviderOperationClaim{}, err
	}
	return result, nil
}

func (r *Repository) CompleteCreateOperation(ctx context.Context, orderID, ownerToken uuid.UUID, checkout Checkout, now time.Time) (Order, error) {
	if r == nil || orderID == uuid.Nil || ownerToken == uuid.Nil {
		return Order{}, ErrInvalidInput
	}
	checkout, err := validateCheckout(checkout)
	if err != nil {
		return Order{}, err
	}
	encoded, err := json.Marshal(checkout.Data)
	if err != nil {
		return Order{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err = r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByID(dbCtx, q, orderID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		operation, err := loadProviderOperation(dbCtx, q, orderID, OperationCreate, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if operation.MerchantID != checkout.MerchantID || operation.ProviderLiveMode != checkout.LiveMode {
			return ErrConflict
		}
		if order.ProviderTradeNo != "" && order.ProviderTradeNo != checkout.ProviderTradeNo {
			return ErrConflict
		}
		if order.MerchantID != "" && order.MerchantID != checkout.MerchantID {
			return ErrConflict
		}
		if order.ProviderLiveMode != nil && *order.ProviderLiveMode != checkout.LiveMode {
			return ErrConflict
		}
		if operation.Status == OperationSucceeded {
			result = order
			return nil
		}
		if operation.Status != OperationProcessing || operation.OwnerToken == nil || *operation.OwnerToken != ownerToken {
			return ErrOperationPending
		}
		if order.Status != StatusCreated && order.Status != StatusPending && order.Status != StatusPaid {
			return ErrInvalidTransition
		}
		if order.Status == StatusPaid {
			if _, err := q.Exec(dbCtx, `
				UPDATE payment_provider_operations
				SET status = 'succeeded', owner_token = NULL, lease_expires_at = NULL,
					provider_reference = $2, updated_at = $3
				WHERE id = $1`, operation.ID, checkout.ProviderTradeNo, now); err != nil {
				return mapRepositoryError(err)
			}
			result = order
			return nil
		}
		expiresAt := order.ExpiresAt
		if checkout.ExpiresAt != nil && (expiresAt == nil || checkout.ExpiresAt.Before(*expiresAt)) {
			expiresAt = checkout.ExpiresAt
		}
		value, err := scanOrder(q.QueryRow(dbCtx, `
			UPDATE payment_orders
			SET status = 'pending', merchant_id = $3, provider_live_mode = $4,
				provider_trade_no = $2, checkout_data = $5,
				expires_at = $6, failure_class = NULL, updated_at = $7
			WHERE id = $1
			RETURNING `+orderColumns,
			order.ID, checkout.ProviderTradeNo, checkout.MerchantID, checkout.LiveMode, encoded, expiresAt, now))
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_order", value.ID, "payment.order.pending", "payment:pending:v2:"+value.ID.String(), map[string]any{
			"order_id": value.ID, "order_no": value.OrderNo, "provider_trade_no": value.ProviderTradeNo,
		}); err != nil {
			return err
		}
		result = value
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	return result, nil
}

func (r *Repository) FinishCreateOperation(ctx context.Context, orderID, ownerToken uuid.UUID, errorClass string, definitive bool, now time.Time) (Order, error) {
	errorClass = normalizeText(errorClass, 64)
	if r == nil || orderID == uuid.Nil || ownerToken == uuid.Nil || errorClass == "" {
		return Order{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByID(dbCtx, q, orderID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		operation, err := loadProviderOperation(dbCtx, q, orderID, OperationCreate, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if operation.Status == OperationSucceeded || operation.Status == OperationDefinitiveFailed {
			result = order
			return nil
		}
		if operation.Status != OperationProcessing || operation.OwnerToken == nil || *operation.OwnerToken != ownerToken {
			return ErrOperationPending
		}
		status := OperationRetryable
		if definitive {
			status = OperationDefinitiveFailed
		}
		nextAttemptAt := now
		if !definitive {
			nextAttemptAt = now.Add(providerOperationRetryDelay(operation.Attempts))
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE payment_provider_operations
			SET status = $2, owner_token = NULL, lease_expires_at = NULL,
				next_attempt_at = $3, last_error_class = $4, updated_at = $5
			WHERE id = $1`, operation.ID, status, nextAttemptAt, errorClass, now); err != nil {
			return mapRepositoryError(err)
		}
		if definitive && order.Status == StatusCreated {
			value, err := scanOrder(q.QueryRow(dbCtx, `
				UPDATE payment_orders
				SET status = 'failed', failure_class = $2, updated_at = $3
				WHERE id = $1
				RETURNING `+orderColumns, order.ID, errorClass, now))
			if err != nil {
				return mapRepositoryError(err)
			}
			if err := insertPaymentOutbox(dbCtx, q, "payment_order", value.ID, "payment.order.failed", "payment:create-failed:v2:"+value.ID.String(), map[string]any{
				"order_id": value.ID, "order_no": value.OrderNo, "error_class": errorClass,
			}); err != nil {
				return err
			}
			order = value
		}
		result = order
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	return result, nil
}

func (r *Repository) ClaimRefundOperation(ctx context.Context, orderID uuid.UUID, identity ProviderIdentity, payload [32]byte, now time.Time) (ProviderOperationClaim, error) {
	return r.ClaimProviderOperation(ctx, orderID, OperationRefund, identity, payload, now)
}

func (r *Repository) CompleteRefundOperation(ctx context.Context, orderID, ownerToken uuid.UUID, refundNo string, now time.Time) (Order, error) {
	refundNo = normalizeTradeNo(refundNo)
	if r == nil || orderID == uuid.Nil || ownerToken == uuid.Nil || refundNo == "" {
		return Order{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByID(dbCtx, q, orderID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		operation, err := loadProviderOperation(dbCtx, q, orderID, OperationRefund, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if operation.Status == OperationSucceeded {
			result = order
			return nil
		}
		if operation.Status != OperationProcessing || operation.OwnerToken == nil || *operation.OwnerToken != ownerToken {
			return ErrOperationPending
		}
		if order.Status != StatusRefundPending && order.Status != StatusRefunded {
			return ErrInvalidTransition
		}
		if order.ProviderRefundNo != "" && order.ProviderRefundNo != refundNo {
			return ErrConflict
		}
		value, err := scanOrder(q.QueryRow(dbCtx, `
			UPDATE payment_orders
			SET provider_refund_no = COALESCE(provider_refund_no, $2), updated_at = $3
			WHERE id = $1
			RETURNING `+orderColumns, order.ID, refundNo, now))
		if err != nil {
			return mapRepositoryError(err)
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE payment_provider_operations
			SET status = 'succeeded', owner_token = NULL, lease_expires_at = NULL,
				provider_reference = $2, next_attempt_at = $3, updated_at = $3
			WHERE id = $1`, operation.ID, refundNo, now); err != nil {
			return mapRepositoryError(err)
		}
		result = value
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	return result, nil
}

func (r *Repository) FinishRefundOperation(ctx context.Context, orderID, ownerToken uuid.UUID, errorClass string, definitive bool, now time.Time) (Order, error) {
	errorClass = normalizeText(errorClass, 64)
	if r == nil || orderID == uuid.Nil || ownerToken == uuid.Nil || errorClass == "" {
		return Order{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByID(dbCtx, q, orderID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		operation, err := loadProviderOperation(dbCtx, q, orderID, OperationRefund, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if operation.Status == OperationSucceeded || operation.Status == OperationDefinitiveFailed {
			result = order
			return nil
		}
		if operation.Status != OperationProcessing || operation.OwnerToken == nil || *operation.OwnerToken != ownerToken {
			return ErrOperationPending
		}
		status := OperationRetryable
		if definitive {
			status = OperationDefinitiveFailed
		}
		nextAttemptAt := now
		if !definitive {
			nextAttemptAt = now.Add(providerOperationRetryDelay(operation.Attempts))
			if _, err := q.Exec(dbCtx, `
				UPDATE payment_provider_operations
				SET status = $2, owner_token = NULL, lease_expires_at = NULL,
					next_attempt_at = $3, last_error_class = $4, updated_at = $5
				WHERE id = $1`, operation.ID, status, nextAttemptAt, errorClass, now); err != nil {
				return mapRepositoryError(err)
			}
			result = order
			return nil
		}
		if order.Status != StatusRefundPending || order.ProviderRefundNo != "" || order.RefundHoldLedgerID == nil || order.UpdatedBy == nil {
			return ErrRefundPending
		}
		if _, err := r.billing.ReleasePaymentRefundInTx(dbCtx, q, billing.PaymentRefundInput{
			UserID: order.UserID, Currency: order.Currency, AmountMinor: order.AmountMinor,
			PaymentOrderID: order.ID, OperatorUserID: *order.UpdatedBy, Source: "payment",
		}); err != nil {
			return err
		}
		value, err := scanOrder(q.QueryRow(dbCtx, `
			UPDATE payment_orders
			SET status = 'paid', failure_class = $2, updated_at = $3
			WHERE id = $1
			RETURNING `+orderColumns, order.ID, errorClass, now))
		if err != nil {
			return mapRepositoryError(err)
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE payment_provider_operations
			SET status = 'definitive_failed', owner_token = NULL, lease_expires_at = NULL,
				next_attempt_at = $2, last_error_class = $3, updated_at = $2
			WHERE id = $1`, operation.ID, now, errorClass); err != nil {
			return mapRepositoryError(err)
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_order", value.ID, "payment.order.refund_failed", "payment:refund-failed:v2:"+value.ID.String(), map[string]any{
			"order_id": value.ID, "order_no": value.OrderNo, "error_class": errorClass,
		}); err != nil {
			return err
		}
		result = value
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	return result, nil
}

func providerOperationRetryDelay(attempts int) time.Duration {
	delay := providerOperationRetryBase
	for attempt := 1; attempt < attempts && delay < 5*time.Minute; attempt++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func loadProviderOperation(ctx context.Context, q data.Querier, orderID uuid.UUID, operationType string, forUpdate bool) (ProviderOperation, error) {
	query := `SELECT ` + providerOperationColumns + ` FROM payment_provider_operations WHERE payment_order_id = $1 AND operation_type = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanProviderOperation(q.QueryRow(ctx, query, orderID, operationType))
}
func (r *Repository) ListDueProviderOperations(ctx context.Context, limit int, now time.Time) ([]DueProviderOperation, error) {
	if r == nil || limit < 1 || limit > 1000 {
		return nil, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	rows, err := r.store.Queryer().Query(dbCtx, `
		SELECT payment_order_id, operation_type, payload_sha256, merchant_id, provider_live_mode
		FROM payment_provider_operations
		WHERE (status IN ('pending', 'retryable') AND next_attempt_at <= $1)
		   OR (status = 'processing' AND lease_expires_at <= $1)
		ORDER BY next_attempt_at, payment_order_id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	defer rows.Close()
	result := make([]DueProviderOperation, 0, limit)
	for rows.Next() {
		var value DueProviderOperation
		var payload []byte
		if err := rows.Scan(&value.OrderID, &value.OperationType, &payload, &value.MerchantID, &value.ProviderLiveMode); err != nil {
			return nil, mapRepositoryError(err)
		}
		if len(payload) != len(value.PayloadSHA256) {
			return nil, ErrConflict
		}
		copy(value.PayloadSHA256[:], payload)
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, mapRepositoryError(err)
	}
	return result, nil
}


func scanProviderOperation(row rowScanner) (ProviderOperation, error) {
	var value ProviderOperation
	var payload []byte
	if err := row.Scan(
		&value.ID, &value.PaymentOrderID, &value.OperationType, &payload,
		&value.MerchantID, &value.ProviderLiveMode,
		&value.Status, &value.OwnerToken, &value.LeaseExpiresAt, &value.NextAttemptAt,
		&value.Attempts, &value.ProviderReference, &value.LastErrorClass,
		&value.CreatedAt, &value.UpdatedAt,
	); err != nil {
		return ProviderOperation{}, err
	}
	if len(payload) != len(value.PayloadSHA256) {
		return ProviderOperation{}, ErrConflict
	}
	copy(value.PayloadSHA256[:], payload)
	return value, nil
}

func dereferenceUUID(value *uuid.UUID) uuid.UUID {
	if value == nil {
		return uuid.Nil
	}
	return *value
}
