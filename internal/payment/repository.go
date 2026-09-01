package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
	"github.com/starhui-dev/bablo/internal/secret"
)

const (
	paymentDurableTimeout             = 30 * time.Second
	maximumActivePaymentOrdersPerUser = 5
)

const orderColumns = `id, order_no, user_id, amount_minor, btrim(currency),
	payment_provider, COALESCE(merchant_id, ''), provider_live_mode, COALESCE(provider_trade_no, ''), COALESCE(provider_refund_no, ''),
	COALESCE(provider_payment_intent_no, ''), COALESCE(provider_charge_no, ''),
	status, idempotency_key, checkout_data, COALESCE(failure_class, ''),
	wallet_id, recharge_ledger_id, refund_hold_ledger_id, refund_ledger_id, external_refunded_amount_minor, updated_by,
	expires_at, paid_at, refunded_at, closed_at, created_at, updated_at`

// Repository owns payment state transitions and their PostgreSQL transactions.
type Repository struct {
	store       *data.Store
	billing     *billing.Service
	voucherKeys *secret.Keyring
}

func NewRepository(store *data.Store, billingService *billing.Service, voucherKeys ...*secret.Keyring) (*Repository, error) {
	if store == nil || store.Queryer() == nil || billingService == nil || len(voucherKeys) > 1 {
		return nil, errors.New("payment repository requires data store and billing service")
	}
	var keys *secret.Keyring
	if len(voucherKeys) == 1 {
		keys = voucherKeys[0]
	}
	return &Repository{store: store, billing: billingService, voucherKeys: keys}, nil
}

func (r *Repository) CreateOrder(ctx context.Context, input CreateOrderInput, identity ProviderIdentity, expiresAt, now time.Time) (Order, error) {
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		existing, err := loadOrderByIdempotency(dbCtx, q, input.IdempotencyKey, true)
		switch {
		case err == nil:
			if existing.UserID != input.UserID || existing.AmountMinor != input.AmountMinor || existing.Currency != input.Currency || existing.PaymentProvider != input.PaymentProvider {
				return ErrConflict
			}
			if existing.Status == StatusCreated {
				if err := ensureProviderOperation(dbCtx, q, existing, OperationCreate, identity, now); err != nil {
					return err
				}
			}
			result = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return mapRepositoryError(err)
		}
		var lockedUserID uuid.UUID
		if err := q.QueryRow(dbCtx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, input.UserID).Scan(&lockedUserID); err != nil {
			return mapRepositoryError(err)
		}
		var activeOrders int
		if err := q.QueryRow(dbCtx, `
			SELECT count(*) FROM payment_orders
			WHERE user_id = $1 AND status IN ('created', 'pending')`, input.UserID).Scan(&activeOrders); err != nil {
			return mapRepositoryError(err)
		}
		if activeOrders >= maximumActivePaymentOrdersPerUser {
			return ErrOrderLimit
		}
		orderID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate payment order UUIDv7: %w", err)
		}
		orderNo := "bablo_pay_" + strings.ReplaceAll(orderID.String(), "-", "")
		value, err := scanOrder(q.QueryRow(dbCtx, `
			INSERT INTO payment_orders (
				id, order_no, user_id, amount_minor, currency, payment_provider,
				status, idempotency_key, expires_at, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, 'created', $7, $8, $9, $9)
			RETURNING `+orderColumns,
			orderID, orderNo, input.UserID, input.AmountMinor, input.Currency,
			input.PaymentProvider, input.IdempotencyKey, expiresAt, now,
		))
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_order", value.ID, "payment.order.created", "payment:create:v1:"+value.ID.String(), map[string]any{
			"order_id": value.ID, "order_no": value.OrderNo, "user_id": value.UserID,
			"amount_minor": value.AmountMinor, "currency": value.Currency,
			"payment_provider": value.PaymentProvider,
		}); err != nil {
			return err
		}
		if err := ensureProviderOperation(dbCtx, q, value, OperationCreate, identity, now); err != nil {
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

func (r *Repository) GetOrderForUser(ctx context.Context, userID uuid.UUID, orderNo string) (Order, error) {
	value, err := scanOrder(r.store.Queryer().QueryRow(ctx, `SELECT `+orderColumns+` FROM payment_orders WHERE user_id = $1 AND order_no = $2`, userID, orderNo))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, mapRepositoryError(err)
	}
	return value, nil
}

func (r *Repository) GetOrder(ctx context.Context, orderNo string) (Order, error) {
	value, err := scanOrder(r.store.Queryer().QueryRow(ctx, `SELECT `+orderColumns+` FROM payment_orders WHERE order_no = $1`, orderNo))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, mapRepositoryError(err)
	}
	return value, nil
}

