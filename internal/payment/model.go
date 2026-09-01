// Package payment owns payment orders, verified provider events, vouchers, and
// external refund orchestration. It never trusts a browser success callback.
package payment

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/billing"
)

const (
	StatusCreated       = "created"
	StatusPending       = "pending"
	StatusPaid          = "paid"
	StatusFailed        = "failed"
	StatusExpired       = "expired"
	StatusRefundPending = "refund_pending"
	StatusRefunded      = "refunded"
	StatusClosed        = "closed"
)

const (
	EventPending       = "pending"
	EventPaid          = "paid"
	EventFailed        = "failed"
	EventExpired       = "expired"
	EventRefunded      = "refunded"
	EventRefundFailed  = "refund_failed"
	EventClosed        = "closed"
	EventDisputeOpened = "dispute_opened"
	EventDisputeWon    = "dispute_won"
	EventDisputeLost   = "dispute_lost"
	EventUnverified    = "unverified"
)

const (
	ProcessingPending    = "pending"
	ProcessingProcessing = "processing"
	ProcessingProcessed  = "processed"
	ProcessingRejected   = "rejected"
	ProcessingFailed     = "failed"
)

const (
	VoucherActive   = "active"
	VoucherRedeemed = "redeemed"
	VoucherRevoked  = "revoked"
	VoucherExpired  = "expired"
)

const (
	OperationCreate = "create"
	OperationRefund = "refund"

	OperationPending          = "pending"
	OperationProcessing       = "processing"
	OperationRetryable        = "retryable"
	OperationSucceeded        = "succeeded"
	OperationDefinitiveFailed = "definitive_failed"
)

const (
	VerificationWebhook  = "webhook_signature"
	VerificationProvider = "provider_api"
)

var (
	ErrInvalidInput        = errors.New("invalid payment input")
	ErrNotFound            = errors.New("payment order not found")
	ErrConflict            = errors.New("payment conflict")
	ErrInvalidTransition   = errors.New("invalid payment transition")
	ErrProviderUnavailable = errors.New("payment provider is unavailable")
	ErrProviderRejected    = errors.New("payment provider rejected the operation")
	ErrOperationPending    = errors.New("payment provider operation is already in progress")
	ErrWebhookInvalid      = errors.New("payment webhook verification failed")
	ErrWebhookIgnored      = errors.New("payment webhook event is not subscribed")
	ErrWebhookReplay       = errors.New("payment webhook replay conflict")
	ErrWebhookMismatch     = errors.New("payment webhook does not match order")
	ErrRefundPending       = errors.New("payment refund remains pending")
	ErrInsufficientFunds   = errors.New("wallet funds are insufficient for payment refund")
	ErrOrderLimit          = errors.New("too many active payment orders")
	ErrVoucherInvalid      = errors.New("payment voucher is invalid")
	ErrVoucherUnavailable  = errors.New("payment voucher is unavailable")
)

// Order is the persisted payment state. CheckoutData is provider-produced,
// allowlisted display data; it must never contain provider secrets.
type Order struct {
	ID                          uuid.UUID         `json:"id"`
	OrderNo                     string            `json:"order_no"`
	UserID                      uuid.UUID         `json:"user_id"`
	AmountMinor                 int64             `json:"amount_minor"`
	Currency                    string            `json:"currency"`
	PaymentProvider             string            `json:"payment_provider"`
	MerchantID                  string            `json:"merchant_id,omitempty"`
	ProviderLiveMode            *bool             `json:"provider_live_mode,omitempty"`
	ProviderTradeNo             string            `json:"provider_trade_no,omitempty"`
	ProviderRefundNo            string            `json:"provider_refund_no,omitempty"`
	ProviderPaymentIntentNo     string            `json:"provider_payment_intent_no,omitempty"`
	ProviderChargeNo            string            `json:"provider_charge_no,omitempty"`
	Status                      string            `json:"status"`
	IdempotencyKey              string            `json:"-"`
	CheckoutData                map[string]string `json:"checkout,omitempty"`
	FailureClass                string            `json:"failure_class,omitempty"`
	WalletID                    *uuid.UUID        `json:"wallet_id,omitempty"`
	RechargeLedgerID            *uuid.UUID        `json:"recharge_ledger_id,omitempty"`
	RefundHoldLedgerID          *uuid.UUID        `json:"refund_hold_ledger_id,omitempty"`
	RefundLedgerID              *uuid.UUID        `json:"refund_ledger_id,omitempty"`
	ExternalRefundedAmountMinor int64             `json:"external_refunded_amount_minor"`
	UpdatedBy                   *uuid.UUID        `json:"updated_by,omitempty"`
	ExpiresAt                   *time.Time        `json:"expires_at,omitempty"`
	PaidAt                      *time.Time        `json:"paid_at,omitempty"`
	RefundedAt                  *time.Time        `json:"refunded_at,omitempty"`
	ClosedAt                    *time.Time        `json:"closed_at,omitempty"`
	CreatedAt                   time.Time         `json:"created_at"`
	UpdatedAt                   time.Time         `json:"updated_at"`
}

