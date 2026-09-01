package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

const eventColumns = `event.id, event.payment_provider, event.provider_event_id,
	event.order_id, COALESCE(event.provider_trade_no, ''), COALESCE(event.provider_refund_no, ''),
	COALESCE(event.provider_payment_intent_no, ''), COALESCE(event.provider_charge_no, ''), COALESCE(event.provider_dispute_no, ''),
	event.event_type, event.amount_minor, COALESCE(btrim(event.currency), ''),
	COALESCE(event.merchant_id, ''), event.provider_live_mode, event.signature_verified,
	event.verification_source, event.occurred_at, event.received_at,
	processing.status, COALESCE(processing.last_error_class, ''), event.payload_sha256`

type persistedEvent struct {
	Event
	payloadHash []byte
}

func (r *Repository) ProcessVerifiedEvent(ctx context.Context, provider string, verified VerifiedEvent, payloadHash [sha256.Size]byte, requestID string, receivedAt time.Time) (WebhookResult, error) {
	return r.processAuthenticatedEvent(ctx, provider, verified, payloadHash, VerificationWebhook, true, requestID, receivedAt)
}

func (r *Repository) processAuthenticatedEvent(ctx context.Context, provider string, verified VerifiedEvent, payloadHash [sha256.Size]byte, verificationSource string, signatureVerified bool, requestID string, receivedAt time.Time) (WebhookResult, error) {
	if (verificationSource != VerificationWebhook && verificationSource != VerificationProvider) || signatureVerified != (verificationSource == VerificationWebhook) {
		return WebhookResult{}, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result WebhookResult
	var committedError error
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		existing, err := loadEventByProviderID(dbCtx, q, provider, verified.ProviderEventID)
		switch {
		case err == nil:
			if !sameAuthenticatedEvent(existing, verified, payloadHash, verificationSource, signatureVerified) {
				return ErrWebhookReplay
			}
			result = replayedWebhookResult(dbCtx, q, existing)
			committedError = replayedWebhookError(existing)
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}
		order, orderErr := loadOrderForVerifiedEvent(dbCtx, q, provider, verified, true)
		var orderID any
		if orderErr == nil {
			orderID = order.ID
		} else if !errors.Is(orderErr, pgx.ErrNoRows) {
			return mapRepositoryError(orderErr)
		}
		eventID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate payment event UUIDv7: %w", err)
		}
		command, err := q.Exec(dbCtx, `
			INSERT INTO payment_events (
				id, payment_provider, provider_event_id, order_id, provider_trade_no,
				provider_refund_no, provider_payment_intent_no, provider_charge_no, provider_dispute_no,
				event_type, amount_minor, currency, payload_sha256,
				signature_verified, verification_source, merchant_id, provider_live_mode,
				occurred_at, received_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (payment_provider, provider_event_id) DO NOTHING`,
			eventID, provider, verified.ProviderEventID, orderID, verified.ProviderTradeNo,
			nullableText(verified.ProviderRefundNo), nullableText(verified.ProviderPaymentIntentNo),
			nullableText(verified.ProviderChargeNo), nullableText(verified.ProviderDisputeNo),
			verified.EventType, verified.AmountMinor, verified.Currency, payloadHash[:],
			signatureVerified, verificationSource, verified.MerchantID, verified.LiveMode, verified.OccurredAt, receivedAt)
		if err != nil {
			return fmt.Errorf("insert verified payment event: %w", mapVerifiedEventInsertError(err))
		}
		if command.RowsAffected() == 0 {
			existing, err := loadEventByProviderID(dbCtx, q, provider, verified.ProviderEventID)
			if err != nil {
				return mapRepositoryError(err)
			}
			if !sameAuthenticatedEvent(existing, verified, payloadHash, verificationSource, signatureVerified) {
				return ErrWebhookReplay
			}
			result = replayedWebhookResult(dbCtx, q, existing)
			committedError = replayedWebhookError(existing)
			return nil
		}
		if _, err := q.Exec(dbCtx, `
			INSERT INTO payment_event_processing (
				payment_event_id, status, attempts, updated_at
			)
			VALUES ($1, 'pending', 1, $2)`, eventID, receivedAt); err != nil {
			return mapRepositoryError(err)
		}

		if orderErr != nil {
			committedError = ErrWebhookMismatch
			if err := rejectVerifiedEvent(dbCtx, q, eventID, provider, requestID, "order_not_found", receivedAt); err != nil {
				return err
			}
			result.Rejected = true
			value, err := loadEventByID(dbCtx, q, eventID)
			if err != nil {
				return mapRepositoryError(err)
			}
			result.Event = value.Event
			return nil
		}
		merchantMismatch := order.MerchantID != "" && order.MerchantID != verified.MerchantID
		modeMismatch := order.ProviderLiveMode != nil && *order.ProviderLiveMode != verified.LiveMode
		amountMismatch := order.AmountMinor != verified.AmountMinor
		if isDisputeEvent(verified.EventType) || (verified.EventType == EventRefunded && order.Status == StatusPaid && order.RefundHoldLedgerID == nil) {
			amountMismatch = verified.AmountMinor <= 0 || verified.AmountMinor > order.AmountMinor
		}
		if order.PaymentProvider != provider || amountMismatch || order.Currency != verified.Currency || merchantMismatch || modeMismatch || !eventReferencesOrder(order, verified) {
			committedError = ErrWebhookMismatch
			if err := rejectVerifiedEvent(dbCtx, q, eventID, provider, requestID, "order_mismatch", receivedAt); err != nil {
				return err
			}
			result.Rejected = true
			result.Order = &order
			value, err := loadEventByID(dbCtx, q, eventID)
			if err != nil {
				return mapRepositoryError(err)
			}
			result.Event = value.Event
			return nil
		}
		order, err = attachProviderIdentifiers(dbCtx, q, order, verified, receivedAt)
		if err != nil {
			return fmt.Errorf("attach payment provider identifiers: %w", err)
		}

		updated, processErr := r.applyVerifiedEvent(dbCtx, q, order, verified, receivedAt)
		if processErr == nil && !isDisputeEvent(verified.EventType) {
			processErr = completeProviderOperationFromEvent(dbCtx, q, order.ID, verified, receivedAt)
		}
		if processErr != nil {
			if !permanentPaymentEventError(processErr) {
				return processErr
			}
			committedError = processErr
			if err := rejectVerifiedEvent(dbCtx, q, eventID, provider, requestID, paymentErrorClass(processErr), receivedAt); err != nil {
				return err
			}
			result.Rejected = true
			result.Order = &order
		} else {
			if _, err := q.Exec(dbCtx, `
				UPDATE payment_event_processing
				SET status = 'processed', processed_at = $2,
					last_error_class = NULL, updated_at = $2
				WHERE payment_event_id = $1`, eventID, receivedAt); err != nil {
				return mapRepositoryError(err)
			}
			if err := audit.Insert(dbCtx, q, audit.Event{
				Action:     "payment.webhook." + verified.EventType,
				TargetType: "payment_order", TargetID: order.ID.String(),
				RequestID: requestID, Result: "success",
			}); err != nil {
				return err
			}
			result.Order = &updated
		}
		value, err := loadEventByID(dbCtx, q, eventID)
		if err != nil {
			return mapRepositoryError(err)
		}
		result.Event = value.Event
		return nil
	})
	if err != nil {
		return WebhookResult{}, err
	}
	if committedError != nil {
		return result, committedError
	}
	return result, nil
}
func loadOrderForVerifiedEvent(ctx context.Context, q data.Querier, provider string, event VerifiedEvent, forUpdate bool) (Order, error) {
	load := func(query string, value string) (Order, error) {
		if value == "" {
			return Order{}, pgx.ErrNoRows
		}
		if forUpdate {
			query += ` FOR UPDATE`
		}
		return scanOrder(q.QueryRow(ctx, query, provider, value))
	}
	if event.OrderNo != "" {
		query := `SELECT ` + orderColumns + ` FROM payment_orders WHERE order_no = $1`
		if forUpdate {
			query += ` FOR UPDATE`
		}
		if order, err := scanOrder(q.QueryRow(ctx, query, event.OrderNo)); err == nil {
			return order, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Order{}, err
		}
	}
	lookups := []struct {
		query string
		value string
	}{
		{`SELECT ` + orderColumns + ` FROM payment_orders WHERE payment_provider = $1 AND provider_payment_intent_no = $2`, event.ProviderPaymentIntentNo},
		{`SELECT ` + orderColumns + ` FROM payment_orders WHERE payment_provider = $1 AND provider_charge_no = $2`, event.ProviderChargeNo},
		{`SELECT ` + orderColumns + ` FROM payment_orders WHERE payment_provider = $1 AND provider_trade_no = $2`, event.ProviderTradeNo},
	}
	for _, lookup := range lookups {
		order, err := load(lookup.query, lookup.value)
		if err == nil {
			return order, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Order{}, err
		}
	}
	return Order{}, pgx.ErrNoRows
}