func (r *Repository) ListOrders(ctx context.Context, userID uuid.UUID, cursor *PageCursor, limit int) (OrderPage, error) {
	var cursorTime any
	var cursorID any
	if cursor != nil {
		cursorTime = cursor.CreatedAt
		cursorID = cursor.ID
	}
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT `+orderColumns+`
		FROM payment_orders
		WHERE user_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3::uuid))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, userID, cursorTime, cursorID, limit+1)
	if err != nil {
		return OrderPage{}, mapRepositoryError(err)
	}
	defer rows.Close()
	values := make([]Order, 0, limit+1)
	for rows.Next() {
		value, err := scanOrder(rows)
		if err != nil {
			return OrderPage{}, mapRepositoryError(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return OrderPage{}, mapRepositoryError(err)
	}
	page := OrderPage{Orders: values}
	if len(values) > limit {
		last := values[limit-1]
		page.Orders = values[:limit]
		page.NextCursor = &PageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *Repository) PrepareRefund(ctx context.Context, input RefundInput, identity ProviderIdentity, now time.Time) (Order, error) {
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByNo(dbCtx, q, input.OrderNo, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if order.MerchantID != identity.MerchantID || order.ProviderLiveMode == nil || *order.ProviderLiveMode != identity.LiveMode {
			return ErrProviderUnavailable
		}
		if order.Status == StatusRefundPending || order.Status == StatusRefunded {
			if order.Status == StatusRefundPending {
				if err := ensureProviderOperation(dbCtx, q, order, OperationRefund, identity, now); err != nil {
					return err
				}
			}
			result = order
			return nil
		}
		if order.ProviderRefundNo != "" || order.FailureClass != "" {
			return ErrInvalidTransition
		}
		if order.Status != StatusPaid || order.WalletID == nil || order.RechargeLedgerID == nil {
			return ErrInvalidTransition
		}
		hold, err := r.billing.HoldPaymentRefundInTx(dbCtx, q, billing.PaymentRefundInput{
			UserID: order.UserID, Currency: order.Currency, AmountMinor: order.AmountMinor,
			PaymentOrderID: order.ID, OperatorUserID: input.OperatorUserID, Source: "payment",
		})
		if err != nil {
			if errors.Is(err, billing.ErrInsufficientFunds) {
				return ErrInsufficientFunds
			}
			return err
		}
		value, err := scanOrder(q.QueryRow(dbCtx, `
			UPDATE payment_orders
			SET status = 'refund_pending', updated_by = $2,
				refund_hold_ledger_id = $3, failure_class = NULL, updated_at = $4
			WHERE id = $1
			RETURNING `+orderColumns,
			order.ID, input.OperatorUserID, hold.ID, now,
		))
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(dbCtx, q, audit.Event{
			ActorUserID: &input.OperatorUserID, Action: "payment.refund.requested",
			TargetType: "payment_order", TargetID: order.ID.String(),
			RequestID: input.RequestID, Result: "success",
		}); err != nil {
			return err
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_order", order.ID, "payment.refund.requested", "payment:refund-request:v1:"+order.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo, "wallet_id": hold.WalletID,
			"ledger_id": hold.ID, "amount_minor": order.AmountMinor, "currency": order.Currency,
		}); err != nil {
			return err
		}
		if err := ensureProviderOperation(dbCtx, q, value, OperationRefund, identity, now); err != nil {
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

func (r *Repository) MarkClosed(ctx context.Context, input CloseInput, now time.Time) (Order, error) {
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	var result Order
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByNo(dbCtx, q, input.OrderNo, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if order.Status == StatusClosed {
			result = order
			return nil
		}
		if order.Status != StatusCreated && order.Status != StatusPending && order.Status != StatusFailed && order.Status != StatusExpired {
			return ErrInvalidTransition
		}
		value, err := scanOrder(q.QueryRow(dbCtx, `
			UPDATE payment_orders
			SET status = 'closed', closed_at = $2, updated_by = $3, updated_at = $2
			WHERE id = $1
			RETURNING `+orderColumns, order.ID, now, input.OperatorUserID))
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(dbCtx, q, audit.Event{
			ActorUserID: &input.OperatorUserID, Action: "payment.order.closed",
			TargetType: "payment_order", TargetID: order.ID.String(),
			RequestID: input.RequestID, Result: "success",
		}); err != nil {
			return err
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_order", order.ID, "payment.order.closed", "payment:close:v1:"+order.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo,
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

func (r *Repository) ListDueOrders(ctx context.Context, limit int, now time.Time) ([]Order, error) {
	if r == nil || limit < 1 || limit > 1000 {
		return nil, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	rows, err := r.store.Queryer().Query(dbCtx, `
		SELECT `+orderColumns+`
		FROM payment_orders
		WHERE status IN ('created', 'pending') AND expires_at <= $1
		ORDER BY updated_at, expires_at, id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	defer rows.Close()
	orders := make([]Order, 0, limit)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, mapRepositoryError(err)
	}
	return orders, nil
}

func (r *Repository) TouchMaintenance(ctx context.Context, orderID uuid.UUID, now time.Time) error {
	if r == nil || orderID == uuid.Nil || now.IsZero() {
		return ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	command, err := r.store.Queryer().Exec(dbCtx, `
		UPDATE payment_orders SET updated_at = $2
		WHERE id = $1 AND status IN ('created', 'pending', 'refund_pending')`, orderID, now.UTC())
	if err != nil {
		return mapRepositoryError(err)
	}
	if command.RowsAffected() == 0 {
		return ErrInvalidTransition
	}
	return nil
}

func (r *Repository) ListReconciliationOrders(ctx context.Context, limit int) ([]Order, error) {
	if r == nil || limit < 1 || limit > 1000 {
		return nil, ErrInvalidInput
	}
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	rows, err := r.store.Queryer().Query(dbCtx, `
		SELECT `+orderColumns+`
		FROM payment_orders
		WHERE (status = 'pending' AND provider_trade_no IS NOT NULL)
		   OR (status = 'refund_pending' AND provider_refund_no IS NOT NULL)
		ORDER BY updated_at, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	defer rows.Close()
	orders := make([]Order, 0, limit)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, mapRepositoryError(err)
	}
	return orders, nil
}

func (r *Repository) MarkExpired(ctx context.Context, orderID uuid.UUID, now time.Time) (bool, error) {
	dbCtx, cancel := paymentContext(ctx)
	defer cancel()
	expired := false
	err := r.store.WithTx(dbCtx, func(q data.Querier) error {
		order, err := loadOrderByID(dbCtx, q, orderID, true)
		if err != nil {
			return mapRepositoryError(err)
		}
		if (order.Status != StatusCreated && order.Status != StatusPending) || order.ExpiresAt == nil || order.ExpiresAt.After(now) {
			return nil
		}
		if _, err := q.Exec(dbCtx, `
			UPDATE payment_orders SET status = 'expired', updated_at = $2
			WHERE id = $1`, order.ID, now); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(dbCtx, q, audit.Event{
			Action: "payment.order.expired", TargetType: "payment_order",
			TargetID: order.ID.String(), RequestID: order.ID.String(), Result: "success",
		}); err != nil {
			return err
		}
		if err := insertPaymentOutbox(dbCtx, q, "payment_order", order.ID, "payment.order.expired", "payment:expire:v1:"+order.ID.String(), map[string]any{
			"order_id": order.ID, "order_no": order.OrderNo,
		}); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

func loadOrderByID(ctx context.Context, q data.Querier, orderID uuid.UUID, forUpdate bool) (Order, error) {
	query := `SELECT ` + orderColumns + ` FROM payment_orders WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanOrder(q.QueryRow(ctx, query, orderID))
}

func loadOrderByNo(ctx context.Context, q data.Querier, orderNo string, forUpdate bool) (Order, error) {
	query := `SELECT ` + orderColumns + ` FROM payment_orders WHERE order_no = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanOrder(q.QueryRow(ctx, query, orderNo))
}

func loadOrderByIdempotency(ctx context.Context, q data.Querier, key string, forUpdate bool) (Order, error) {
	query := `SELECT ` + orderColumns + ` FROM payment_orders WHERE idempotency_key = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanOrder(q.QueryRow(ctx, query, key))
}

type rowScanner interface {
	Scan(...any) error
}

func scanOrder(row rowScanner) (Order, error) {
	var value Order
	var checkout []byte
	if err := row.Scan(
		&value.ID, &value.OrderNo, &value.UserID, &value.AmountMinor, &value.Currency,
		&value.PaymentProvider, &value.MerchantID, &value.ProviderLiveMode, &value.ProviderTradeNo, &value.ProviderRefundNo,
		&value.ProviderPaymentIntentNo, &value.ProviderChargeNo,
		&value.Status, &value.IdempotencyKey, &checkout, &value.FailureClass,
		&value.WalletID, &value.RechargeLedgerID, &value.RefundHoldLedgerID, &value.RefundLedgerID,
		&value.ExternalRefundedAmountMinor, &value.UpdatedBy,
		&value.ExpiresAt, &value.PaidAt, &value.RefundedAt, &value.ClosedAt,
		&value.CreatedAt, &value.UpdatedAt,
	); err != nil {
		return Order{}, err
	}
	value.CheckoutData = map[string]string{}
	if len(checkout) > 0 {
		if err := json.Unmarshal(checkout, &value.CheckoutData); err != nil {
			return Order{}, fmt.Errorf("decode payment checkout data: %w", err)
		}
	}
	return value, nil
}

func insertPaymentOutbox(ctx context.Context, q data.Querier, aggregateType string, aggregateID uuid.UUID, eventType, key string, payload any) error {
	if aggregateType != "payment_order" && aggregateType != "payment_voucher" {
		return ErrInvalidInput
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payment outbox payload: %w", err)
	}
	outboxID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate payment outbox UUIDv7: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO outbox_events (
			id, aggregate_type, aggregate_id, event_type, idempotency_key, payload
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (aggregate_type, aggregate_id, event_type, idempotency_key) DO NOTHING`,
		outboxID, aggregateType, aggregateID, eventType, key, encoded); err != nil {
		return mapRepositoryError(err)
	}
	return nil
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
		case "23503", "23514", "22P02", "22003":
			return ErrInvalidInput
		case "23505", "55000":
			return ErrConflict
		}
	}
	return err
}

func paymentContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), paymentDurableTimeout)
}
