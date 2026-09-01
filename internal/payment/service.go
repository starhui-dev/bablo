package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/billing"
)

const (
	defaultOrderTTL          = 15 * time.Minute
	maximumPageLimit         = 100
	voucherPrefix            = "bablo_vc_"
	providerOperationTimeout = 15 * time.Second
)

type Options struct {
	OrderTTL time.Duration
	Now      func() time.Time
}

// Service validates payment operations and delegates provider-specific work to
// the configured registry.
type Service struct {
	repository *Repository
	billing    *billing.Service
	providers  *Registry
	orderTTL   time.Duration
	now        func() time.Time
}

func NewService(repository *Repository, billingService *billing.Service, providers *Registry, options ...Options) (*Service, error) {
	if repository == nil || billingService == nil || providers == nil || len(options) > 1 {
		return nil, ErrInvalidInput
	}
	config := Options{OrderTTL: defaultOrderTTL, Now: time.Now}
	if len(options) == 1 {
		if options[0].OrderTTL != 0 {
			config.OrderTTL = options[0].OrderTTL
		}
		if options[0].Now != nil {
			config.Now = options[0].Now
		}
	}
	if config.OrderTTL < time.Minute || config.OrderTTL > 24*time.Hour {
		return nil, ErrInvalidInput
	}
	return &Service{
		repository: repository,
		billing:    billingService,
		providers:  providers,
		orderTTL:   config.OrderTTL,
		now:        func() time.Time { return config.Now().UTC() },
	}, nil
}

func (s *Service) WebhookResponse(providerName string, processingError error) WebhookResponse {
	if s == nil || s.providers == nil {
		return WebhookResponse{StatusCode: 503, ContentType: "application/json", Body: []byte(`{"status":"unavailable"}`)}
	}
	provider, ok := s.providers.Provider(providerName)
	if !ok {
		return WebhookResponse{StatusCode: 404, ContentType: "application/json", Body: []byte(`{"status":"unknown_provider"}`)}
	}
	response := provider.WebhookResponse(processingError)
	if response.StatusCode < 200 || response.StatusCode > 599 || len(response.Body) > 4096 {
		return WebhookResponse{StatusCode: 500, ContentType: "application/json", Body: []byte(`{"status":"invalid_provider_response"}`)}
	}
	if response.ContentType == "" {
		response.ContentType = "application/octet-stream"
	}
	return response
}

func (s *Service) ProviderNames() []string {
	if s == nil {
		return []string{}
	}
	return s.providers.Names()
}

func (s *Service) CreateOrder(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	if s == nil || s.repository == nil {
		return CreateOrderResult{}, ErrInvalidInput
	}
	normalized, err := normalizeCreateOrderInput(input)
	if err != nil {
		return CreateOrderResult{}, err
	}
	provider, ok := s.providers.Provider(normalized.PaymentProvider)
	if !ok {
		return CreateOrderResult{}, ErrProviderUnavailable
	}
	providerIdentity, err := normalizeProviderIdentity(provider.Identity())
	if err != nil {
		return CreateOrderResult{}, err
	}
	normalized.IdempotencyKey = orderIdempotencyKey(normalized.UserID, normalized.IdempotencyKey)
	now := s.now()
	order, err := s.repository.CreateOrder(ctx, normalized, providerIdentity, now.Add(s.orderTTL), now)
	if err != nil {
		return CreateOrderResult{}, err
	}
	if order.Status == StatusPending {
		return CreateOrderResult{Order: order, Checkout: checkoutFromOrder(order)}, nil
	}
	if order.Status != StatusCreated {
		return CreateOrderResult{Order: order}, nil
	}
	if order.ExpiresAt == nil {
		return CreateOrderResult{Order: order}, ErrProviderRejected
	}
	claim, err := s.repository.ClaimProviderOperation(ctx, order.ID, OperationCreate, providerIdentity, providerOperationPayload(OperationCreate, order, providerIdentity), now)
	if err != nil {
		return CreateOrderResult{Order: order}, err
	}
	return s.executeCreateClaim(ctx, provider, claim)
}

