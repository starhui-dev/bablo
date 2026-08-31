package billing

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/usage"
)

func TestCalculateQuoteUsesExactDecimalAndRoundsAggregateUp(t *testing.T) {
	snapshot := pricing.Snapshot{
		VersionID: uuid.New(),
		Currency:  "USD",
		Prices: map[string]string{
			pricing.DimensionInputToken:  "0.000003",
			pricing.DimensionOutputToken: "0.000009",
		},
	}
	quote, err := calculateQuote(snapshot, usage.TokenUsage{InputTokens: 2, OutputTokens: 3})
	if err != nil {
		t.Fatalf("calculateQuote() error = %v", err)
	}
	if quote.AmountMinor != 1 || quote.Currency != "USD" || quote.PriceVersionID != snapshot.VersionID {
		t.Fatalf("quote = %+v, want one USD cent", quote)
	}
}

func TestCalculateQuoteSupportsRequestPriceAndCurrencyScale(t *testing.T) {
	snapshot := pricing.Snapshot{
		VersionID: uuid.New(),
		Currency:  "KWD",
		Prices:    map[string]string{pricing.DimensionRequest: "1.234567890123"},
	}
	quote, err := calculateQuote(snapshot, usage.TokenUsage{})
	if err != nil {
		t.Fatalf("calculateQuote() error = %v", err)
	}
	if quote.AmountMinor != 1235 {
		t.Fatalf("amount = %d, want 1235 fils", quote.AmountMinor)
	}
}

func TestCalculateQuoteFallsBackToBaseRatesForOptionalDimensions(t *testing.T) {
	snapshot := pricing.Snapshot{
		VersionID: uuid.New(),
		Currency:  "USD",
		Prices: map[string]string{
			pricing.DimensionInputToken:  "1",
			pricing.DimensionOutputToken: "1",
		},
	}
	quote, err := calculateQuote(snapshot, usage.TokenUsage{
		InputTokens: 1, OutputTokens: 1, CacheReadTokens: 1, ReasoningTokens: 1,
	})
	if err != nil {
		t.Fatalf("calculateQuote() error = %v", err)
	}
	if quote.AmountMinor != 200 {
		t.Fatalf("fallback amount = %d, want 200", quote.AmountMinor)
	}
}

func TestCalculateQuoteDoesNotDoubleChargeInclusiveBreakdowns(t *testing.T) {
	snapshot := pricing.Snapshot{
		VersionID: uuid.New(),
		Currency:  "USD",
		Prices: map[string]string{
			pricing.DimensionInputToken:     "1",
			pricing.DimensionOutputToken:    "1",
			pricing.DimensionCacheReadToken: "0.1",
			pricing.DimensionReasoningToken: "0.2",
		},
	}
	quote, err := calculateQuote(snapshot, usage.TokenUsage{
		InputTokens: 10, OutputTokens: 8, CacheReadTokens: 4, ReasoningTokens: 3,
	})
	if err != nil {
		t.Fatalf("calculateQuote() error = %v", err)
	}
	if quote.AmountMinor != 1200 {
		t.Fatalf("breakdown amount = %d, want 1200", quote.AmountMinor)
	}
}

func TestCalculateQuoteFreeSnapshotIsZero(t *testing.T) {
	snapshot := pricing.Snapshot{VersionID: uuid.New(), Currency: "JPY", Free: true}
	quote, err := calculateQuote(snapshot, usage.TokenUsage{InputTokens: math.MaxInt64})
	if err != nil {
		t.Fatalf("calculateQuote() error = %v", err)
	}
	if quote.AmountMinor != 0 || quote.Currency != "JPY" {
		t.Fatalf("free quote = %+v", quote)
	}
}

func TestCalculateQuoteRejectsUnrepresentablePrice(t *testing.T) {
	snapshot := pricing.Snapshot{
		VersionID: uuid.New(),
		Currency:  "USD",
		Prices: map[string]string{
			pricing.DimensionInputToken:  "0.0000000000001",
			pricing.DimensionOutputToken: "0",
		},
	}
	_, err := calculateQuote(snapshot, usage.TokenUsage{InputTokens: 1})
	if !errors.Is(err, ErrPriceMissing) {
		t.Fatalf("calculateQuote() error = %v, want ErrPriceMissing", err)
	}
}

func TestCalculateQuoteRejectsMinorUnitOverflow(t *testing.T) {
	snapshot := pricing.Snapshot{
		VersionID: uuid.New(),
		Currency:  "USD",
		Prices: map[string]string{
			pricing.DimensionInputToken:  "1",
			pricing.DimensionOutputToken: "0",
		},
	}
	_, err := calculateQuote(snapshot, usage.TokenUsage{InputTokens: math.MaxInt64})
	if !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("calculateQuote() error = %v, want ErrBalanceOverflow", err)
	}
}
