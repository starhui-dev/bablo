package billing

import (
	"math"
	"math/big"
	"strings"

	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/usage"
)

const priceScale = 12

var priceScaleBase = new(big.Int).Exp(big.NewInt(10), big.NewInt(priceScale), nil)

// DefaultCurrencyMinorUnits returns the ISO-style minor-unit scale used by P0.
// Unknown currencies use two decimals; changing this policy requires a billing
// compatibility decision because ledger amounts are already persisted.
func DefaultCurrencyMinorUnits(currency string) int {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "BHD", "IQD", "JOD", "KWD", "OMR", "TND":
		return 3
	case "CLP", "ISK", "JPY", "KRW", "PYG", "UGX", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	default:
		return 2
	}
}

func calculateQuote(snapshot pricing.Snapshot, observed usage.TokenUsage) (Quote, error) {
	currency := strings.ToUpper(strings.TrimSpace(snapshot.Currency))
	if snapshot.Free {
		return Quote{PriceVersionID: snapshot.VersionID, Currency: currency}, nil
	}
	if snapshot.VersionID == [16]byte{} || len(snapshot.Prices) == 0 || len(currency) != 3 {
		return Quote{}, ErrPriceMissing
	}
	for _, count := range []int64{
		observed.InputTokens,
		observed.OutputTokens,
		observed.CacheReadTokens,
		observed.CacheWriteTokens,
		observed.ReasoningTokens,
	} {
		if count < 0 {
			return Quote{}, ErrInvalidInput
		}
	}

	total := new(big.Int)
	if requestPrice, ok := snapshot.Prices[pricing.DimensionRequest]; ok {
		if err := addPricedUnits(total, requestPrice, 1); err != nil {
			return Quote{}, err
		}
	} else {
		inputPrice, inputOK := snapshot.Prices[pricing.DimensionInputToken]
		outputPrice, outputOK := snapshot.Prices[pricing.DimensionOutputToken]
		if !inputOK || !outputOK {
			return Quote{}, ErrPriceMissing
		}
		inputTokens := observed.InputTokens
		outputTokens := observed.OutputTokens

		if observed.CacheReadTokens > 0 {
			if cachePrice, ok := snapshot.Prices[pricing.DimensionCacheReadToken]; ok {
				// OpenAI totals include cached input. If the cache count is larger
				// than input, treat it as a disjoint provider counter instead.
				if observed.CacheReadTokens <= inputTokens {
					inputTokens -= observed.CacheReadTokens
				}
				if err := addPricedUnits(total, cachePrice, observed.CacheReadTokens); err != nil {
					return Quote{}, err
				}
			} else if observed.CacheReadTokens > inputTokens {
				// A disjoint cache counter without a dedicated rate falls back to
				// the ordinary input rate rather than becoming free.
				if err := addPricedUnits(total, inputPrice, observed.CacheReadTokens); err != nil {
					return Quote{}, err
				}
			}
		}
		if observed.CacheWriteTokens > 0 {
			cachePrice := inputPrice
			if configured, ok := snapshot.Prices[pricing.DimensionCacheWriteToken]; ok {
				cachePrice = configured
			}
			if err := addPricedUnits(total, cachePrice, observed.CacheWriteTokens); err != nil {
				return Quote{}, err
			}
		}
		if observed.ReasoningTokens > 0 {
			if reasoningPrice, ok := snapshot.Prices[pricing.DimensionReasoningToken]; ok {
				// OpenAI output totals include reasoning tokens. A larger reasoning
				// count is treated as a disjoint provider counter.
				if observed.ReasoningTokens <= outputTokens {
					outputTokens -= observed.ReasoningTokens
				}
				if err := addPricedUnits(total, reasoningPrice, observed.ReasoningTokens); err != nil {
					return Quote{}, err
				}
			} else if observed.ReasoningTokens > outputTokens {
				if err := addPricedUnits(total, outputPrice, observed.ReasoningTokens); err != nil {
					return Quote{}, err
				}
			}
		}
		if err := addPricedUnits(total, inputPrice, inputTokens); err != nil {
			return Quote{}, err
		}
		if err := addPricedUnits(total, outputPrice, outputTokens); err != nil {
			return Quote{}, err
		}
	}

	amount, err := scaledToMinor(total, DefaultCurrencyMinorUnits(currency))
	if err != nil {
		return Quote{}, err
	}
	return Quote{
		PriceVersionID: snapshot.VersionID,
		Currency:       currency,
		AmountMinor:    amount,
	}, nil
}

func addPricedUnits(total *big.Int, price string, count int64) error {
	if count < 0 {
		return ErrInvalidInput
	}
	scaled, err := parseScaledPrice(price)
	if err != nil {
		return ErrPriceMissing
	}
	if count == 0 || scaled.Sign() == 0 {
		return nil
	}
	product := new(big.Int).Mul(scaled, big.NewInt(count))
	total.Add(total, product)
	return nil
}

func parseScaledPrice(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		return nil, ErrPriceMissing
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return nil, ErrPriceMissing
	}
	if len(parts[0]) > 18 {
		return nil, ErrPriceMissing
	}
	for _, character := range parts[0] {
		if character < '0' || character > '9' {
			return nil, ErrPriceMissing
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if len(fraction) > priceScale {
			return nil, ErrPriceMissing
		}
		for _, character := range fraction {
			if character < '0' || character > '9' {
				return nil, ErrPriceMissing
			}
		}
	}
	fraction += strings.Repeat("0", priceScale-len(fraction))
	combined := strings.TrimLeft(parts[0]+fraction, "0")
	if combined == "" {
		combined = "0"
	}
	result, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, ErrPriceMissing
	}
	return result, nil
}

func scaledToMinor(total *big.Int, minorUnits int) (int64, error) {
	if minorUnits < 0 || minorUnits > 9 {
		return 0, ErrInvalidInput
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(minorUnits)), nil)
	numerator := new(big.Int).Mul(total, factor)
	quotient, remainder := new(big.Int).QuoRem(numerator, priceScaleBase, new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if quotient.Sign() < 0 || !quotient.IsInt64() || quotient.Cmp(big.NewInt(math.MaxInt64)) > 0 {
		return 0, ErrBalanceOverflow
	}
	return quotient.Int64(), nil
}
