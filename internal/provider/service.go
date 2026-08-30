package provider

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/model"
)

const maximumPageSize = 100

// Service owns provider resource policy and discovery/review invariants.
type Service struct {
	repository *Repository
	now        func() time.Time
}

// NewService constructs a provider catalog service.
func NewService(repository *Repository) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalidInput
	}
	return &Service{repository: repository, now: time.Now}, nil
}

// Create validates and persists provider metadata.
func (service *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Provider, error) {
	if actorID == uuid.Nil {
		return Provider{}, ErrInvalidInput
	}
	validated, err := validateCreate(input)
	if err != nil {
		return Provider{}, err
	}
	return service.repository.create(ctx, actorID, validated, requestID)
}

// Update patches resource policy. Subscription resources remain non-commercial
// in P0; a future legal/business decision must explicitly change that policy.
func (service *Service) Update(ctx context.Context, actorID, providerID uuid.UUID, input UpdateInput, requestID string) (Provider, error) {
	if actorID == uuid.Nil || providerID == uuid.Nil || emptyUpdate(input) {
		return Provider{}, ErrInvalidInput
	}
	current, err := service.repository.get(ctx, providerID)
	if err != nil {
		return Provider{}, err
	}
	if input.Slug != nil {
		value, err := normalizeSlug(*input.Slug)
		if err != nil {
			return Provider{}, err
		}
		input.Slug = &value
	}
	if input.DisplayName != nil {
		value, err := normalizeDisplayName(*input.DisplayName)
		if err != nil {
			return Provider{}, err
		}
		input.DisplayName = &value
	}
	if input.ResourceType != nil {
		value := strings.TrimSpace(*input.ResourceType)
		if !validResourceType(value) {
			return Provider{}, ErrInvalidInput
		}
		input.ResourceType = &value
		current.ResourceType = value
	}
	if input.CommercialAllowed != nil {
		current.CommercialAllowed = *input.CommercialAllowed
	}
	if current.ResourceType == ResourceSubscription && current.CommercialAllowed {
		return Provider{}, ErrInvalidInput
	}
	return service.repository.update(ctx, actorID, providerID, input, requestID)
}

// Get returns one provider.
func (service *Service) Get(ctx context.Context, providerID uuid.UUID) (Provider, error) {
	if providerID == uuid.Nil {
		return Provider{}, ErrInvalidInput
	}
	return service.repository.get(ctx, providerID)
}

// List returns a stable provider page.
func (service *Service) List(ctx context.Context, cursor string, limit int) (Page, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		var err error
		cursor, err = normalizeSlug(cursor)
		if err != nil {
			return Page{}, err
		}
	}
	limit, err := validateLimit(limit)
	if err != nil {
		return Page{}, err
	}
	return service.repository.list(ctx, cursor, limit)
}

// CreateModel manually maps and approves one upstream provider model.
func (service *Service) CreateModel(ctx context.Context, actorID uuid.UUID, input CreateModelInput, requestID string) (ProviderModel, error) {
	if actorID == uuid.Nil || input.ProviderID == uuid.Nil || input.ModelID == uuid.Nil {
		return ProviderModel{}, ErrInvalidInput
	}
	upstreamID, err := normalizeUpstreamModelID(input.UpstreamModelID)
	if err != nil {
		return ProviderModel{}, err
	}
	input.UpstreamModelID = upstreamID
	input.Protocol = strings.TrimSpace(input.Protocol)
	if !validProtocol(input.Protocol) || !capabilitiesMatchProtocol(input.Protocol, input.Capabilities) {
		return ProviderModel{}, ErrInvalidInput
	}
	publicCapabilities, err := service.repository.modelCapabilities(ctx, input.ModelID)
	if err != nil {
		return ProviderModel{}, err
	}
	if !publicCapabilities.Supports(input.Capabilities) {
		return ProviderModel{}, ErrInvalidInput
	}
	return service.repository.createModel(ctx, actorID, input, requestID)
}

