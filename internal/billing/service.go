package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/usage"
)

const (
	defaultOutputReserveTokens int64 = 4096
	maximumOutputReserveTokens int64 = 10_000_000
)

// Options controls deterministic P0 reservation policy.
type Options struct {
	DefaultOutputTokens int64
	Now                 func() time.Time
}

// Service validates price, budget, and wallet operations before repository writes.
type Service struct {
	repository          *Repository
	defaultOutputTokens int64
	now                 func() time.Time
}

// NewService constructs the wallet and settlement service.
func NewService(repository *Repository, options ...Options) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("billing service requires a repository")
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("billing service accepts at most one options value")
	}
	config := Options{DefaultOutputTokens: defaultOutputReserveTokens, Now: time.Now}
	if len(options) == 1 {
		if options[0].DefaultOutputTokens != 0 {
			config.DefaultOutputTokens = options[0].DefaultOutputTokens
		}
		if options[0].Now != nil {
			config.Now = options[0].Now
		}
	}
	if config.DefaultOutputTokens < 0 || config.DefaultOutputTokens > maximumOutputReserveTokens {
		return nil, fmt.Errorf("billing default output tokens are invalid")
	}
	return &Service{
		repository:          repository,
		defaultOutputTokens: config.DefaultOutputTokens,
		now:                 func() time.Time { return config.Now().UTC() },
	}, nil
}

// Quote converts an immutable price snapshot and verified usage into an exact
// minor-unit charge. The final aggregate is rounded up once, never through float.
func (s *Service) Quote(snapshot pricing.Snapshot, observed usage.TokenUsage) (Quote, error) {
	if s == nil {
		return Quote{}, ErrInvalidInput
	}
	return calculateQuote(snapshot, observed)
}

// Reserve checks API-key budgets and moves spendable wallet balance into a hold
// before the upstream request starts.
func (s *Service) Reserve(ctx context.Context, input ReserveInput) (Reservation, error) {
	if s == nil || s.repository == nil {
		return Reservation{}, fmt.Errorf("%w: billing service is not initialized", ErrInvalidInput)
	}
	normalized, err := normalizeReserveInput(input, s.defaultOutputTokens, s.now())
	if err != nil {
		return Reservation{}, err
	}
	quote, err := s.Quote(normalized.Price, normalized.EstimatedUsage)
	if err != nil {
		return Reservation{}, err
	}
	if quote.AmountMinor == 0 {
		now := s.now()
		return Reservation{
			UserID:          normalized.UserID,
			APIKeyID:        uuidPointer(normalized.APIKeyID),
			RequestID:       normalized.RequestID,
			RequestRecordID: cloneUUID(normalized.RequestRecordID),
			ModelID:         cloneUUID(normalized.ModelID),
			ProviderModelID: cloneUUID(normalized.ProviderModelID),
			RouteVersionID:  cloneUUID(normalized.RouteVersionID),
			ProviderID:      cloneUUID(normalized.ProviderID),
			CredentialID:    cloneUUID(normalized.CredentialID),
			PriceVersionID:  quote.PriceVersionID,
			EstimatedUsage:  normalized.EstimatedUsage,
			ReservationKey:  reservationKey(normalized.RequestID),
			Currency:        quote.Currency,
			Status:          ReservationNone,
			CreatedAt:       now,
			UpdatedAt:       now,
		}, nil
	}
	return s.repository.Reserve(ctx, normalized, quote, s.now())
}

// Settle consumes a reservation and appends usage_charge/release ledger entries.
// A pending result is durable and returned with ErrSettlementPending so callers
// can fail closed without losing the retry state.
func (s *Service) Settle(ctx context.Context, input SettleInput) (Settlement, error) {
	if s == nil || s.repository == nil {
		return Settlement{}, fmt.Errorf("%w: billing service is not initialized", ErrInvalidInput)
	}
	if input.ReservationID == uuid.Nil || input.Event.ID == uuid.Nil || input.Event.RequestID == "" || input.Event.AmountMinor == nil || *input.Event.AmountMinor < 0 {
		return Settlement{}, ErrInvalidInput
	}
	if input.Event.PriceVersionID == nil || *input.Event.PriceVersionID == uuid.Nil {
		return Settlement{}, ErrInvalidInput
	}
	if input.Event.Currency != strings.ToUpper(strings.TrimSpace(input.Event.Currency)) || len(input.Event.Currency) != 3 {
		return Settlement{}, ErrInvalidInput
	}
	return s.repository.Settle(ctx, input, s.now())
}

// Release returns a hold when no upstream-billable attempt occurred.
func (s *Service) Release(ctx context.Context, input ReleaseInput) error {
	if s == nil || s.repository == nil {
		return fmt.Errorf("%w: billing service is not initialized", ErrInvalidInput)
	}
	if input.ReservationID == uuid.Nil {
		return nil
	}
	input.Reason = normalizeText(input.Reason, 128)
	if input.Reason == "" {
		input.Reason = "request_not_charged"
	}
	return s.repository.Release(ctx, input, s.now())
}

// Credit appends one funding/refund/adjustment fact and updates the wallet projection.
func (s *Service) Credit(ctx context.Context, input CreditInput) (LedgerEntry, error) {
	if s == nil || s.repository == nil {
		return LedgerEntry{}, fmt.Errorf("%w: billing service is not initialized", ErrInvalidInput)
	}
	normalized, err := normalizeCreditInput(input)
	if err != nil {
		return LedgerEntry{}, err
	}
	return s.repository.Credit(ctx, normalized, s.now())
}

