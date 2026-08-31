// Package billing owns wallet reservations, immutable ledger entries, and usage settlement.
// Monetary values are always integer minor units; price inputs remain exact decimal strings.
package billing

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/usage"
)

const (
	ReservationNone              = "none"
	ReservationReserved          = "reserved"
	ReservationSettlementPending = "settlement_pending"
	ReservationSettled           = "settled"
	ReservationReleased          = "released"
)

const (
	SettlementPending         = "pending"
	SettlementSettled         = "settled"
	SettlementFailed          = "failed"
	SettlementReconcileNeeded = "reconcile_needed"
)

const (
	EntryReservation     = "reservation"
	EntryUsageCharge     = "usage_charge"
	EntryRelease         = "release"
	EntryRecharge        = "recharge"
	EntryRefund          = "refund"
	EntryAdjustment      = "adjustment"
	EntryAdminAdjustment = "admin_adjustment"
	EntryGrant           = "grant"
	EntryBonus           = "bonus"
	EntryExpiration      = "expiration"
)

var (
	ErrInvalidInput        = errors.New("invalid billing input")
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrWalletFrozen        = errors.New("wallet is not active")
	ErrInsufficientFunds   = errors.New("wallet has insufficient funds")
	ErrBudgetExceeded      = errors.New("billing budget exceeded")
	ErrReservationNotFound = errors.New("wallet reservation not found")
	ErrReservationConflict = errors.New("wallet reservation conflict")
	ErrSettlementPending   = errors.New("billing settlement is pending")
	ErrSettlementConflict  = errors.New("billing settlement conflict")
	ErrCurrencyMismatch    = errors.New("billing currency mismatch")
	ErrBalanceOverflow     = errors.New("wallet balance overflow")
	ErrPriceMissing        = errors.New("billing price is missing")
)