func (s *Service) executeCreateClaim(ctx context.Context, provider Provider, claim ProviderOperationClaim) (CreateOrderResult, error) {
	order := claim.Order
	if !sameProviderIdentity(provider.Identity(), ProviderIdentity{MerchantID: claim.Operation.MerchantID, LiveMode: claim.Operation.ProviderLiveMode}) {
		return CreateOrderResult{Order: order}, ErrProviderUnavailable
	}
	if !claim.Claimed {
		switch claim.Operation.Status {
		case OperationSucceeded:
			if order.Status == StatusPending {
				return CreateOrderResult{Order: order, Checkout: checkoutFromOrder(order)}, nil
			}
			return CreateOrderResult{Order: order}, nil
		case OperationDefinitiveFailed:
			return CreateOrderResult{Order: order}, ErrProviderRejected
		default:
			return CreateOrderResult{Order: order}, ErrOperationPending
		}
	}
	if claim.Operation.OwnerToken == nil || order.ExpiresAt == nil {
		return CreateOrderResult{Order: order}, ErrOperationPending
	}
	providerCtx, providerCancel := paymentProviderContext(ctx)
	checkout, providerErr := provider.CreateOrder(providerCtx, ProviderCreateInput{
		OrderNo: order.OrderNo, AmountMinor: order.AmountMinor, Currency: order.Currency,
		ExpiresAt: *order.ExpiresAt,
	})
	providerCancel()
	if providerErr != nil {
		definitive := errors.Is(providerErr, ErrProviderRejected) || errors.Is(providerErr, ErrInvalidInput)
		failed, finishErr := s.repository.FinishCreateOperation(ctx, order.ID, *claim.Operation.OwnerToken, "provider_create_failed", definitive, s.now())
		if finishErr != nil {
			return CreateOrderResult{Order: order}, finishErr
		}
		if definitive {
			return CreateOrderResult{Order: failed}, ErrProviderRejected
		}
		return CreateOrderResult{Order: failed}, providerError(providerErr)
	}
	checkout, err := validateCheckout(checkout)
	if err != nil || (checkout.ExpiresAt != nil && checkout.ExpiresAt.Before(order.CreatedAt)) {
		_, finishErr := s.repository.FinishCreateOperation(ctx, order.ID, *claim.Operation.OwnerToken, "provider_response_invalid", false, s.now())
		if finishErr != nil {
			return CreateOrderResult{Order: order}, finishErr
		}
		return CreateOrderResult{Order: order}, ErrProviderUnavailable
	}
	pending, err := s.repository.CompleteCreateOperation(ctx, order.ID, *claim.Operation.OwnerToken, checkout, s.now())
	if err != nil {
		return CreateOrderResult{Order: order}, err
	}
	return CreateOrderResult{Order: pending, Checkout: checkoutFromOrder(pending)}, nil
}

func (s *Service) GetOrder(ctx context.Context, userID uuid.UUID, orderNo string) (Order, error) {
	if s == nil || userID == uuid.Nil {
		return Order{}, ErrInvalidInput
	}
	orderNo = normalizeOrderNo(orderNo)
	if orderNo == "" {
		return Order{}, ErrInvalidInput
	}
	return s.repository.GetOrderForUser(ctx, userID, orderNo)
}

func (s *Service) ListOrders(ctx context.Context, userID uuid.UUID, cursor *PageCursor, limit int) (OrderPage, error) {
	if s == nil || userID == uuid.Nil || limit < 1 || limit > maximumPageLimit {
		return OrderPage{}, ErrInvalidInput
	}
	if cursor != nil && (cursor.ID == uuid.Nil || cursor.CreatedAt.IsZero()) {
		return OrderPage{}, ErrInvalidInput
	}
	return s.repository.ListOrders(ctx, userID, cursor, limit)
}

func (s *Service) ManualRecharge(ctx context.Context, input ManualRechargeInput) (billing.LedgerEntry, error) {
	if s == nil || input.OperatorUserID == uuid.Nil || input.UserID == uuid.Nil || input.AmountMinor <= 0 {
		return billing.LedgerEntry{}, ErrInvalidInput
	}
	input.Currency = normalizeCurrency(input.Currency)
	input.RequestID = normalizeText(input.RequestID, 128)
	input.IdempotencyKey = normalizeText(input.IdempotencyKey, 128)
	if input.Currency == "" || input.RequestID == "" || len(input.IdempotencyKey) < 8 {
		return billing.LedgerEntry{}, ErrInvalidInput
	}
	idempotencyKey := financialIdempotencyKey("admin-recharge", input.OperatorUserID, input.UserID, input.IdempotencyKey)
	return s.repository.ManualRecharge(ctx, input, idempotencyKey, s.now())
}

