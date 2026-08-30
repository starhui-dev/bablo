// Package pricing owns exact versioned price snapshots selected after route resolution.
package pricing

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ScopeGlobal        = "global"
	ScopeModel         = "model"
	ScopeProviderModel = "provider_model"

	DimensionInputToken      = "input_token"
	DimensionOutputToken     = "output_token"
	DimensionCacheReadToken  = "cache_read_token"
	DimensionCacheWriteToken = "cache_write_token"
	DimensionReasoningToken  = "reasoning_token"
	DimensionRequest         = "request"

	StatusDraft   = "draft"
	StatusActive  = "active"
	StatusRetired = "retired"
)

var (
	ErrInvalidInput    = errors.New("invalid pricing input")
	ErrNotFound        = errors.New("price version not found")
	ErrConflict        = errors.New("price version conflict")
	ErrPriceMissing    = errors.New("required price is missing")
	ErrBillingDisabled = errors.New("model billing is disabled")

	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	amountPattern   = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})(\.[0-9]{1,12})?$`)
)

// Entry is one exact numeric dimension. UnitPrice is a decimal string so JSON
// and Go never round monetary values through float64.
type Entry struct {
	ID              uuid.UUID  `json:"id"`
	PricingScope    string     `json:"pricing_scope"`
	ModelID         *uuid.UUID `json:"model_id"`
	ProviderModelID *uuid.UUID `json:"provider_model_id"`
	Dimension       string     `json:"dimension"`
	UnitPrice       string     `json:"unit_price"`
}

// Version is a published or draft price bundle for one precedence scope.
type Version struct {
	ID            uuid.UUID  `json:"id"`
	Scope         string     `json:"scope"`
	VersionNo     int64      `json:"version_no"`
	Currency      string     `json:"currency"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	Status        string     `json:"status"`
	CreatedBy     uuid.UUID  `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	Prices        []Entry    `json:"prices"`
}

// EntryInput omits PricingScope because it must equal its parent version scope.
type EntryInput struct {
	ModelID         *uuid.UUID `json:"model_id"`
	ProviderModelID *uuid.UUID `json:"provider_model_id"`
	Dimension       string     `json:"dimension"`
	UnitPrice       string     `json:"unit_price"`
}

// CreateInput creates one immutable draft bundle.
type CreateInput struct {
	Scope         string       `json:"scope"`
	Currency      string       `json:"currency"`
	EffectiveFrom time.Time    `json:"effective_from"`
	EffectiveTo   *time.Time   `json:"effective_to"`
	Prices        []EntryInput `json:"prices"`
}

// Page is a version-number descending page within one required scope.
type Page struct {
	Versions   []Version
	NextCursor int64
}

// Snapshot is the exact price version bound to a resolved request target.
type Snapshot struct {
	VersionID     uuid.UUID         `json:"price_version_id"`
	Scope         string            `json:"scope"`
	Currency      string            `json:"currency"`
	EffectiveFrom time.Time         `json:"effective_from"`
	Prices        map[string]string `json:"prices"`
	Free          bool              `json:"free"`
}

func normalizeAmount(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !amountPattern.MatchString(value) {
		return "", ErrInvalidInput
	}
	if strings.Contains(value, ".") {
		value = strings.TrimRight(value, "0")
		value = strings.TrimSuffix(value, ".")
	}
	return value, nil
}

func validScope(value string) bool {
	switch value {
	case ScopeGlobal, ScopeModel, ScopeProviderModel:
		return true
	default:
		return false
	}
}

func validDimension(value string) bool {
	switch value {
	case DimensionInputToken, DimensionOutputToken, DimensionCacheReadToken,
		DimensionCacheWriteToken, DimensionReasoningToken, DimensionRequest:
		return true
	default:
		return false
	}
}