func eventReferencesOrder(order Order, event VerifiedEvent) bool {
	tradeUsesProviderObject := event.ProviderTradeNo != "" &&
		(event.ProviderTradeNo == event.ProviderPaymentIntentNo || event.ProviderTradeNo == event.ProviderChargeNo)
	return (event.OrderNo == "" || event.OrderNo == order.OrderNo) &&
		(order.ProviderTradeNo == "" || event.ProviderTradeNo == "" || tradeUsesProviderObject || event.ProviderTradeNo == order.ProviderTradeNo) &&
		(order.ProviderRefundNo == "" || event.ProviderRefundNo == "" || event.ProviderRefundNo == order.ProviderRefundNo) &&
		(order.ProviderPaymentIntentNo == "" || event.ProviderPaymentIntentNo == "" || event.ProviderPaymentIntentNo == order.ProviderPaymentIntentNo) &&
		(order.ProviderChargeNo == "" || event.ProviderChargeNo == "" || event.ProviderChargeNo == order.ProviderChargeNo)
}

func attachProviderIdentifiers(ctx context.Context, q data.Querier, order Order, event VerifiedEvent, now time.Time) (Order, error) {
	if (order.ProviderPaymentIntentNo != "" && event.ProviderPaymentIntentNo != "" && order.ProviderPaymentIntentNo != event.ProviderPaymentIntentNo) ||
		(order.ProviderChargeNo != "" && event.ProviderChargeNo != "" && order.ProviderChargeNo != event.ProviderChargeNo) {
		return Order{}, ErrWebhookMismatch
	}
	if event.ProviderPaymentIntentNo == "" && event.ProviderChargeNo == "" {
		return order, nil
	}
	value, err := scanOrder(q.QueryRow(ctx, `
		UPDATE payment_orders
		SET provider_payment_intent_no = COALESCE(provider_payment_intent_no, NULLIF($2, '')),
			provider_charge_no = COALESCE(provider_charge_no, NULLIF($3, '')),
			updated_at = $4
		WHERE id = $1
		RETURNING `+orderColumns, order.ID, event.ProviderPaymentIntentNo, event.ProviderChargeNo, now))
	if err != nil {
		if errors.Is(mapRepositoryError(err), ErrConflict) {
			return Order{}, ErrWebhookMismatch
		}
		return Order{}, mapRepositoryError(err)
	}
	return value, nil
}