func (s *Service) CreateVoucher(ctx context.Context, input CreateVoucherInput) (CreatedVoucher, error) {
	if s == nil || input.OperatorUserID == uuid.Nil || input.AmountMinor <= 0 {
		return CreatedVoucher{}, ErrInvalidInput
	}
	input.Currency = normalizeCurrency(input.Currency)
	input.RequestID = normalizeText(input.RequestID, 128)
	input.IdempotencyKey = normalizeText(input.IdempotencyKey, 128)
	if input.Currency == "" || input.RequestID == "" || len(input.IdempotencyKey) < 8 {
		return CreatedVoucher{}, ErrInvalidInput
	}
	now := s.now()
	if input.ExpiresAt != nil {
		expires := input.ExpiresAt.UTC()
		if !expires.After(now) {
			return CreatedVoucher{}, ErrInvalidInput
		}
		input.ExpiresAt = &expires
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return CreatedVoucher{}, fmt.Errorf("generate payment voucher secret: %w", err)
	}
	code := voucherPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(code))
	prefix := code
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	input.IdempotencyKey = financialIdempotencyKey("voucher-create", input.OperatorUserID, input.OperatorUserID, input.IdempotencyKey)
	return s.repository.CreateVoucher(ctx, input, code, hash, prefix, now)
}

func (s *Service) RedeemVoucher(ctx context.Context, input RedeemVoucherInput) (VoucherRedemption, error) {
	if s == nil || input.UserID == uuid.Nil {
		return VoucherRedemption{}, ErrInvalidInput
	}
	input.Code = strings.TrimSpace(input.Code)
	input.RequestID = normalizeText(input.RequestID, 128)
	if !validVoucherCode(input.Code) || input.RequestID == "" {
		return VoucherRedemption{}, ErrVoucherInvalid
	}
	hash := sha256.Sum256([]byte(input.Code))
	input.Code = ""
	return s.repository.RedeemVoucher(ctx, input, hash, s.now())
}

func (s *Service) RevokeVoucher(ctx context.Context, operatorUserID, voucherID uuid.UUID, requestID string) (Voucher, error) {
	requestID = normalizeText(requestID, 128)
	if s == nil || operatorUserID == uuid.Nil || voucherID == uuid.Nil || requestID == "" {
		return Voucher{}, ErrInvalidInput
	}
	return s.repository.RevokeVoucher(ctx, operatorUserID, voucherID, requestID, s.now())
}

func (s *Service) Refund(ctx context.Context, input RefundInput) (Order, error) {
	input.OrderNo = normalizeOrderNo(input.OrderNo)
	input.RequestID = normalizeText(input.RequestID, 128)
	if s == nil || input.OperatorUserID == uuid.Nil || input.OrderNo == "" || input.RequestID == "" {
		return Order{}, ErrInvalidInput
	}
	order, err := s.repository.GetOrder(ctx, input.OrderNo)
	if err != nil {
		return Order{}, err
	}
	if order.ExternalRefundedAmountMinor > 0 {
		return Order{}, ErrInvalidTransition
	}
	provider, ok := s.providers.Provider(order.PaymentProvider)
	if !ok {
		return Order{}, ErrProviderUnavailable
	}
	providerIdentity, err := normalizeProviderIdentity(provider.Identity())
	if err != nil {
		return Order{}, err
	}
	prepared, err := s.repository.PrepareRefund(ctx, input, providerIdentity, s.now())
	if err != nil {
		return Order{}, err
	}
	if prepared.Status == StatusRefunded || prepared.ProviderRefundNo != "" {
		return prepared, nil
	}
	claim, err := s.repository.ClaimRefundOperation(ctx, prepared.ID, providerIdentity, providerOperationPayload(OperationRefund, prepared, providerIdentity), s.now())
	if err != nil {
		return prepared, err
	}
	return s.executeRefundClaim(ctx, provider, claim)
}