// Event is one immutable provider notification plus its mutable processing state.
type Event struct {
	ID                      uuid.UUID  `json:"id"`
	PaymentProvider         string     `json:"payment_provider"`
	ProviderEventID         string     `json:"provider_event_id"`
	OrderID                 *uuid.UUID `json:"order_id,omitempty"`
	ProviderTradeNo         string     `json:"provider_trade_no,omitempty"`
	ProviderRefundNo        string     `json:"provider_refund_no,omitempty"`
	ProviderPaymentIntentNo string     `json:"provider_payment_intent_no,omitempty"`
	ProviderChargeNo        string     `json:"provider_charge_no,omitempty"`
	ProviderDisputeNo       string     `json:"provider_dispute_no,omitempty"`
	EventType               string     `json:"event_type"`
	AmountMinor             *int64     `json:"amount_minor,omitempty"`
	Currency                string     `json:"currency,omitempty"`
	MerchantID              string     `json:"merchant_id,omitempty"`
	ProviderLiveMode        *bool      `json:"provider_live_mode,omitempty"`
	SignatureVerified       bool       `json:"signature_verified"`
	VerificationSource      string     `json:"verification_source"`
	OccurredAt              *time.Time `json:"occurred_at,omitempty"`
	ReceivedAt              time.Time  `json:"received_at"`
	ProcessingStatus        string     `json:"processing_status"`
	ErrorClass              string     `json:"error_class,omitempty"`
}

// Voucher stores only a hash and non-secret prefix. Plaintext is returned once.
type Voucher struct {
	ID             uuid.UUID  `json:"id"`
	CodePrefix     string     `json:"code_prefix"`
	AmountMinor    int64      `json:"amount_minor"`
	Currency       string     `json:"currency"`
	Status         string     `json:"status"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	CreatedBy      uuid.UUID  `json:"created_by"`
	RedeemedBy     *uuid.UUID `json:"redeemed_by,omitempty"`
	RedeemedAt     *time.Time `json:"redeemed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CodeCiphertext []byte     `json:"-"`
	CodeNonce      []byte     `json:"-"`
	CodeHash       []byte     `json:"-"`
	CodeKeyVersion string     `json:"-"`
}

// CreatedVoucher contains the one-time plaintext redemption code.
type CreatedVoucher struct {
	Voucher Voucher `json:"voucher"`
	Code    string  `json:"code"`
}

type PageCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type OrderPage struct {
	Orders     []Order
	NextCursor *PageCursor
}

type CreateOrderInput struct {
	UserID          uuid.UUID
	AmountMinor     int64
	Currency        string
	PaymentProvider string
	IdempotencyKey  string
}

type ProviderOperation struct {
	ID                uuid.UUID
	PaymentOrderID    uuid.UUID
	OperationType     string
	PayloadSHA256     [32]byte
	MerchantID        string
	ProviderLiveMode  bool
	Status            string
	OwnerToken        *uuid.UUID
	LeaseExpiresAt    *time.Time
	NextAttemptAt     time.Time
	Attempts          int
	ProviderReference string
	LastErrorClass    string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ProviderOperationClaim struct {
	Order     Order
	Operation ProviderOperation
	Claimed   bool
}

type CreateOrderResult struct {
	Order    Order
	Checkout Checkout
}

type ManualRechargeInput struct {
	OperatorUserID uuid.UUID
	UserID         uuid.UUID
	AmountMinor    int64
	Currency       string
	RequestID      string
	IdempotencyKey string
}

type CreateVoucherInput struct {
	OperatorUserID uuid.UUID
	AmountMinor    int64
	IdempotencyKey string
	Currency       string
	ExpiresAt      *time.Time
	RequestID      string
}

type RedeemVoucherInput struct {
	UserID    uuid.UUID
	Code      string
	RequestID string
}

type RefundInput struct {
	OperatorUserID uuid.UUID
	OrderNo        string
	RequestID      string
}

type CloseInput struct {
	OperatorUserID uuid.UUID
	OrderNo        string
	RequestID      string
}

type WebhookResult struct {
	Event    Event
	Order    *Order
	Replayed bool
	Rejected bool
}

type DueProviderOperation struct {
	OrderID          uuid.UUID
	MerchantID       string
	ProviderLiveMode bool
	OperationType    string
	PayloadSHA256    [32]byte
}
type VoucherRedemption struct {
	Voucher Voucher
	Ledger  billing.LedgerEntry
}