func isDisputeEvent(eventType string) bool {
	return eventType == EventDisputeOpened || eventType == EventDisputeWon || eventType == EventDisputeLost
}

func (r *Repository) ProcessProviderState(ctx context.Context, provider string, order Order, state ProviderState, receivedAt time.Time) (WebhookResult, error) {
	if r == nil || order.ID == uuid.Nil || provider == "" || state.MerchantID == "" || state.ProviderTradeNo == "" || !validEventType(state.EventType) || state.EventType == EventPending || state.AmountMinor <= 0 || state.Currency == "" || state.OccurredAt.IsZero() {
		return WebhookResult{}, ErrInvalidInput
	}
	canonical := fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s",
		provider, order.OrderNo, state.MerchantID, state.LiveMode, state.ProviderTradeNo,
		state.ProviderRefundNo, state.EventType, state.AmountMinor, state.Currency,
		state.OccurredAt.UTC().Format(time.RFC3339Nano))
	payloadHash := sha256.Sum256([]byte(canonical))
	verified := VerifiedEvent{
		ProviderEventID:  "reconcile_" + hex.EncodeToString(payloadHash[:]),
		MerchantID:       state.MerchantID,
		LiveMode:         state.LiveMode,
		OrderNo:          order.OrderNo,
		ProviderTradeNo:  state.ProviderTradeNo,
		ProviderRefundNo: state.ProviderRefundNo,
		EventType:        state.EventType,
		AmountMinor:      state.AmountMinor,
		Currency:         state.Currency,
		OccurredAt:       state.OccurredAt.UTC(),
	}
	return r.processAuthenticatedEvent(ctx, provider, verified, payloadHash, VerificationProvider, false, "payment-reconcile:"+order.ID.String(), receivedAt.UTC())
}