func (s *Service) executeRefundClaim(ctx context.Context, provider Provider, claim ProviderOperationClaim) (Order, error) {
	prepared := claim.Order
	if !sameProviderIdentity(provider.Identity(), ProviderIdentity{MerchantID: claim.Operation.MerchantID, LiveMode: claim.Operation.ProviderLiveMode}) {
		return prepared, ErrProviderUnavailable
	}
	if !claim.Claimed {
		switch claim.Operation.Status {
		case OperationSucceeded:
			return prepared, nil
		case OperationDefinitiveFailed:
			return prepared, ErrProviderRejected
		default:
			return prepared, ErrRefundPending
		}
	}
	if claim.Operation.OwnerToken == nil {
		return prepared, ErrRefundPending
	}
	providerCtx, providerCancel := paymentProviderContext(ctx)
	providerResult, providerErr := provider.Refund(providerCtx, ProviderRefundInput{
		OrderNo: prepared.OrderNo, ProviderTradeNo: prepared.ProviderTradeNo,
		AmountMinor: prepared.AmountMinor, Currency: prepared.Currency,
	})
	providerCancel()
	if providerErr != nil {
		definitive := errors.Is(providerErr, ErrProviderRejected) || errors.Is(providerErr, ErrInvalidInput)
		finished, finishErr := s.repository.FinishRefundOperation(ctx, prepared.ID, *claim.Operation.OwnerToken, "provider_refund_failed", definitive, s.now())
		if finishErr != nil {
			return prepared, finishErr
		}
		if definitive {
			return finished, ErrProviderRejected
		}
		return finished, fmt.Errorf("%w: %v", ErrRefundPending, providerErr)
	}
	providerResult.ProviderRefundNo = normalizeTradeNo(providerResult.ProviderRefundNo)
	if providerResult.ProviderRefundNo == "" {
		_, finishErr := s.repository.FinishRefundOperation(ctx, prepared.ID, *claim.Operation.OwnerToken, "provider_response_invalid", false, s.now())
		if finishErr != nil {
			return prepared, finishErr
		}
		return prepared, ErrRefundPending
	}
	updated, err := s.repository.CompleteRefundOperation(ctx, prepared.ID, *claim.Operation.OwnerToken, providerResult.ProviderRefundNo, s.now())
	if err != nil {
		return prepared, err
	}
	// Money moves only after a signed webhook or authenticated provider API
	// reconciliation. The synchronous response stores only the refund ID.
	return updated, nil
}

func (s *Service) Close(ctx context.Context, input CloseInput) (Order, error) {
	input.OrderNo = normalizeOrderNo(input.OrderNo)
	input.RequestID = normalizeText(input.RequestID, 128)
	if s == nil || input.OperatorUserID == uuid.Nil || input.OrderNo == "" || input.RequestID == "" {
		return Order{}, ErrInvalidInput
	}
	order, err := s.repository.GetOrder(ctx, input.OrderNo)
	if err != nil {
		return Order{}, err
	}
	if order.Status == StatusClosed {
		return order, nil
	}
	switch order.Status {
	case StatusCreated, StatusPending, StatusFailed, StatusExpired:
	default:
		return Order{}, ErrInvalidTransition
	}
	provider, ok := s.providers.Provider(order.PaymentProvider)
	if !ok {
		return Order{}, ErrProviderUnavailable
	}
	providerCtx, providerCancel := paymentProviderContext(ctx)
	err = provider.Close(providerCtx, ProviderCloseInput{OrderNo: order.OrderNo, ProviderTradeNo: order.ProviderTradeNo})
	providerCancel()
	if err != nil {
		return Order{}, providerError(err)
	}
	return s.repository.MarkClosed(ctx, input, s.now())
}

func (s *Service) ExpireDue(ctx context.Context, limit int) (int, error) {
	if s == nil || limit < 1 || limit > 1000 {
		return 0, ErrInvalidInput
	}
	orders, err := s.repository.ListDueOrders(ctx, limit, s.now())
	if err != nil {
		return 0, err
	}
	expired := 0
	failures := make([]error, 0)
	for _, order := range orders {
		observedAt := s.now()
		provider, ok := s.providers.Provider(order.PaymentProvider)
		if !ok {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("expire order %s: %w", order.OrderNo, ErrProviderUnavailable)))
			continue
		}
		providerCtx, providerCancel := paymentProviderContext(ctx)
		err := provider.Close(providerCtx, ProviderCloseInput{OrderNo: order.OrderNo, ProviderTradeNo: order.ProviderTradeNo})
		providerCancel()
		if err != nil {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("expire order %s: %w", order.OrderNo, err)))
			continue
		}
		changed, err := s.repository.MarkExpired(ctx, order.ID, s.now())
		if err != nil {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("expire order %s: %w", order.OrderNo, err)))
			continue
		}
		if changed {
			expired++
		}
	}
	return expired, errors.Join(failures...)
}

