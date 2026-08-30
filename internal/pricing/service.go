package pricing

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/model"
)

const (
	maximumPageSize        = 100
	maximumPricesPerBundle = 5_000
)

// Service owns exact decimal, publication, and fail-closed resolution policy.
type Service struct {
	repository *Repository
	now        func() time.Time
}

// NewService constructs a pricing service.
func NewService(repository *Repository) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalidInput
	}
	return &Service{repository: repository, now: time.Now}, nil
}

// Create validates and persists one immutable draft price bundle.
func (service *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Version, error) {
	if actorID == uuid.Nil {
		return Version{}, ErrInvalidInput
	}
	validated, err := validateCreate(input)
	if err != nil {
		return Version{}, err
	}
	return service.repository.create(ctx, actorID, validated, requestID)
}

// Get returns one complete price version.
func (service *Service) Get(ctx context.Context, versionID uuid.UUID) (Version, error) {
	if versionID == uuid.Nil {
		return Version{}, ErrInvalidInput
	}
	return service.repository.get(ctx, versionID)
}

// List requires a pricing scope and returns version numbers descending.
func (service *Service) List(ctx context.Context, scope string, cursor int64, limit int) (Page, error) {
	scope = strings.TrimSpace(scope)
	if !validScope(scope) || cursor < 0 {
		return Page{}, ErrInvalidInput
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > maximumPageSize {
		return Page{}, ErrInvalidInput
	}
	return service.repository.list(ctx, scope, cursor, limit)
}

// Activate publishes a draft. Published bundles cannot be edited; conflicting
// effective intervals must first be ended explicitly.
func (service *Service) Activate(ctx context.Context, actorID, versionID uuid.UUID, requestID string) (Version, error) {
	if actorID == uuid.Nil || versionID == uuid.Nil {
		return Version{}, ErrInvalidInput
	}
	version, err := service.repository.get(ctx, versionID)
	if err != nil {
		return Version{}, err
	}
	if version.Status == StatusActive {
		return version, nil
	}
	if version.Status != StatusDraft || !publishable(version) {
		return Version{}, ErrConflict
	}
	return service.repository.activate(ctx, actorID, versionID, requestID)
}

// Retire sets the exclusive end of a published interval. A future end keeps the
// version resolvable until that instant but prevents later mutation.
func (service *Service) Retire(ctx context.Context, actorID, versionID uuid.UUID, retiredAt time.Time, requestID string) (Version, error) {
	if actorID == uuid.Nil || versionID == uuid.Nil {
		return Version{}, ErrInvalidInput
	}
	if retiredAt.IsZero() {
		retiredAt = service.now().UTC()
	} else {
		retiredAt = retiredAt.UTC()
	}
	return service.repository.retire(ctx, actorID, versionID, retiredAt, requestID)
}

// ResolveSnapshot selects provider-model, model, then global precedence after a
// route target is resolved. Missing required dimensions fail closed.
func (service *Service) ResolveSnapshot(ctx context.Context, modelID uuid.UUID, providerModelID *uuid.UUID, at time.Time) (Snapshot, error) {
	if modelID == uuid.Nil || (providerModelID != nil && *providerModelID == uuid.Nil) {
		return Snapshot{}, ErrInvalidInput
	}
	if at.IsZero() {
		at = service.now().UTC()
	} else {
		at = at.UTC()
	}
	billingClass, err := service.repository.billingClass(ctx, modelID)
	if err != nil {
		return Snapshot{}, err
	}
	if billingClass == model.BillingDisabled {
		return Snapshot{}, ErrBillingDisabled
	}
	if billingClass == model.BillingFree {
		return Snapshot{Scope: model.BillingFree, Prices: map[string]string{}, Free: true}, nil
	}
	required := []string{DimensionRequest}
	if billingClass == model.BillingToken {
		required = []string{DimensionInputToken, DimensionOutputToken}
	}
	providerModelValue := uuid.Nil
	scopes := []string{}
	if providerModelID != nil {
		providerModelValue = *providerModelID
		scopes = append(scopes, ScopeProviderModel)
	}
	scopes = append(scopes, ScopeModel, ScopeGlobal)
	for _, scope := range scopes {
		snapshot, found, err := service.repository.resolve(ctx, scope, modelID, providerModelValue, at)
		if err != nil {
			return Snapshot{}, err
		}
		if !found {
			continue
		}
		for _, dimension := range required {
			if _, exists := snapshot.Prices[dimension]; !exists {
				return Snapshot{}, ErrPriceMissing
			}
		}
		return snapshot, nil
	}
	return Snapshot{}, ErrPriceMissing
}

func validateCreate(input CreateInput) (CreateInput, error) {
	input.Scope = strings.TrimSpace(input.Scope)
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	if !validScope(input.Scope) || !currencyPattern.MatchString(input.Currency) || input.EffectiveFrom.IsZero() {
		return CreateInput{}, ErrInvalidInput
	}
	input.EffectiveFrom = input.EffectiveFrom.UTC()
	if input.EffectiveTo != nil {
		value := input.EffectiveTo.UTC()
		if !value.After(input.EffectiveFrom) {
			return CreateInput{}, ErrInvalidInput
		}
		input.EffectiveTo = &value
	}
	if len(input.Prices) == 0 || len(input.Prices) > maximumPricesPerBundle {
		return CreateInput{}, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(input.Prices))
	for index := range input.Prices {
		entry := &input.Prices[index]
		entry.Dimension = strings.TrimSpace(entry.Dimension)
		if !validDimension(entry.Dimension) {
			return CreateInput{}, ErrInvalidInput
		}
		amount, err := normalizeAmount(entry.UnitPrice)
		if err != nil {
			return CreateInput{}, err
		}
		entry.UnitPrice = amount
		var target string
		switch input.Scope {
		case ScopeGlobal:
			if entry.ModelID != nil || entry.ProviderModelID != nil {
				return CreateInput{}, ErrInvalidInput
			}
			target = "global"
		case ScopeModel:
			if entry.ModelID == nil || *entry.ModelID == uuid.Nil || entry.ProviderModelID != nil {
				return CreateInput{}, ErrInvalidInput
			}
			target = entry.ModelID.String()
		case ScopeProviderModel:
			if entry.ProviderModelID == nil || *entry.ProviderModelID == uuid.Nil || entry.ModelID != nil {
				return CreateInput{}, ErrInvalidInput
			}
			target = entry.ProviderModelID.String()
		}
		key := target + "\x00" + entry.Dimension
		if _, exists := seen[key]; exists {
			return CreateInput{}, ErrInvalidInput
		}
		seen[key] = struct{}{}
	}
	sort.Slice(input.Prices, func(left, right int) bool {
		return priceSortKey(input.Prices[left]) < priceSortKey(input.Prices[right])
	})
	return input, nil
}

func publishable(version Version) bool {
	if len(version.Prices) == 0 {
		return false
	}
	dimensionsByTarget := make(map[string]map[string]struct{})
	for _, entry := range version.Prices {
		target := "global"
		if entry.ModelID != nil {
			target = entry.ModelID.String()
		}
		if entry.ProviderModelID != nil {
			target = entry.ProviderModelID.String()
		}
		if dimensionsByTarget[target] == nil {
			dimensionsByTarget[target] = make(map[string]struct{})
		}
		dimensionsByTarget[target][entry.Dimension] = struct{}{}
	}
	for _, dimensions := range dimensionsByTarget {
		_, requestPriced := dimensions[DimensionRequest]
		_, inputPriced := dimensions[DimensionInputToken]
		_, outputPriced := dimensions[DimensionOutputToken]
		if !requestPriced && !(inputPriced && outputPriced) {
			return false
		}
	}
	return true
}

func priceSortKey(entry EntryInput) string {
	target := "global"
	if entry.ModelID != nil {
		target = entry.ModelID.String()
	}
	if entry.ProviderModelID != nil {
		target = entry.ProviderModelID.String()
	}
	return target + "\x00" + entry.Dimension + "\x00" + strconv.Itoa(len(entry.UnitPrice))
}