func (r *Repository) applyVerifiedEvent(ctx context.Context, q data.Querier, order Order, event VerifiedEvent, now time.Time) (Order, error) {
	switch event.EventType {
	case EventPending:
		if event.ProviderRefundNo != "" {
			if order.ProviderRefundNo != "" || order.Status != StatusRefundPending {
				return order, nil
			}
			value, err := scanOrder(q.QueryRow(ctx, `
				UPDATE payment_orders
				SET provider_refund_no = $2, updated_at = $3
				WHERE id = $1
				RETURNING `+orderColumns, order.ID, event.ProviderRefundNo, now))
			if err != nil {
				return Order{}, mapRepositoryError(err)
			}
			return value, nil
		}
		if order.Status == StatusPaid {
			return order, nil
		}
		if order.Status != StatusCreated && order.Status != StatusPending {
			return Order{}, ErrInvalidTransition
		}
		value, err := scanOrder(q.QueryRow(ctx, `
			UPDATE payment_orders
			SET status = 'pending', provider_trade_no = COALESCE(provider_trade_no, $2),
				merchant_id = COALESCE(merchant_id, $3), provider_live_mode = COALESCE(provider_live_mode, $4),
				failure_class = NULL, updated_at = $5
			WHERE id = $1
			RETURNING `+orderColumns, order.ID, event.ProviderTradeNo, event.MerchantID, event.LiveMode, now))
		if err != nil {
			return Order{}, mapRepositoryError(err)
		}
		return value, nil
	case EventPaid:
		if order.Status == StatusPaid {
			return order, nil
		}
		if order.Status != StatusCreated && order.Status != StatusPending {
			return Order{}, ErrInvalidTransition
		}
		if event.OccurredAt.Before(order.CreatedAt) {
			return Order{}, ErrWebhookMismatch
		}
		ledger, err := r.billing.CreditInTx(ctx, q, billing.CreditInput{
			UserID: order.UserID, Currency: order.Currency, EntryType: billing.EntryRecharge,
			AmountMinor: order.AmountMinor, ReferenceType: "payment_order",
			ReferenceID: order.ID.String(), IdempotencyKey: "payment:recharge:v1:" + order.ID.String(),
			Source: "payment",
		})
		if err != nil {
			return Order{}, err
		}
		value, err := scanOrder(q.QueryRow(ctx, `
			UPDATE payment_orders
			SET status = 'paid', provider_trade_no = COALESCE(provider_trade_no, $2),
				merchant_id = COALESCE(merchant_id, $3), provider_live_mode = COALESCE(provider_live_mode, $4),
				wallet_id = $5, recharge_ledger_id = $6, paid_at = $7,
				failure_class = NULL, updated_at = $8
			WHERE id = $1
			RETURNING `+orderColumns,
			order.ID, event.ProviderTradeNo, event.MerchantID, event.LiveMode,
			ledger.WalletID, ledger.ID, event.OccurredAt, now,
		))
		if err != nil {
			return Order{}, mapRepositoryError(err)
		}
		if err := insertPaymentOutbox(ctx, q, "payment_order", order.ID, "payment.order.paid", "payment:paid:v1:"+order.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo, "wallet_id": ledger.WalletID,
			"ledger_id": ledger.ID, "amount_minor": order.AmountMinor, "currency": order.Currency,
		}); err != nil {
			return Order{}, err
		}
		return value, nil
	case EventFailed:
		return transitionProviderTerminal(ctx, q, order, StatusFailed, "provider_failed", event, now)
	case EventExpired:
		return transitionProviderTerminal(ctx, q, order, StatusExpired, "", event, now)
	case EventClosed:
		return transitionProviderTerminal(ctx, q, order, StatusClosed, "", event, now)
	case EventRefunded:
		if order.Status == StatusPaid && order.RefundHoldLedgerID == nil {
			return r.applyExternalRefund(ctx, q, order, event, now)
		}
		if order.Status == StatusRefunded {
			return order, nil
		}
		refundNo := order.ProviderRefundNo
		if refundNo == "" {
			refundNo = event.ProviderRefundNo
		}
		if order.Status != StatusRefundPending || order.UpdatedBy == nil || refundNo == "" {
			return Order{}, ErrInvalidTransition
		}
		ledger, err := r.billing.CompletePaymentRefundInTx(ctx, q, billing.PaymentRefundInput{
			UserID: order.UserID, Currency: order.Currency, AmountMinor: order.AmountMinor,
			PaymentOrderID: order.ID, OperatorUserID: *order.UpdatedBy, Source: "payment",
		})
		if err != nil {
			return Order{}, err
		}
		value, err := scanOrder(q.QueryRow(ctx, `
			UPDATE payment_orders
			SET status = 'refunded', provider_refund_no = $2, refund_ledger_id = $3,
				merchant_id = COALESCE(merchant_id, $4), provider_live_mode = COALESCE(provider_live_mode, $5),
				refunded_at = $6, failure_class = NULL, updated_at = $7
			WHERE id = $1
			RETURNING `+orderColumns, order.ID, refundNo, ledger.ID, event.MerchantID, event.LiveMode, event.OccurredAt, now))
		if err != nil {
			return Order{}, mapRepositoryError(err)
		}
		if err := insertPaymentOutbox(ctx, q, "payment_order", order.ID, "payment.order.refunded", "payment:refunded:v1:"+order.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo, "wallet_id": ledger.WalletID,
			"ledger_id": ledger.ID, "amount_minor": order.AmountMinor, "currency": order.Currency,
		}); err != nil {
			return Order{}, err
		}
		return value, nil
	case EventRefundFailed:
		if order.Status == StatusPaid && order.ProviderRefundNo == event.ProviderRefundNo && order.FailureClass == "provider_refund_failed" {
			return order, nil
		}
		refundNo := order.ProviderRefundNo
		if refundNo == "" {
			refundNo = event.ProviderRefundNo
		}
		if order.Status != StatusRefundPending || order.UpdatedBy == nil || refundNo == "" {
			return Order{}, ErrInvalidTransition
		}
		if _, err := r.billing.ReleasePaymentRefundInTx(ctx, q, billing.PaymentRefundInput{
			UserID: order.UserID, Currency: order.Currency, AmountMinor: order.AmountMinor,
			PaymentOrderID: order.ID, OperatorUserID: *order.UpdatedBy, Source: "payment",
		}); err != nil {
			return Order{}, err
		}
		value, err := scanOrder(q.QueryRow(ctx, `
			UPDATE payment_orders
			SET status = 'paid', provider_refund_no = $2,
				merchant_id = COALESCE(merchant_id, $3), provider_live_mode = COALESCE(provider_live_mode, $4),
				failure_class = 'provider_refund_failed', updated_at = $5
			WHERE id = $1
			RETURNING `+orderColumns, order.ID, refundNo, event.MerchantID, event.LiveMode, now))
		if err != nil {
			return Order{}, mapRepositoryError(err)
		}
		if err := insertPaymentOutbox(ctx, q, "payment_order", order.ID, "payment.order.refund_failed", "payment:refund-failed:v1:"+order.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo,
			"amount_minor": order.AmountMinor, "currency": order.Currency,
		}); err != nil {
			return Order{}, err
		}
		return value, nil
	case EventDisputeOpened, EventDisputeWon, EventDisputeLost:
		return r.applyDisputeEvent(ctx, q, order, event, now)
	default:
		return Order{}, ErrWebhookMismatch
	}
}