func (s *Service) RecoverProviderOperations(ctx context.Context, limit int) (int, error) {
	if s == nil || limit < 1 || limit > 1000 {
		return 0, ErrInvalidInput
	}
	now := s.now()
	due, err := s.repository.ListDueProviderOperations(ctx, limit, now)
	if err != nil {
		return 0, err
	}
	recovered := 0
	failures := make([]error, 0)
	for _, operation := range due {
		identity := ProviderIdentity{MerchantID: operation.MerchantID, LiveMode: operation.ProviderLiveMode}
		claim, err := s.repository.ClaimProviderOperation(ctx, operation.OrderID, operation.OperationType, identity, operation.PayloadSHA256, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("claim payment provider operation %s: %w", operation.OrderID, err))
			continue
		}
		if !claim.Claimed {
			continue
		}
		provider, ok := s.providers.Provider(claim.Order.PaymentProvider)
		if !ok {
			if claim.Operation.OwnerToken != nil {
				var finishErr error
				if operation.OperationType == OperationCreate {
					_, finishErr = s.repository.FinishCreateOperation(ctx, claim.Order.ID, *claim.Operation.OwnerToken, "provider_unconfigured", false, s.now())
				} else {
					_, finishErr = s.repository.FinishRefundOperation(ctx, claim.Order.ID, *claim.Operation.OwnerToken, "provider_unconfigured", false, s.now())
				}
				if finishErr != nil {
					failures = append(failures, fmt.Errorf("persist unconfigured payment provider operation %s: %w", operation.OrderID, finishErr))
					continue
				}
			}
			failures = append(failures, fmt.Errorf("recover payment provider operation %s: %w", operation.OrderID, ErrProviderUnavailable))
			continue
		}
		if !sameProviderIdentity(provider.Identity(), identity) {
			if claim.Operation.OwnerToken != nil {
				if operation.OperationType == OperationCreate {
					_, err = s.repository.FinishCreateOperation(ctx, claim.Order.ID, *claim.Operation.OwnerToken, "provider_identity_unavailable", false, s.now())
				} else {
					_, err = s.repository.FinishRefundOperation(ctx, claim.Order.ID, *claim.Operation.OwnerToken, "provider_identity_unavailable", false, s.now())
				}
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("persist payment provider identity mismatch %s: %w", operation.OrderID, err))
			} else {
				failures = append(failures, fmt.Errorf("recover payment provider operation %s: %w", operation.OrderID, ErrProviderUnavailable))
			}
			continue
		}
		switch operation.OperationType {
		case OperationCreate:
			_, err = s.executeCreateClaim(ctx, provider, claim)
		case OperationRefund:
			_, err = s.executeRefundClaim(ctx, provider, claim)
		default:
			err = ErrInvalidInput
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("recover payment provider operation %s: %w", operation.OrderID, err))
			continue
		}
		recovered++
	}
	return recovered, errors.Join(failures...)
}