// UpdateModel applies administrator review without allowing discovery status to
// become a business-control input.
func (service *Service) UpdateModel(ctx context.Context, actorID, providerModelID uuid.UUID, input UpdateModelInput, requestID string) (ProviderModel, error) {
	if actorID == uuid.Nil || providerModelID == uuid.Nil || emptyModelUpdate(input) {
		return ProviderModel{}, ErrInvalidInput
	}
	current, err := service.repository.getModel(ctx, providerModelID)
	if err != nil {
		return ProviderModel{}, err
	}
	if input.ModelID.Set {
		if input.ModelID.Value != nil && *input.ModelID.Value == uuid.Nil {
			return ProviderModel{}, ErrInvalidInput
		}
		current.ModelID = input.ModelID.Value
	}
	if input.Protocol != nil {
		value := strings.TrimSpace(*input.Protocol)
		if !validProtocol(value) {
			return ProviderModel{}, ErrInvalidInput
		}
		input.Protocol = &value
		current.Protocol = value
	}
	if input.Capabilities != nil {
		if !input.Capabilities.Valid() {
			return ProviderModel{}, ErrInvalidInput
		}
		current.Capabilities = *input.Capabilities
	}
	if !capabilitiesMatchProtocol(current.Protocol, current.Capabilities) {
		return ProviderModel{}, ErrInvalidInput
	}
	if input.ReviewStatus != nil {
		value := strings.TrimSpace(*input.ReviewStatus)
		if !validReviewStatus(value) {
			return ProviderModel{}, ErrInvalidInput
		}
		input.ReviewStatus = &value
		current.ReviewStatus = value
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if current.ReviewStatus == ReviewRejected {
		current.Enabled = false
	}
	if current.ReviewStatus == ReviewApproved && current.ModelID == nil {
		return ProviderModel{}, ErrInvalidInput
	}
	if current.Enabled && current.ReviewStatus != ReviewApproved {
		return ProviderModel{}, ErrInvalidInput
	}
	if current.ModelID != nil {
		publicCapabilities, err := service.repository.modelCapabilities(ctx, *current.ModelID)
		if err != nil {
			return ProviderModel{}, err
		}
		if !publicCapabilities.Supports(current.Capabilities) {
			return ProviderModel{}, ErrInvalidInput
		}
	}
	return service.repository.updateModel(ctx, actorID, providerModelID, input, requestID)
}

// GetModel returns one provider model.
func (service *Service) GetModel(ctx context.Context, providerModelID uuid.UUID) (ProviderModel, error) {
	if providerModelID == uuid.Nil {
		return ProviderModel{}, ErrInvalidInput
	}
	return service.repository.getModel(ctx, providerModelID)
}

// ListModels requires a provider scope to avoid an unbounded cross-provider scan.
func (service *Service) ListModels(ctx context.Context, providerID uuid.UUID, cursor string, limit int) (ModelPage, error) {
	if providerID == uuid.Nil {
		return ModelPage{}, ErrInvalidInput
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		var err error
		cursor, err = normalizeUpstreamModelID(cursor)
		if err != nil {
			return ModelPage{}, err
		}
	}
	limit, err := validateLimit(limit)
	if err != nil {
		return ModelPage{}, err
	}
	return service.repository.listModels(ctx, providerID, cursor, limit)
}

// Reconcile records a complete successful discovery snapshot. It never maps a
// new model, approves it, or changes approved enablement.
func (service *Service) Reconcile(ctx context.Context, actorID, providerID uuid.UUID, discoveries []Discovery, requestID string) (ReconcileResult, error) {
	if actorID == uuid.Nil || providerID == uuid.Nil || len(discoveries) > 10_000 {
		return ReconcileResult{}, ErrInvalidInput
	}
	normalized := make([]Discovery, 0, len(discoveries))
	seen := make(map[string]Discovery, len(discoveries))
	for _, discovery := range discoveries {
		upstreamID, err := normalizeUpstreamModelID(discovery.UpstreamModelID)
		if err != nil {
			return ReconcileResult{}, err
		}
		discovery.UpstreamModelID = upstreamID
		discovery.Protocol = strings.TrimSpace(discovery.Protocol)
		if !validProtocol(discovery.Protocol) || !capabilitiesMatchProtocol(discovery.Protocol, discovery.Capabilities) {
			return ReconcileResult{}, ErrInvalidInput
		}
		if existing, exists := seen[upstreamID]; exists {
			if !reflect.DeepEqual(existing, discovery) {
				return ReconcileResult{}, ErrInvalidInput
			}
			continue
		}
		seen[upstreamID] = discovery
		normalized = append(normalized, discovery)
	}
	sort.Slice(normalized, func(left, right int) bool {
		return normalized[left].UpstreamModelID < normalized[right].UpstreamModelID
	})
	return service.repository.reconcile(ctx, actorID, providerID, normalized, service.now().UTC(), requestID)
}

func validateCreate(input CreateInput) (CreateInput, error) {
	slug, err := normalizeSlug(input.Slug)
	if err != nil {
		return CreateInput{}, err
	}
	displayName, err := normalizeDisplayName(input.DisplayName)
	if err != nil {
		return CreateInput{}, err
	}
	input.Slug = slug
	input.DisplayName = displayName
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	if !validResourceType(input.ResourceType) {
		return CreateInput{}, ErrInvalidInput
	}
	if input.ResourceType == ResourceSubscription && input.CommercialAllowed {
		return CreateInput{}, ErrInvalidInput
	}
	return input, nil
}

func capabilitiesMatchProtocol(protocol string, capabilities model.Capabilities) bool {
	if !capabilities.Valid() {
		return false
	}
	switch protocol {
	case ProtocolOpenAIChat:
		return capabilities.Chat
	case ProtocolOpenAIResponses:
		return capabilities.Responses
	case ProtocolClaudeMessages:
		return capabilities.Messages
	case ProtocolGemini, ProtocolCustom:
		return true
	default:
		return false
	}
}

func validateLimit(limit int) (int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > maximumPageSize {
		return 0, ErrInvalidInput
	}
	return limit, nil
}

func emptyUpdate(input UpdateInput) bool {
	return input.Slug == nil && input.DisplayName == nil && input.ResourceType == nil &&
		input.CommercialAllowed == nil && input.Enabled == nil
}

func emptyModelUpdate(input UpdateModelInput) bool {
	return !input.ModelID.Set && input.Protocol == nil && input.Capabilities == nil && input.Enabled == nil && input.ReviewStatus == nil
}