type paymentDispute struct {
	ID                uuid.UUID
	OrderID           uuid.UUID
	LiabilityID       uuid.UUID
	Provider          string
	ProviderDisputeNo string
	ProviderChargeNo  string
	PaymentIntentNo   string
	AmountMinor       int64
	Currency          string
	Status            string
	OpenedAt          time.Time
}

func (r *Repository) applyExternalRefund(ctx context.Context, q data.Querier, order Order, event VerifiedEvent, now time.Time) (Order, error) {
	if order.WalletID == nil || event.ProviderRefundNo == "" || event.OccurredAt.Before(order.CreatedAt) {
		return Order{}, ErrWebhookMismatch
	}
	var existingOrderID uuid.UUID
	var existingAmount int64
	var existingCurrency string
	err := q.QueryRow(ctx, `
		SELECT payment_order_id, amount_minor, btrim(currency)
		FROM payment_external_refunds
		WHERE payment_provider = $1 AND provider_refund_no = $2`,
		order.PaymentProvider, event.ProviderRefundNo,
	).Scan(&existingOrderID, &existingAmount, &existingCurrency)
	if err == nil {
		if existingOrderID != order.ID || existingAmount != event.AmountMinor || existingCurrency != event.Currency {
			return Order{}, ErrWebhookMismatch
		}
		return order, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, mapRepositoryError(err)
	}
	if event.AmountMinor > order.AmountMinor || order.ExternalRefundedAmountMinor > order.AmountMinor-event.AmountMinor {
		return Order{}, ErrWebhookMismatch
	}
	total := order.ExternalRefundedAmountMinor + event.AmountMinor
	liability, err := r.billing.OpenLiabilityInTx(ctx, q, billing.LiabilityInput{
		UserID: order.UserID, Currency: order.Currency, LiabilityType: "payment_refund",
		ReferenceType: "payment_refund", ReferenceID: order.PaymentProvider + ":" + event.ProviderRefundNo,
		AmountMinor: event.AmountMinor,
	})
	if err != nil {
		return Order{}, err
	}
	refundID, err := id.New()
	if err != nil {
		return Order{}, fmt.Errorf("generate external refund UUIDv7: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO payment_external_refunds (
			id, payment_provider, provider_refund_no, payment_order_id,
			wallet_liability_id, provider_charge_no, provider_payment_intent_no,
			amount_minor, currency, occurred_at, created_at
		)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10, $11)`,
		refundID, order.PaymentProvider, event.ProviderRefundNo, order.ID,
		liability.ID, event.ProviderChargeNo, event.ProviderPaymentIntentNo,
		event.AmountMinor, event.Currency, event.OccurredAt, now); err != nil {
		return Order{}, mapRepositoryError(err)
	}
	targetStatus := order.Status
	var refundedAt any
	if total == order.AmountMinor {
		targetStatus = StatusRefunded
		refundedAt = event.OccurredAt
	}
	value, err := scanOrder(q.QueryRow(ctx, `
		UPDATE payment_orders
		SET status = $2, external_refunded_amount_minor = $3,
			refunded_at = COALESCE(refunded_at, $4), failure_class = NULL, updated_at = $5
		WHERE id = $1
		RETURNING `+orderColumns,
		order.ID, targetStatus, total, refundedAt, now,
	))
	if err != nil {
		return Order{}, mapRepositoryError(err)
	}
	if err := insertPaymentOutbox(ctx, q, "payment_order", order.ID, "payment.order.externally_refunded", "payment:external-refund:v1:"+refundID.String(), map[string]any{
		"order_id": order.ID, "order_no": order.OrderNo, "wallet_id": order.WalletID,
		"external_refund_id": refundID, "provider_refund_no": event.ProviderRefundNo,
		"liability_id": liability.ID, "amount_minor": event.AmountMinor,
		"external_refunded_amount_minor": total,
		"recovered_amount_minor":         liability.RecoveredAmountMinor, "currency": order.Currency,
	}); err != nil {
		return Order{}, err
	}
	return value, nil
}

func (r *Repository) applyDisputeEvent(ctx context.Context, q data.Querier, order Order, event VerifiedEvent, now time.Time) (Order, error) {
	if order.WalletID == nil || event.ProviderDisputeNo == "" || event.ProviderChargeNo == "" || event.OccurredAt.Before(order.CreatedAt) {
		return Order{}, ErrWebhookMismatch
	}
	dispute, err := loadPaymentDispute(ctx, q, order.PaymentProvider, event.ProviderDisputeNo, true)
	if errors.Is(err, pgx.ErrNoRows) {
		liability, openErr := r.billing.OpenLiabilityInTx(ctx, q, billing.LiabilityInput{
			UserID: order.UserID, Currency: order.Currency, LiabilityType: "payment_dispute",
			ReferenceType: "payment_dispute", ReferenceID: order.PaymentProvider + ":" + event.ProviderDisputeNo,
			AmountMinor: event.AmountMinor,
		})
		if openErr != nil {
			return Order{}, fmt.Errorf("open dispute liability: %w", openErr)
		}
		disputeID, idErr := id.New()
		if idErr != nil {
			return Order{}, fmt.Errorf("generate payment dispute UUIDv7: %w", idErr)
		}
		dispute = paymentDispute{
			ID: disputeID, OrderID: order.ID, LiabilityID: liability.ID,
			Provider: order.PaymentProvider, ProviderDisputeNo: event.ProviderDisputeNo,
			ProviderChargeNo: event.ProviderChargeNo, PaymentIntentNo: event.ProviderPaymentIntentNo,
			AmountMinor: event.AmountMinor, Currency: event.Currency, Status: "open", OpenedAt: event.OccurredAt,
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO payment_disputes (
				id, payment_provider, provider_dispute_no, payment_order_id,
				wallet_liability_id, provider_charge_no, provider_payment_intent_no,
				amount_minor, currency, status, opened_at, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, 'open', $10, $11, $11)`,
			dispute.ID, dispute.Provider, dispute.ProviderDisputeNo, dispute.OrderID,
			dispute.LiabilityID, dispute.ProviderChargeNo, dispute.PaymentIntentNo,
			dispute.AmountMinor, dispute.Currency, dispute.OpenedAt, now); err != nil {
			return Order{}, fmt.Errorf("insert payment dispute: %w", mapRepositoryError(err))
		}
	} else if err != nil {
		return Order{}, mapRepositoryError(err)
	} else if dispute.OrderID != order.ID || dispute.ProviderChargeNo != event.ProviderChargeNo ||
		dispute.AmountMinor != event.AmountMinor || dispute.Currency != event.Currency {
		return Order{}, ErrWebhookMismatch
	}

	target := "open"
	switch event.EventType {
	case EventDisputeWon:
		target = "won"
	case EventDisputeLost:
		target = "lost"
	}
	if dispute.Status == target || (target == "open" && dispute.Status != "open") {
		return order, nil
	}
	if dispute.Status != "open" {
		return Order{}, ErrWebhookMismatch
	}
	if target == "won" {
		if _, err := r.billing.ReverseLiabilityInTx(ctx, q, dispute.LiabilityID); err != nil {
			return Order{}, err
		}
	}
	if target != "open" {
		if _, err := q.Exec(ctx, `
			UPDATE payment_disputes
			SET status = $2, resolved_at = $3, updated_at = $4
			WHERE id = $1`, dispute.ID, target, event.OccurredAt, now); err != nil {
			return Order{}, mapRepositoryError(err)
		}
	}
	if err := insertPaymentOutbox(ctx, q, "payment_order", order.ID, "payment.dispute."+target,
		"payment:dispute:"+target+":v1:"+dispute.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo, "dispute_id": dispute.ID,
			"provider_dispute_no": dispute.ProviderDisputeNo, "liability_id": dispute.LiabilityID,
			"amount_minor": dispute.AmountMinor, "currency": dispute.Currency, "status": target,
		}); err != nil {
		return Order{}, err
	}
	return order, nil
}