func (s *Service) ReconcileOrders(ctx context.Context, limit int) (int, error) {
	if s == nil || limit < 1 || limit > 1000 {
		return 0, ErrInvalidInput
	}
	orders, err := s.repository.ListReconciliationOrders(ctx, limit)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	failures := make([]error, 0)
	for _, order := range orders {
		observedAt := s.now()
		provider, ok := s.providers.Provider(order.PaymentProvider)
		if !ok {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("reconcile payment order %s: %w", order.OrderNo, ErrProviderUnavailable)))
			continue
		}
		providerCtx, providerCancel := paymentProviderContext(ctx)
		var state ProviderState
		if order.Status == StatusRefundPending {
			state, err = provider.ReconcileRefund(providerCtx, ProviderRefundStateInput{
				OrderNo: order.OrderNo, ProviderTradeNo: order.ProviderTradeNo,
				ProviderRefundNo: order.ProviderRefundNo, AmountMinor: order.AmountMinor,
				Currency: order.Currency, ObservedAt: observedAt,
			})
		} else {
			state, err = provider.ReconcilePayment(providerCtx, ProviderPaymentStateInput{
				OrderNo: order.OrderNo, ProviderTradeNo: order.ProviderTradeNo,
				AmountMinor: order.AmountMinor, Currency: order.Currency,
				ExpiresAt: order.ExpiresAt, ObservedAt: observedAt,
			})
		}
		providerCancel()
		if err != nil {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("reconcile payment order %s: %w", order.OrderNo, providerError(err))))
			continue
		}
		state.MerchantID = normalizeTradeNo(state.MerchantID)
		state.ProviderTradeNo = normalizeTradeNo(state.ProviderTradeNo)
		state.ProviderRefundNo = normalizeTradeNo(state.ProviderRefundNo)
		state.Currency = normalizeCurrency(state.Currency)
		if state.EventType == EventPending {
			if err := s.repository.TouchMaintenance(ctx, order.ID, observedAt); err != nil {
				failures = append(failures, fmt.Errorf("defer pending payment reconciliation %s: %w", order.OrderNo, err))
			}
			continue
		}
		merchantMismatch := order.MerchantID == "" || state.MerchantID != order.MerchantID
		modeMismatch := order.ProviderLiveMode == nil || state.LiveMode != *order.ProviderLiveMode
		refundMismatch := order.Status == StatusRefundPending && state.ProviderRefundNo != order.ProviderRefundNo
		if merchantMismatch || modeMismatch || state.ProviderTradeNo != order.ProviderTradeNo || refundMismatch || state.AmountMinor != order.AmountMinor || state.Currency != order.Currency || !validEventType(state.EventType) || state.OccurredAt.IsZero() {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("reconcile payment order %s: %w", order.OrderNo, ErrWebhookMismatch)))
			continue
		}
		result, err := s.repository.ProcessProviderState(ctx, order.PaymentProvider, order, state, observedAt)
		if err != nil {
			failures = append(failures, s.maintenanceFailure(ctx, order, observedAt, fmt.Errorf("persist payment reconciliation %s: %w", order.OrderNo, err)))
			continue
		}
		if !result.Rejected {
			reconciled++
		}
	}
	return reconciled, errors.Join(failures...)
}

func (s *Service) maintenanceFailure(ctx context.Context, order Order, now time.Time, cause error) error {
	if err := s.repository.TouchMaintenance(ctx, order.ID, now); err != nil {
		return errors.Join(cause, fmt.Errorf("defer payment order maintenance %s: %w", order.OrderNo, err))
	}
	return cause
}

func (s *Service) HandleWebhook(ctx context.Context, providerName string, headers map[string][]string, body []byte, requestID string, receivedAt time.Time) (WebhookResult, error) {
	providerName = normalizeProviderName(providerName)
	requestID = normalizeText(requestID, 128)
	if s == nil || providerName == "" || len(body) == 0 || requestID == "" {
		return WebhookResult{}, ErrInvalidInput
	}
	provider, ok := s.providers.Provider(providerName)
	if !ok {
		return WebhookResult{}, ErrProviderUnavailable
	}
	if receivedAt.IsZero() {
		receivedAt = s.now()
	} else {
		receivedAt = receivedAt.UTC()
	}
	hash := sha256.Sum256(body)
	verified, err := provider.VerifyWebhook(ctx, SignedWebhook{Headers: cloneHeaders(headers), Body: append([]byte(nil), body...), ReceivedAt: receivedAt})
	if err != nil {
		if !errors.Is(err, ErrWebhookInvalid) {
			return WebhookResult{}, fmt.Errorf("verify payment webhook: %w", err)
		}
		return WebhookResult{}, ErrWebhookInvalid
	}
	verified.ProviderEventID = normalizeEventID(verified.ProviderEventID)
	verified.MerchantID = normalizeTradeNo(verified.MerchantID)
	verified.OrderNo = normalizeOrderNo(verified.OrderNo)
	verified.ProviderTradeNo = normalizeTradeNo(verified.ProviderTradeNo)
	verified.ProviderRefundNo = normalizeTradeNo(verified.ProviderRefundNo)
	verified.ProviderPaymentIntentNo = normalizeTradeNo(verified.ProviderPaymentIntentNo)
	verified.ProviderChargeNo = normalizeTradeNo(verified.ProviderChargeNo)
	verified.ProviderDisputeNo = normalizeTradeNo(verified.ProviderDisputeNo)
	verified.Currency = normalizeCurrency(verified.Currency)
	hasOrderReference := verified.OrderNo != "" || verified.ProviderPaymentIntentNo != "" || verified.ProviderChargeNo != ""
	disputeReferenceInvalid := isDisputeEvent(verified.EventType) && (verified.ProviderDisputeNo == "" || verified.ProviderChargeNo == "")
	if verified.ProviderEventID == "" || !hasOrderReference || verified.ProviderTradeNo == "" ||
		!validEventType(verified.EventType) || (isRefundTerminalEvent(verified.EventType) && verified.ProviderRefundNo == "") ||
		disputeReferenceInvalid || verified.AmountMinor <= 0 || verified.Currency == "" || verified.MerchantID == "" || verified.OccurredAt.IsZero() {
		return WebhookResult{}, ErrWebhookInvalid
	}
	verified.OccurredAt = verified.OccurredAt.UTC()
	return s.repository.ProcessVerifiedEvent(ctx, providerName, verified, hash, requestID, receivedAt)
}