// CreditInTx validates and appends a wallet credit/debit inside a transaction
// owned by another Bablo financial domain.
func (s *Service) CreditInTx(ctx context.Context, q data.Querier, input CreditInput) (LedgerEntry, error) {
	if s == nil || s.repository == nil || q == nil {
		return LedgerEntry{}, fmt.Errorf("%w: billing service is not initialized", ErrInvalidInput)
	}
	normalized, err := normalizeCreditInput(input)
	if err != nil {
		return LedgerEntry{}, err
	}
	return s.repository.CreditInTx(ctx, q, normalized, s.now())
}

// GetWallet returns the current wallet projection without creating one.
func (s *Service) GetWallet(ctx context.Context, userID uuid.UUID, currency string) (Wallet, error) {
	if s == nil || s.repository == nil || userID == uuid.Nil {
		return Wallet{}, ErrInvalidInput
	}
	currency = normalizeCurrency(currency)
	if currency == "" {
		return Wallet{}, ErrInvalidInput
	}
	return s.repository.GetWallet(ctx, userID, currency)
}

// RebuildBalance sums immutable ledger deltas for reconciliation and recovery checks.
func (s *Service) RebuildBalance(ctx context.Context, walletID uuid.UUID) (int64, int64, error) {
	if s == nil || s.repository == nil || walletID == uuid.Nil {
		return 0, 0, ErrInvalidInput
	}
	return s.repository.RebuildBalance(ctx, walletID)
}

func normalizeReserveInput(input ReserveInput, defaultOutput int64, now time.Time) (ReserveInput, error) {
	if input.UserID == uuid.Nil || input.APIKeyID == uuid.Nil {
		return ReserveInput{}, ErrInvalidInput
	}
	if input.RequestID != strings.TrimSpace(input.RequestID) || normalizeText(input.RequestID, 128) == "" {
		return ReserveInput{}, ErrInvalidInput
	}
	if input.EstimatedUsage.InputTokens < 0 || input.EstimatedUsage.OutputTokens < 0 || input.EstimatedUsage.CacheReadTokens < 0 || input.EstimatedUsage.CacheWriteTokens < 0 || input.EstimatedUsage.ReasoningTokens < 0 {
		return ReserveInput{}, ErrInvalidInput
	}
	if input.EstimatedUsage.OutputTokens == 0 && !input.Price.Free {
		input.EstimatedUsage.OutputTokens = defaultOutput
	}
	if input.EstimatedUsage.OutputTokens > maximumOutputReserveTokens {
		return ReserveInput{}, ErrInvalidInput
	}
	if input.DailyBudgetMinor != nil && *input.DailyBudgetMinor < 0 {
		return ReserveInput{}, ErrInvalidInput
	}
	if input.MonthlyBudgetMinor != nil && *input.MonthlyBudgetMinor < 0 {
		return ReserveInput{}, ErrInvalidInput
	}
	input.RequestRecordID = cloneUUID(input.RequestRecordID)
	input.ModelID = cloneUUID(input.ModelID)
	input.ProviderModelID = cloneUUID(input.ProviderModelID)
	input.RouteVersionID = cloneUUID(input.RouteVersionID)
	input.ProviderID = cloneUUID(input.ProviderID)
	input.CredentialID = cloneUUID(input.CredentialID)
	input.DailyBudgetMinor = cloneInt64(input.DailyBudgetMinor)
	input.MonthlyBudgetMinor = cloneInt64(input.MonthlyBudgetMinor)
	input.Reason = normalizeText(input.Reason, 128)
	if input.Reason == "" {
		input.Reason = "inference_budget_hold"
	}
	if input.ExpiresAt != nil {
		value := input.ExpiresAt.UTC()
		if !value.After(now) {
			return ReserveInput{}, ErrInvalidInput
		}
		input.ExpiresAt = &value
	}
	return input, nil
}

func normalizeCreditInput(input CreditInput) (CreditInput, error) {
	if input.UserID == uuid.Nil || !validEntryType(input.EntryType) {
		return CreditInput{}, ErrInvalidInput
	}
	switch input.EntryType {
	case EntryRecharge, EntryRefund, EntryGrant, EntryBonus:
		if input.AmountMinor <= 0 {
			return CreditInput{}, ErrInvalidInput
		}
	case EntryAdjustment, EntryAdminAdjustment, EntryExpiration:
		if input.AmountMinor == 0 {
			return CreditInput{}, ErrInvalidInput
		}
	default:
		return CreditInput{}, ErrInvalidInput
	}
	input.Currency = normalizeCurrency(input.Currency)
	input.ReferenceType = normalizeText(input.ReferenceType, 64)
	input.ReferenceID = normalizeText(input.ReferenceID, 160)
	input.IdempotencyKey = normalizeText(input.IdempotencyKey, 160)
	if input.RequestID != "" {
		input.RequestID = normalizeText(input.RequestID, 160)
		if input.RequestID == "" {
			return CreditInput{}, ErrInvalidInput
		}
	}
	input.Source = normalizeText(input.Source, 64)
	if input.Currency == "" || input.ReferenceType == "" || input.ReferenceID == "" || input.IdempotencyKey == "" || input.Source == "" {
		return CreditInput{}, ErrInvalidInput
	}
	input.OperatorUserID = cloneUUID(input.OperatorUserID)
	if input.EntryType == EntryAdminAdjustment && input.OperatorUserID == nil {
		return CreditInput{}, ErrInvalidInput
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

func reservationKey(requestID string) string {
	return "billing:v1:" + requestID
}

func settlementKey(eventID uuid.UUID) string {
	return "billing-settle:v1:" + eventID.String()
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	copy := value
	return &copy
}

func cloneUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