func loadPaymentDispute(ctx context.Context, q data.Querier, provider, disputeNo string, forUpdate bool) (paymentDispute, error) {
	query := `
		SELECT id, payment_order_id, wallet_liability_id, payment_provider,
			provider_dispute_no, provider_charge_no, COALESCE(provider_payment_intent_no, ''),
			amount_minor, btrim(currency), status, opened_at
		FROM payment_disputes
		WHERE payment_provider = $1 AND provider_dispute_no = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var value paymentDispute
	err := q.QueryRow(ctx, query, provider, disputeNo).Scan(
		&value.ID, &value.OrderID, &value.LiabilityID, &value.Provider,
		&value.ProviderDisputeNo, &value.ProviderChargeNo, &value.PaymentIntentNo,
		&value.AmountMinor, &value.Currency, &value.Status, &value.OpenedAt,
	)
	return value, err
}

func transitionProviderTerminal(ctx context.Context, q data.Querier, order Order, target, failureClass string, event VerifiedEvent, now time.Time) (Order, error) {
	if order.Status == target {
		return order, nil
	}
	allowed := order.Status == StatusCreated || order.Status == StatusPending
	if target == StatusClosed {
		allowed = allowed || order.Status == StatusFailed || order.Status == StatusExpired
	}
	if !allowed {
		return Order{}, ErrInvalidTransition
	}
	closedAt := any(nil)
	if target == StatusClosed {
		closedAt = event.OccurredAt
	}
	value, err := scanOrder(q.QueryRow(ctx, `
		UPDATE payment_orders
		SET status = $2, provider_trade_no = COALESCE(provider_trade_no, $3),
			merchant_id = COALESCE(merchant_id, $4), provider_live_mode = COALESCE(provider_live_mode, $5),
			failure_class = $6, closed_at = COALESCE(closed_at, $7), updated_at = $8
		WHERE id = $1
		RETURNING `+orderColumns,
		order.ID, target, event.ProviderTradeNo, event.MerchantID, event.LiveMode,
		nullableText(failureClass), closedAt, now,
	))
	if err != nil {
		return Order{}, mapRepositoryError(err)
	}
	if err := insertPaymentOutbox(ctx, q, "payment_order", order.ID, "payment.order."+target, "payment:"+target+":v1:"+order.ID.String(), map[string]any{
		"order_id": order.ID, "order_no": order.OrderNo, "status": target,
	}); err != nil {
		return Order{}, err
	}
	return value, nil
}

func completeProviderOperationFromEvent(ctx context.Context, q data.Querier, orderID uuid.UUID, event VerifiedEvent, now time.Time) error {
	operationType := OperationCreate
	reference := event.ProviderTradeNo
	if event.ProviderRefundNo != "" {
		operationType = OperationRefund
		reference = event.ProviderRefundNo
	}
	command, err := q.Exec(ctx, `
		UPDATE payment_provider_operations
		SET status = 'succeeded', owner_token = NULL, lease_expires_at = NULL,
			provider_reference = $3, next_attempt_at = $4,
			last_error_class = NULL, updated_at = $4
		WHERE payment_order_id = $1 AND operation_type = $2
		  AND status <> 'definitive_failed'`, orderID, operationType, reference, now)
	if err != nil {
		return mapRepositoryError(err)
	}
	if command.RowsAffected() > 0 {
		return nil
	}
	var status string
	err = q.QueryRow(ctx, `
		SELECT status FROM payment_provider_operations
		WHERE payment_order_id = $1 AND operation_type = $2`, orderID, operationType).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapRepositoryError(err)
	}
	if status == OperationDefinitiveFailed {
		return ErrConflict
	}
	return nil
}

func permanentPaymentEventError(err error) bool {
	return errors.Is(err, ErrWebhookMismatch) || errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrConflict)
}

func rejectVerifiedEvent(ctx context.Context, q data.Querier, eventID uuid.UUID, provider, requestID, errorClass string, now time.Time) error {
	if _, err := q.Exec(ctx, `
		UPDATE payment_event_processing
		SET status = 'rejected', processed_at = $2,
			last_error_class = $3, updated_at = $2
		WHERE payment_event_id = $1`, eventID, now, errorClass); err != nil {
		return mapRepositoryError(err)
	}
	if err := audit.Insert(ctx, q, audit.Event{
		Action: "payment.webhook.rejected", TargetType: "payment_provider",
		TargetID: provider, RequestID: requestID, Result: "denied",
	}); err != nil {
		return err
	}
	return nil
}

func loadEventByProviderID(ctx context.Context, q data.Querier, provider, eventID string) (persistedEvent, error) {
	return scanEvent(q.QueryRow(ctx, `
		SELECT `+eventColumns+`
		FROM payment_events event
		JOIN payment_event_processing processing ON processing.payment_event_id = event.id
		WHERE event.payment_provider = $1 AND event.provider_event_id = $2`, provider, eventID))
}

func loadEventByID(ctx context.Context, q data.Querier, eventID uuid.UUID) (persistedEvent, error) {
	return scanEvent(q.QueryRow(ctx, `
		SELECT `+eventColumns+`
		FROM payment_events event
		JOIN payment_event_processing processing ON processing.payment_event_id = event.id
		WHERE event.id = $1`, eventID))
}

func replayedWebhookResult(ctx context.Context, q data.Querier, existing persistedEvent) WebhookResult {
	result := WebhookResult{Event: existing.Event, Replayed: true}
	if existing.OrderID != nil {
		if order, err := loadOrderByID(ctx, q, *existing.OrderID, false); err == nil {
			result.Order = &order
		}
	}
	result.Rejected = replayedWebhookError(existing) != nil
	return result
}

func replayedWebhookError(existing persistedEvent) error {
	if existing.ProcessingStatus == ProcessingProcessed {
		return nil
	}
	switch existing.ErrorClass {
	case "order_not_found", "order_mismatch":
		return ErrWebhookMismatch
	case "invalid_transition":
		return ErrInvalidTransition
	case "insufficient_funds":
		return ErrInsufficientFunds
	default:
		return ErrConflict
	}
}

func mapVerifiedEventInsertError(err error) error {
	mapped := mapRepositoryError(err)
	if errors.Is(mapped, ErrConflict) {
		return ErrWebhookReplay
	}
	return mapped
}

func scanEvent(row rowScanner) (persistedEvent, error) {
	var value persistedEvent
	if err := row.Scan(
		&value.ID, &value.PaymentProvider, &value.ProviderEventID,
		&value.OrderID, &value.ProviderTradeNo, &value.ProviderRefundNo,
		&value.ProviderPaymentIntentNo, &value.ProviderChargeNo, &value.ProviderDisputeNo,
		&value.EventType, &value.AmountMinor, &value.Currency, &value.MerchantID, &value.ProviderLiveMode,
		&value.SignatureVerified, &value.VerificationSource, &value.OccurredAt, &value.ReceivedAt,
		&value.ProcessingStatus, &value.ErrorClass, &value.payloadHash,
	); err != nil {
		return persistedEvent{}, err
	}
	return value, nil
}

func sameAuthenticatedEvent(existing persistedEvent, value VerifiedEvent, hash [sha256.Size]byte, verificationSource string, signatureVerified bool) bool {
	return bytes.Equal(existing.payloadHash, hash[:]) &&
		existing.SignatureVerified == signatureVerified && existing.VerificationSource == verificationSource &&
		existing.ProviderTradeNo == value.ProviderTradeNo &&
		existing.ProviderPaymentIntentNo == value.ProviderPaymentIntentNo &&
		existing.ProviderChargeNo == value.ProviderChargeNo &&
		existing.ProviderDisputeNo == value.ProviderDisputeNo &&
		existing.EventType == value.EventType &&
		existing.ProviderRefundNo == value.ProviderRefundNo &&
		existing.AmountMinor != nil && *existing.AmountMinor == value.AmountMinor &&
		existing.Currency == value.Currency &&
		existing.MerchantID == value.MerchantID &&
		existing.ProviderLiveMode != nil && *existing.ProviderLiveMode == value.LiveMode &&
		existing.OccurredAt != nil && existing.OccurredAt.Equal(value.OccurredAt)
}

func paymentErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrInvalidTransition):
		return "invalid_transition"
	case errors.Is(err, ErrWebhookMismatch):
		return "order_mismatch"
	case errors.Is(err, billing.ErrInsufficientFunds):
		return "insufficient_funds"
	default:
		return "processing_failed"
	}
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