func normalizeCreateOrderInput(input CreateOrderInput) (CreateOrderInput, error) {
	if input.UserID == uuid.Nil || input.AmountMinor <= 0 {
		return CreateOrderInput{}, ErrInvalidInput
	}
	input.Currency = normalizeCurrency(input.Currency)
	input.PaymentProvider = normalizeProviderName(input.PaymentProvider)
	input.IdempotencyKey = normalizeText(input.IdempotencyKey, 128)
	if input.Currency == "" || input.PaymentProvider == "" || len(input.IdempotencyKey) < 8 {
		return CreateOrderInput{}, ErrInvalidInput
	}
	return input, nil
}

func normalizeCurrency(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 3 {
		return ""
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return ""
		}
	}
	return value
}

func normalizeOrderNo(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 8 || len(value) > 80 || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func normalizeText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func orderIdempotencyKey(userID uuid.UUID, clientKey string) string {
	hash := sha256.Sum256([]byte(userID.String() + ":" + clientKey))
	return "payment:create:v1:" + hex.EncodeToString(hash[:])
}

func providerOperationPayload(operation string, order Order, identity ProviderIdentity) [sha256.Size]byte {
	expiresAt := ""
	if order.ExpiresAt != nil {
		expiresAt = order.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	payload := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%t", operation,
		order.PaymentProvider, order.OrderNo, order.AmountMinor, order.Currency,
		order.ProviderTradeNo, expiresAt, identity.MerchantID, identity.LiveMode)
	return sha256.Sum256([]byte(payload))
}

func sameProviderIdentity(left, right ProviderIdentity) bool {
	left, leftErr := normalizeProviderIdentity(left)
	right, rightErr := normalizeProviderIdentity(right)
	return leftErr == nil && rightErr == nil && left == right
}

func checkoutFromOrder(order Order) Checkout {
	liveMode := false
	if order.ProviderLiveMode != nil {
		liveMode = *order.ProviderLiveMode
	}
	return Checkout{
		ProviderTradeNo: order.ProviderTradeNo,
		MerchantID:      order.MerchantID,
		LiveMode:        liveMode,
		Data:            cloneStringMap(order.CheckoutData),
		ExpiresAt:       cloneTime(order.ExpiresAt),
	}
}

func financialIdempotencyKey(kind string, operatorID, userID uuid.UUID, requestID string) string {
	hash := sha256.Sum256([]byte(kind + ":" + operatorID.String() + ":" + userID.String() + ":" + requestID))
	return "payment:" + kind + ":v1:" + hex.EncodeToString(hash[:])
}

func validVoucherCode(value string) bool {
	if !strings.HasPrefix(value, voucherPrefix) || len(value) != len(voucherPrefix)+43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, voucherPrefix))
	return err == nil && len(decoded) == 32
}

func cloneStringMap(value map[string]string) map[string]string {
	if len(value) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneHeaders(value map[string][]string) map[string][]string {
	result := make(map[string][]string, len(value))
	for key, items := range value {
		result[key] = append([]string(nil), items...)
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func paymentProviderContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, providerOperationTimeout)
}
