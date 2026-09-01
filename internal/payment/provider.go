package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	fixtureProviderName    = "fixture_hmac"
	fixtureTimestampHeader = "X-Bablo-Fixture-Timestamp"
	fixtureSignatureHeader = "X-Bablo-Fixture-Signature"
)

// Checkout contains only safe client-facing payment instructions.
type Checkout struct {
	ProviderTradeNo string            `json:"provider_trade_no,omitempty"`
	MerchantID      string            `json:"merchant_id,omitempty"`
	LiveMode        bool              `json:"-"`
	Data            map[string]string `json:"data,omitempty"`
	ExpiresAt       *time.Time        `json:"expires_at,omitempty"`
}

type ProviderCreateInput struct {
	OrderNo     string
	AmountMinor int64
	Currency    string
	ExpiresAt   time.Time
}

type ProviderIdentity struct {
	MerchantID string
	LiveMode   bool
}

type ProviderRefundInput struct {
	OrderNo         string
	ProviderTradeNo string
	AmountMinor     int64
	Currency        string
}

type ProviderRefundResult struct {
	ProviderRefundNo string
}

type ProviderCloseInput struct {
	OrderNo         string
	ProviderTradeNo string
}

type ProviderPaymentStateInput struct {
	OrderNo         string
	ProviderTradeNo string
	AmountMinor     int64
	Currency        string
	ExpiresAt       *time.Time
	ObservedAt      time.Time
}

type ProviderRefundStateInput struct {
	OrderNo          string
	ProviderTradeNo  string
	ProviderRefundNo string
	AmountMinor      int64
	Currency         string
	ObservedAt       time.Time
}

// ProviderState is an authenticated provider API observation. It is never
// treated as a signed webhook and receives a distinct persistence source.
type ProviderState struct {
	MerchantID       string
	LiveMode         bool
	ProviderTradeNo  string
	ProviderRefundNo string
	EventType        string
	AmountMinor      int64
	Currency         string
	OccurredAt       time.Time
}

type SignedWebhook struct {
	Headers    map[string][]string
	Body       []byte
	ReceivedAt time.Time
}

type WebhookResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// VerifiedEvent is returned only after a provider adapter has verified the
// signature, merchant identity, timestamp, and provider-specific event shape.
type VerifiedEvent struct {
	ProviderEventID         string
	MerchantID              string
	LiveMode                bool
	OrderNo                 string
	ProviderTradeNo         string
	ProviderRefundNo        string
	ProviderPaymentIntentNo string
	ProviderChargeNo        string
	ProviderDisputeNo       string
	EventType               string
	AmountMinor             int64
	Currency                string
	OccurredAt              time.Time
}

// Provider is the only boundary allowed to understand provider-specific
// signatures and request/response formats.
type Provider interface {
	Name() string
	CreateOrder(context.Context, ProviderCreateInput) (Checkout, error)
	VerifyWebhook(context.Context, SignedWebhook) (VerifiedEvent, error)
	WebhookResponse(error) WebhookResponse
	Refund(context.Context, ProviderRefundInput) (ProviderRefundResult, error)
	Close(context.Context, ProviderCloseInput) error
	Identity() ProviderIdentity
	ReconcilePayment(context.Context, ProviderPaymentStateInput) (ProviderState, error)
	ReconcileRefund(context.Context, ProviderRefundStateInput) (ProviderState, error)
}

// Registry stores explicitly configured providers. An empty registry is a safe
// production state: self-service order creation and webhooks fail closed while
// vouchers and administrator recharge remain available.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	registry := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if provider == nil {
			return nil, ErrInvalidInput
		}
		name := normalizeProviderName(provider.Name())
		if name == "" || name != provider.Name() {
			return nil, ErrInvalidInput
		}
		if _, exists := registry.providers[name]; exists {
			return nil, ErrConflict
		}
		registry.providers[name] = provider
	}
	return registry, nil
}

func (r *Registry) Provider(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	provider, ok := r.providers[normalizeProviderName(name)]
	return provider, ok
}

