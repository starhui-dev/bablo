package payment

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/httpapi"
)

const (
	maximumPaymentJSONBytes     = 32 << 10
	maximumWebhookBytes         = 256 << 10
	maximumConcurrentWebhooks   = 32
	paymentWebhookReadTimeout   = 10 * time.Second
)

// Handler exposes user, administrator, and unauthenticated webhook surfaces.
// Authentication and CSRF enforcement are composed by httpapi.Server.
type Handler struct {
	service      *Service
	logger       *slog.Logger
	webhookSlots chan struct{}
}

func NewHandler(service *Service, logger *slog.Logger) (*Handler, error) {
	if service == nil {
		return nil, errors.New("payment HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{service: service, logger: logger, webhookSlots: make(chan struct{}, maximumConcurrentWebhooks)}, nil
}

func (h *Handler) ServeUserHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/me/payment-orders":
		switch r.Method {
		case http.MethodGet:
			h.listOrders(w, r)
		case http.MethodPost:
			h.createOrder(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case r.URL.Path == "/api/v1/me/payment-vouchers/redeem":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.redeemVoucher(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/me/payment-orders/"):
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.getOrder(w, r)
	default:
		h.transportError(w, r, http.StatusNotFound, "not_found", "接口不存在。")
	}
}

func (h *Handler) ServeAdminHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/admin/wallet-credits":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.manualRecharge(w, r)
	case r.URL.Path == "/api/v1/admin/payment-vouchers":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.createVoucher(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/admin/payment-vouchers/") && strings.HasSuffix(r.URL.Path, "/revoke"):
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.revokeVoucher(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/admin/payment-orders/") && strings.HasSuffix(r.URL.Path, "/refund"):
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.refundOrder(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/admin/payment-orders/") && strings.HasSuffix(r.URL.Path, "/close"):
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.closeOrder(w, r)
	default:
		h.transportError(w, r, http.StatusNotFound, "not_found", "接口不存在。")
	}
}

func (h *Handler) ServeWebhookHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/webhooks/"
	providerName := strings.TrimPrefix(r.URL.Path, prefix)
	if !strings.HasPrefix(r.URL.Path, prefix) || providerName == "" || strings.Contains(providerName, "/") {
		h.transportError(w, r, http.StatusNotFound, "not_found", "接口不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	select {
	case h.webhookSlots <- struct{}{}:
		defer func() { <-h.webhookSlots }()
	default:
		h.transportError(w, r, http.StatusTooManyRequests, "webhook_busy", "支付回调处理繁忙，请稍后重试。")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(paymentWebhookReadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, maximumWebhookBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		h.transportError(w, r, http.StatusBadRequest, "invalid_webhook", "支付回调载荷无效。")
		return
	}
	_, processingErr := h.service.HandleWebhook(r.Context(), providerName, r.Header, body, httpapi.RequestID(r.Context()), time.Time{})
	response := h.service.WebhookResponse(providerName, processingErr)
	w.Header().Set("Content-Type", response.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(response.Body)
}

type createOrderRequest struct {
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	PaymentProvider string `json:"payment_provider"`
}

type manualRechargeRequest struct {
	UserID      string `json:"user_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type createVoucherRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

type redeemVoucherRequest struct {
	Code string `json:"code"`
}

type orderView struct {
	ID               string            `json:"id"`
	OrderNo          string            `json:"order_no"`
	AmountMinor      int64             `json:"amount_minor"`
	Currency         string            `json:"currency"`
	Status           string            `json:"status"`
	PaymentProvider  string            `json:"payment_provider"`
	ProviderTradeNo  string            `json:"provider_trade_no,omitempty"`
	ProviderRefundNo string            `json:"provider_refund_no,omitempty"`
	Checkout         map[string]string `json:"checkout,omitempty"`
	ExpiresAt        string            `json:"expires_at,omitempty"`
	PaidAt           string            `json:"paid_at,omitempty"`
	RefundedAt       string            `json:"refunded_at,omitempty"`
	ClosedAt         string            `json:"closed_at,omitempty"`
	FailureClass     string            `json:"failure_class,omitempty"`
	CreatedAt        string            `json:"created_at"`
	UpdatedAt        string            `json:"updated_at"`
}

type voucherView struct {
	ID          string `json:"id"`
	CodePrefix  string `json:"code_prefix"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	RedeemedAt  string `json:"redeemed_at,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type ledgerView struct {
	ID                  string `json:"id"`
	WalletID            string `json:"wallet_id"`
	EntryType           string `json:"entry_type"`
	AmountMinor         int64  `json:"amount_minor"`
	AvailableAfterMinor int64  `json:"available_balance_after_minor"`
	ReservedAfterMinor  int64  `json:"reserved_balance_after_minor"`
	Currency            string `json:"currency"`
	CreatedAt           string `json:"created_at"`
}

type cursorEnvelope struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	idempotencyKey, ok := singleHeader(r.Header, "Idempotency-Key")
	if !ok {
		h.transportError(w, r, http.StatusBadRequest, "idempotency_key_required", "必须提供唯一的 Idempotency-Key。")
		return
	}
	var request createOrderRequest
	if err := decodePaymentJSON(w, r, &request); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	result, err := h.service.CreateOrder(r.Context(), CreateOrderInput{
		UserID: session.UserID, AmountMinor: request.AmountMinor, Currency: request.Currency,
		PaymentProvider: request.PaymentProvider, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := newOrderView(result.Order)
	view.Checkout = cloneStringMap(result.Checkout.Data)
	writePaymentJSON(w, http.StatusCreated, map[string]any{"order": view})
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			h.writeError(w, r, ErrInvalidInput)
			return
		}
		limit = parsed
	}
	cursor, err := decodePageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	page, err := h.service.ListOrders(r.Context(), session.UserID, cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]orderView, 0, len(page.Orders))
	for _, order := range page.Orders {
		items = append(items, newOrderView(order))
	}
	nextCursor := ""
	if page.NextCursor != nil {
		nextCursor, err = encodePageCursor(*page.NextCursor)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	writePaymentJSON(w, http.StatusOK, map[string]any{"data": items, "next_cursor": nextCursor})
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	orderNo := strings.TrimPrefix(r.URL.Path, "/api/v1/me/payment-orders/")
	if orderNo == "" || strings.Contains(orderNo, "/") {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	order, err := h.service.GetOrder(r.Context(), session.UserID, orderNo)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusOK, map[string]any{"order": newOrderView(order)})
}

func (h *Handler) redeemVoucher(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	var request redeemVoucherRequest
	if err := decodePaymentJSON(w, r, &request); err != nil {
		h.writeError(w, r, ErrVoucherInvalid)
		return
	}
	result, err := h.service.RedeemVoucher(r.Context(), RedeemVoucherInput{
		UserID: session.UserID, Code: request.Code, RequestID: httpapi.RequestID(r.Context()),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusOK, map[string]any{
		"voucher": newVoucherView(result.Voucher), "ledger": newLedgerView(result.Ledger),
	})
}

func (h *Handler) manualRecharge(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	idempotencyKey, ok := singleHeader(r.Header, "Idempotency-Key")
	if !ok {
		h.transportError(w, r, http.StatusBadRequest, "idempotency_key_required", "必须提供唯一的 Idempotency-Key。")
		return
	}
	var request manualRechargeRequest
	if err := decodePaymentJSON(w, r, &request); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	userID, err := uuid.Parse(request.UserID)
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	entry, err := h.service.ManualRecharge(r.Context(), ManualRechargeInput{
		OperatorUserID: session.UserID, UserID: userID, AmountMinor: request.AmountMinor,
		Currency: request.Currency, RequestID: httpapi.RequestID(r.Context()), IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusOK, map[string]any{"ledger": newLedgerView(entry)})
}

func (h *Handler) createVoucher(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	idempotencyKey, ok := singleHeader(r.Header, "Idempotency-Key")
	if !ok {
		h.transportError(w, r, http.StatusBadRequest, "idempotency_key_required", "必须提供唯一的 Idempotency-Key。")
		return
	}
	var request createVoucherRequest
	if err := decodePaymentJSON(w, r, &request); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	var expiresAt *time.Time
	if request.ExpiresAt != "" {
		value, err := time.Parse(time.RFC3339, request.ExpiresAt)
		if err != nil {
			h.writeError(w, r, ErrInvalidInput)
			return
		}
		expiresAt = &value
	}
	created, err := h.service.CreateVoucher(r.Context(), CreateVoucherInput{
		OperatorUserID: session.UserID, AmountMinor: request.AmountMinor, Currency: request.Currency,
		ExpiresAt: expiresAt, RequestID: httpapi.RequestID(r.Context()), IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusCreated, map[string]any{
		"voucher": newVoucherView(created.Voucher), "code": created.Code,
	})
}

func (h *Handler) revokeVoucher(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	identifier, ok := pathValue(r.URL.Path, "/api/v1/admin/payment-vouchers/", "/revoke")
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	voucherID, err := uuid.Parse(identifier)
	if err != nil || decodeOptionalEmptyJSON(w, r) != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	voucher, err := h.service.RevokeVoucher(r.Context(), session.UserID, voucherID, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusOK, map[string]any{"voucher": newVoucherView(voucher)})
}

func (h *Handler) refundOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	orderNo, ok := pathValue(r.URL.Path, "/api/v1/admin/payment-orders/", "/refund")
	if !ok || decodeOptionalEmptyJSON(w, r) != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	order, err := h.service.Refund(r.Context(), RefundInput{
		OperatorUserID: session.UserID, OrderNo: orderNo, RequestID: httpapi.RequestID(r.Context()),
	})
	if err != nil && !errors.Is(err, ErrRefundPending) {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusAccepted, map[string]any{"order": newOrderView(order)})
}

func (h *Handler) closeOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	orderNo, ok := pathValue(r.URL.Path, "/api/v1/admin/payment-orders/", "/close")
	if !ok || decodeOptionalEmptyJSON(w, r) != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	order, err := h.service.Close(r.Context(), CloseInput{
		OperatorUserID: session.UserID, OrderNo: orderNo, RequestID: httpapi.RequestID(r.Context()),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writePaymentJSON(w, http.StatusOK, map[string]any{"order": newOrderView(order)})
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := publicPaymentError(err)
	if status >= http.StatusInternalServerError {
		h.logger.Error("payment_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
	}
	writePaymentJSON(w, status, map[string]any{"error": map[string]string{
		"type": "payment_error", "code": code, "message": message,
		"request_id": httpapi.RequestID(r.Context()),
	}})
}

func (h *Handler) transportError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writePaymentJSON(w, status, map[string]any{"error": map[string]string{
		"type": "invalid_request", "code": code, "message": message,
		"request_id": httpapi.RequestID(r.Context()),
	}})
}

func (h *Handler) methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	h.transportError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不受支持。")
}

func publicPaymentError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "invalid_request", "请求参数不符合要求。"
	case errors.Is(err, ErrVoucherInvalid):
		return http.StatusBadRequest, "invalid_voucher", "兑换码无效。"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found", "支付资源不存在。"
	case errors.Is(err, ErrVoucherUnavailable):
		return http.StatusConflict, "voucher_unavailable", "兑换码已使用、撤销或过期。"
	case errors.Is(err, ErrInsufficientFunds):
		return http.StatusConflict, "insufficient_funds", "钱包可用余额不足，无法退款。"
	case errors.Is(err, billing.ErrInsufficientFunds):
		return http.StatusConflict, "insufficient_funds", "钱包可用余额不足。"
	case errors.Is(err, billing.ErrSettlementConflict), errors.Is(err, billing.ErrWalletFrozen), errors.Is(err, billing.ErrReservationConflict):
		return http.StatusConflict, "financial_conflict", "钱包状态与请求不一致。"
	case errors.Is(err, billing.ErrInvalidInput):
		return http.StatusBadRequest, "invalid_request", "请求参数不符合要求。"
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrConflict), errors.Is(err, ErrWebhookReplay), errors.Is(err, ErrWebhookMismatch):
		return http.StatusConflict, "conflict", "当前状态不允许此操作。"
	case errors.Is(err, ErrOrderLimit):
		return http.StatusTooManyRequests, "active_order_limit", "未支付订单数量已达上限。"
	case errors.Is(err, ErrProviderUnavailable):
		return http.StatusServiceUnavailable, "provider_unavailable", "支付渠道尚未配置或暂时不可用。"
	case errors.Is(err, ErrProviderRejected):
		return http.StatusBadGateway, "provider_rejected", "支付渠道拒绝了请求。"
	case errors.Is(err, ErrRefundPending):
		return http.StatusAccepted, "refund_pending", "退款请求状态待渠道确认。"
	case errors.Is(err, ErrWebhookInvalid):
		return http.StatusUnauthorized, "invalid_webhook", "支付回调校验失败。"
	default:
		return http.StatusInternalServerError, "internal_error", "服务暂时无法处理请求。"
	}
}

func decodePaymentJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximumPaymentJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func decodeOptionalEmptyJSON(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximumPaymentJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var value map[string]json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if len(value) != 0 {
		return errors.New("request body must be empty")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain at most one JSON object")
	}
	return nil
}

func writePaymentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func newOrderView(order Order) orderView {
	return orderView{
		ID: order.ID.String(), OrderNo: order.OrderNo, AmountMinor: order.AmountMinor,
		Currency: order.Currency, Status: string(order.Status), PaymentProvider: order.PaymentProvider,
		ProviderTradeNo: order.ProviderTradeNo, ProviderRefundNo: order.ProviderRefundNo,
		ExpiresAt: formatOptionalTime(order.ExpiresAt), PaidAt: formatOptionalTime(order.PaidAt),
		RefundedAt: formatOptionalTime(order.RefundedAt), ClosedAt: formatOptionalTime(order.ClosedAt),
		FailureClass: order.FailureClass, CreatedAt: order.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: order.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func newVoucherView(voucher Voucher) voucherView {
	return voucherView{
		ID: voucher.ID.String(), CodePrefix: voucher.CodePrefix, AmountMinor: voucher.AmountMinor,
		Currency: voucher.Currency, Status: string(voucher.Status), ExpiresAt: formatOptionalTime(voucher.ExpiresAt),
		RedeemedAt: formatOptionalTime(voucher.RedeemedAt), CreatedAt: voucher.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: voucher.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func newLedgerView(entry billing.LedgerEntry) ledgerView {
	return ledgerView{
		ID: entry.ID.String(), WalletID: entry.WalletID.String(), EntryType: string(entry.EntryType),
		AmountMinor: entry.AmountMinor, AvailableAfterMinor: entry.AvailableBalanceAfterMinor,
		ReservedAfterMinor: entry.ReservedBalanceAfterMinor, Currency: entry.Currency,
		CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func pathValue(path, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	return value, value != "" && !strings.Contains(value, "/")
}

func singleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	return value, value != ""
}

func encodePageCursor(cursor PageCursor) (string, error) {
	encoded, err := json.Marshal(cursorEnvelope{CreatedAt: cursor.CreatedAt.UTC().Format(time.RFC3339Nano), ID: cursor.ID.String()})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodePageCursor(value string) (*PageCursor, error) {
	if value == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 512 {
		return nil, ErrInvalidInput
	}
	var envelope cursorEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, ErrInvalidInput
	}
	createdAt, err := time.Parse(time.RFC3339Nano, envelope.CreatedAt)
	if err != nil {
		return nil, ErrInvalidInput
	}
	identifier, err := uuid.Parse(envelope.ID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	return &PageCursor{CreatedAt: createdAt.UTC(), ID: identifier}, nil
}