// Wallet is the transaction-maintained projection of its ledger.
type Wallet struct {
	ID                    uuid.UUID `json:"id"`
	UserID                uuid.UUID `json:"user_id"`
	Currency              string    `json:"currency"`
	AvailableBalanceMinor int64     `json:"available_balance_minor"`
	ReservedBalanceMinor  int64     `json:"reserved_balance_minor"`
	Status                string    `json:"status"`
	Version               int64     `json:"version"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// LedgerEntry is append-only. Delta fields are the authoritative movements;
// amount is the signed business amount for reporting and idempotency review.
type LedgerEntry struct {
	ID                         uuid.UUID  `json:"id"`
	WalletID                   uuid.UUID  `json:"wallet_id"`
	EntryType                  string     `json:"entry_type"`
	AmountMinor                int64      `json:"amount_minor"`
	AvailableDeltaMinor        int64      `json:"available_delta_minor"`
	ReservedDeltaMinor         int64      `json:"reserved_delta_minor"`
	AvailableBalanceAfterMinor int64      `json:"available_balance_after_minor"`
	ReservedBalanceAfterMinor  int64      `json:"reserved_balance_after_minor"`
	Currency                   string     `json:"currency"`
	ReferenceType              string     `json:"reference_type"`
	ReferenceID                string     `json:"reference_id"`
	IdempotencyKey             string     `json:"idempotency_key"`
	UsageEventID               *uuid.UUID `json:"usage_event_id,omitempty"`
	OperatorUserID             *uuid.UUID `json:"operator_user_id,omitempty"`
	Source                     string     `json:"source"`
	CreatedAt                  time.Time  `json:"created_at"`
}

// Reservation holds an upper bound before an upstream request starts.
type Reservation struct {
	ID                 uuid.UUID        `json:"id"`
	WalletID           uuid.UUID        `json:"wallet_id"`
	UserID             uuid.UUID        `json:"user_id"`
	APIKeyID           *uuid.UUID       `json:"api_key_id,omitempty"`
	RequestID          string           `json:"request_id"`
	RequestRecordID    *uuid.UUID       `json:"request_record_id,omitempty"`
	ModelID            *uuid.UUID       `json:"model_id,omitempty"`
	ProviderModelID    *uuid.UUID       `json:"provider_model_id,omitempty"`
	RouteVersionID     *uuid.UUID       `json:"route_version_id,omitempty"`
	ProviderID         *uuid.UUID       `json:"provider_id,omitempty"`
	CredentialID       *uuid.UUID       `json:"credential_id,omitempty"`
	PriceVersionID     uuid.UUID        `json:"price_version_id"`
	EstimatedUsage     usage.TokenUsage `json:"estimated_usage"`
	ReservationKey     string           `json:"reservation_key"`
	AmountMinor        int64            `json:"amount_minor"`
	Currency           string           `json:"currency"`
	Status             string           `json:"status"`
	SettledAmountMinor *int64           `json:"settled_amount_minor,omitempty"`
	UsageEventID       *uuid.UUID       `json:"usage_event_id,omitempty"`
	Reason             string           `json:"reason,omitempty"`
	ExpiresAt          *time.Time       `json:"expires_at,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// Quote is an exact integer-minor-unit amount. Estimated is set only for a
// provisional amount used when the provider did not return verifiable usage.
type Quote struct {
	PriceVersionID uuid.UUID `json:"price_version_id"`
	Currency       string    `json:"currency"`
	AmountMinor    int64     `json:"amount_minor"`
	Estimated      bool      `json:"estimated"`
}

// ReserveInput contains only Bablo domain data; no provider secret or CPA type.
type ReserveInput struct {
	UserID             uuid.UUID
	APIKeyID           uuid.UUID
	RequestID          string
	RequestRecordID    *uuid.UUID
	ModelID            *uuid.UUID
	ProviderModelID    *uuid.UUID
	RouteVersionID     *uuid.UUID
	ProviderID         *uuid.UUID
	CredentialID       *uuid.UUID
	Price              pricing.Snapshot
	EstimatedUsage     usage.TokenUsage
	DailyBudgetMinor   *int64
	MonthlyBudgetMinor *int64
	Reason             string
	ExpiresAt          *time.Time
}

// SettleInput closes one reservation against one immutable UsageEvent.
type SettleInput struct {
	ReservationID uuid.UUID
	Event         usage.Event
}

// ReleaseInput returns a reservation without charging it.
type ReleaseInput struct {
	ReservationID uuid.UUID
	Reason        string
}

// CreditInput appends a recharge, grant, refund, or administrative adjustment.
// Adjustment-like entries may be negative, but the wallet cannot cross zero.
type CreditInput struct {
	UserID         uuid.UUID
	Currency       string
	EntryType      string
	AmountMinor    int64
	ReferenceType  string
	ReferenceID    string
	IdempotencyKey string
	OperatorUserID *uuid.UUID
	Source         string
}

// Settlement is the durable outcome of a reservation settlement attempt.
type Settlement struct {
	ID                  uuid.UUID `json:"id"`
	ReservationID       uuid.UUID `json:"reservation_id"`
	UsageEventID        uuid.UUID `json:"usage_event_id"`
	IdempotencyKey      string    `json:"idempotency_key"`
	ReservedAmountMinor int64     `json:"reserved_amount_minor"`
	ActualAmountMinor   *int64    `json:"actual_amount_minor,omitempty"`
	Status              string    `json:"status"`
	Estimated           bool      `json:"estimated"`
	ErrorClass          string    `json:"error_class,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func validReservationStatus(value string) bool {
	switch value {
	case ReservationReserved, ReservationSettlementPending, ReservationSettled, ReservationReleased:
		return true
	default:
		return false
	}
}

func validSettlementStatus(value string) bool {
	switch value {
	case SettlementPending, SettlementSettled, SettlementFailed, SettlementReconcileNeeded:
		return true
	default:
		return false
	}
}

func validEntryType(value string) bool {
	switch value {
	case EntryReservation, EntryUsageCharge, EntryRelease, EntryRecharge, EntryRefund,
		EntryAdjustment, EntryAdminAdjustment, EntryGrant, EntryBonus, EntryExpiration:
		return true
	default:
		return false
	}
}

func normalizeText(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}