func (r *Registry) Names() []string {
	if r == nil {
		return []string{}
	}
	result := make([]string, 0, len(r.providers))
	for name := range r.providers {
		result = append(result, name)
	}
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j] < result[j-1]; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

func normalizeProviderIdentity(value ProviderIdentity) (ProviderIdentity, error) {
	value.MerchantID = strings.TrimSpace(value.MerchantID)
	if value.MerchantID == "" || len(value.MerchantID) > 160 {
		return ProviderIdentity{}, ErrProviderUnavailable
	}
	return value, nil
}

func normalizeProviderName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return ""
	}
	return value
}

// FixtureProvider is a deterministic HMAC adapter used only by compatibility
// tests and sandbox runbooks. Bablo never registers it from production config.
type FixtureProvider struct {
	merchantID string
	secret     []byte
	tolerance  time.Duration
	now        func() time.Time
}

type FixtureProviderConfig struct {
	MerchantID string
	Secret     []byte
	Tolerance  time.Duration
	Now        func() time.Time
}

func NewFixtureProvider(config FixtureProviderConfig) (*FixtureProvider, error) {
	config.MerchantID = strings.TrimSpace(config.MerchantID)
	if config.MerchantID == "" || len(config.MerchantID) > 160 || len(config.Secret) < 32 {
		return nil, ErrInvalidInput
	}
	if config.Tolerance == 0 {
		config.Tolerance = 5 * time.Minute
	}

	if config.Tolerance < time.Second || config.Tolerance > time.Hour {
		return nil, ErrInvalidInput
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &FixtureProvider{
		merchantID: config.MerchantID,
		secret:     append([]byte(nil), config.Secret...),
		tolerance:  config.Tolerance,
		now:        func() time.Time { return config.Now().UTC() },
	}, nil
}

func (p *FixtureProvider) Name() string { return fixtureProviderName }

func (p *FixtureProvider) Identity() ProviderIdentity {
	if p == nil {
		return ProviderIdentity{}
	}
	return ProviderIdentity{MerchantID: p.merchantID, LiveMode: false}
}

func (p *FixtureProvider) CreateOrder(_ context.Context, input ProviderCreateInput) (Checkout, error) {
	if p == nil || normalizeOrderNo(input.OrderNo) == "" || input.AmountMinor <= 0 || normalizeCurrency(input.Currency) == "" || input.ExpiresAt.IsZero() {
		return Checkout{}, ErrInvalidInput
	}
	expires := input.ExpiresAt.UTC()
	return Checkout{
		ProviderTradeNo: "fixture_trade_" + input.OrderNo,
		MerchantID:      p.merchantID,
		Data: map[string]string{
			"mode": "fixture",
			"url":  "https://fixture.invalid/pay/" + url.PathEscape(input.OrderNo),
		},
		ExpiresAt: &expires,
	}, nil
}

func (p *FixtureProvider) Refund(_ context.Context, input ProviderRefundInput) (ProviderRefundResult, error) {
	if p == nil || normalizeOrderNo(input.OrderNo) == "" || strings.TrimSpace(input.ProviderTradeNo) == "" || input.AmountMinor <= 0 || normalizeCurrency(input.Currency) == "" {
		return ProviderRefundResult{}, ErrInvalidInput
	}
	return ProviderRefundResult{ProviderRefundNo: "fixture_refund_" + input.OrderNo}, nil
}

func (p *FixtureProvider) Close(_ context.Context, input ProviderCloseInput) error {
	if p == nil || normalizeOrderNo(input.OrderNo) == "" {
		return ErrInvalidInput
	}
	if input.ProviderTradeNo != "" && normalizeTradeNo(input.ProviderTradeNo) == "" {
		return ErrInvalidInput
	}
	return nil
}

func (p *FixtureProvider) ReconcilePayment(_ context.Context, input ProviderPaymentStateInput) (ProviderState, error) {
	if p == nil || normalizeOrderNo(input.OrderNo) == "" || normalizeTradeNo(input.ProviderTradeNo) == "" || input.AmountMinor <= 0 || normalizeCurrency(input.Currency) == "" || input.ObservedAt.IsZero() {
		return ProviderState{}, ErrInvalidInput
	}
	eventType := EventPending
	occurredAt := input.ObservedAt.UTC()
	if input.ExpiresAt != nil && !input.ExpiresAt.After(input.ObservedAt) {
		eventType = EventExpired
		occurredAt = input.ExpiresAt.UTC()
	}
	return ProviderState{
		MerchantID: p.merchantID, ProviderTradeNo: input.ProviderTradeNo,
		EventType: eventType, AmountMinor: input.AmountMinor,
		Currency: input.Currency, OccurredAt: occurredAt,
	}, nil
}

func (p *FixtureProvider) ReconcileRefund(_ context.Context, input ProviderRefundStateInput) (ProviderState, error) {
	if p == nil || normalizeOrderNo(input.OrderNo) == "" || normalizeTradeNo(input.ProviderTradeNo) == "" || normalizeTradeNo(input.ProviderRefundNo) == "" || input.AmountMinor <= 0 || normalizeCurrency(input.Currency) == "" || input.ObservedAt.IsZero() {
		return ProviderState{}, ErrInvalidInput
	}
	return ProviderState{
		MerchantID: p.merchantID, ProviderTradeNo: input.ProviderTradeNo,
		ProviderRefundNo: input.ProviderRefundNo, EventType: EventPending,
		AmountMinor: input.AmountMinor, Currency: input.Currency,
		OccurredAt: input.ObservedAt.UTC(),
	}, nil
}

func (p *FixtureProvider) WebhookResponse(err error) WebhookResponse {
	if err == nil {
		return WebhookResponse{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"status":"ok"}`)}
	}
	if errors.Is(err, ErrWebhookInvalid) {
		return WebhookResponse{StatusCode: 401, ContentType: "application/json", Body: []byte(`{"status":"invalid_signature"}`)}
	}
	return WebhookResponse{StatusCode: 409, ContentType: "application/json", Body: []byte(`{"status":"rejected"}`)}
}

func (p *FixtureProvider) VerifyWebhook(_ context.Context, request SignedWebhook) (VerifiedEvent, error) {
	if p == nil || len(request.Body) == 0 {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	timestampText := firstHeader(request.Headers, fixtureTimestampHeader)
	signatureText := firstHeader(request.Headers, fixtureSignatureHeader)
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	receivedAt := request.ReceivedAt.UTC()
	if receivedAt.IsZero() {
		receivedAt = p.now()
	}
	signedAt := time.Unix(timestamp, 0).UTC()
	if absoluteDuration(receivedAt.Sub(signedAt)) > p.tolerance {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	providedSignature, err := hex.DecodeString(signatureText)
	if err != nil || len(providedSignature) != sha256.Size {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write([]byte(timestampText))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(request.Body)
	if !hmac.Equal(mac.Sum(nil), providedSignature) {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	var payload struct {
		EventID         string    `json:"event_id"`
		MerchantID      string    `json:"merchant_id"`
		OrderNo         string    `json:"order_no"`
		TradeNo         string    `json:"trade_no"`
		RefundNo        string    `json:"refund_no"`
		PaymentIntentNo string    `json:"payment_intent_no"`
		ChargeNo        string    `json:"charge_no"`
		DisputeNo       string    `json:"dispute_no"`
		Status          string    `json:"status"`
		AmountMinor     int64     `json:"amount_minor"`
		Currency        string    `json:"currency"`
		OccurredAt      time.Time `json:"occurred_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	payload.EventID = normalizeEventID(payload.EventID)
	payload.MerchantID = normalizeTradeNo(payload.MerchantID)
	payload.OrderNo = normalizeOrderNo(payload.OrderNo)
	payload.TradeNo = normalizeTradeNo(payload.TradeNo)
	payload.RefundNo = normalizeTradeNo(payload.RefundNo)
	payload.PaymentIntentNo = normalizeTradeNo(payload.PaymentIntentNo)
	payload.ChargeNo = normalizeTradeNo(payload.ChargeNo)
	payload.DisputeNo = normalizeTradeNo(payload.DisputeNo)
	payload.Currency = normalizeCurrency(payload.Currency)
	if payload.MerchantID != p.merchantID || payload.EventID == "" || payload.OrderNo == "" || payload.TradeNo == "" || !validEventType(payload.Status) || payload.AmountMinor <= 0 || payload.Currency == "" || payload.OccurredAt.IsZero() {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	payload.OccurredAt = payload.OccurredAt.UTC()
	if absoluteDuration(receivedAt.Sub(payload.OccurredAt)) > p.tolerance {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	providerRefundNo := payload.RefundNo
	if providerRefundNo == "" && (payload.Status == EventRefunded || payload.Status == EventRefundFailed) {
		providerRefundNo = "fixture_refund_" + payload.OrderNo
	}
	return VerifiedEvent{
		ProviderEventID: payload.EventID, MerchantID: payload.MerchantID,
		OrderNo: payload.OrderNo, ProviderTradeNo: payload.TradeNo,
		ProviderRefundNo:        providerRefundNo,
		ProviderPaymentIntentNo: payload.PaymentIntentNo,
		ProviderChargeNo:        payload.ChargeNo, ProviderDisputeNo: payload.DisputeNo,
		EventType: payload.Status, AmountMinor: payload.AmountMinor,
		Currency: payload.Currency, OccurredAt: payload.OccurredAt,
	}, nil
}

// SignWebhook returns fixture-only headers for compatibility tests.
func (p *FixtureProvider) SignWebhook(body []byte, at time.Time) (map[string][]string, error) {
	if p == nil || len(body) == 0 || at.IsZero() {
		return nil, ErrInvalidInput
	}
	timestamp := strconv.FormatInt(at.UTC().Unix(), 10)
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return map[string][]string{
		fixtureTimestampHeader: {timestamp},
		fixtureSignatureHeader: {hex.EncodeToString(mac.Sum(nil))},
	}, nil
}

func firstHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) == 1 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func absoluteDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func validEventType(value string) bool {
	switch value {
	case EventPending, EventPaid, EventFailed, EventExpired, EventRefunded, EventRefundFailed, EventClosed,
		EventDisputeOpened, EventDisputeWon, EventDisputeLost:
		return true
	default:
		return false
	}
}

func isRefundTerminalEvent(value string) bool {
	return value == EventRefunded || value == EventRefundFailed
}

func validateCheckout(value Checkout) (Checkout, error) {
	value.ProviderTradeNo = normalizeTradeNo(value.ProviderTradeNo)
	value.MerchantID = normalizeTradeNo(value.MerchantID)
	if value.ProviderTradeNo == "" || value.MerchantID == "" {
		return Checkout{}, ErrProviderRejected
	}
	if len(value.Data) > 16 {
		return Checkout{}, ErrProviderRejected
	}
	copied := make(map[string]string, len(value.Data))
	for key, item := range value.Data {
		key = strings.TrimSpace(key)
		item = strings.TrimSpace(item)
		if key == "" || len(key) > 64 || len(item) > 2048 || strings.ContainsAny(key+item, "\r\n\x00") {
			return Checkout{}, ErrProviderRejected
		}
		copied[key] = item
	}
	value.Data = copied
	if value.ExpiresAt != nil {
		expires := value.ExpiresAt.UTC()
		value.ExpiresAt = &expires
	}
	return value, nil
}

func normalizeTradeNo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func normalizeEventID(value string) string {
	return normalizeTradeNo(value)
}

func providerError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrWebhookInvalid) || errors.Is(err, ErrProviderRejected) || errors.Is(err, ErrProviderUnavailable) {
		return err
	}
	if errors.Is(err, ErrInvalidInput) {
		return fmt.Errorf("%w: %v", ErrProviderRejected, err)
	}
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}
