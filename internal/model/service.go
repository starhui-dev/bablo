package model

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

const maximumPageSize = 100

// Service owns model identifier, alias, capability, and visibility validation.
type Service struct {
	repository *Repository
}

// NewService constructs a model catalog service.
func NewService(repository *Repository) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalidInput
	}
	return &Service{repository: repository}, nil
}

// Create validates and persists one administrator-owned public model.
func (service *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Model, error) {
	if actorID == uuid.Nil {
		return Model{}, ErrInvalidInput
	}
	validated, err := validateCreate(input)
	if err != nil {
		return Model{}, err
	}
	return service.repository.create(ctx, actorID, validated, requestID)
}

// Update patches one model without reassigning removed aliases.
func (service *Service) Update(ctx context.Context, actorID, modelID uuid.UUID, input UpdateInput, requestID string) (Model, error) {
	if actorID == uuid.Nil || modelID == uuid.Nil || emptyUpdate(input) {
		return Model{}, ErrInvalidInput
	}
	current, err := service.repository.get(ctx, modelID)
	if err != nil {
		return Model{}, err
	}
	if input.PublicID != nil {
		value, err := normalizeIdentifier(*input.PublicID)
		if err != nil {
			return Model{}, err
		}
		input.PublicID = &value
		current.PublicID = value
	}
	if input.DisplayName != nil {
		value, err := normalizeDisplayName(*input.DisplayName)
		if err != nil {
			return Model{}, err
		}
		input.DisplayName = &value
	}
	if input.Visibility != nil {
		value := strings.TrimSpace(*input.Visibility)
		if !validVisibility(value) {
			return Model{}, ErrInvalidInput
		}
		input.Visibility = &value
	}
	if input.BillingClass != nil {
		value := strings.TrimSpace(*input.BillingClass)
		if !validBillingClass(value) {
			return Model{}, ErrInvalidInput
		}
		input.BillingClass = &value
		current.BillingClass = value
	}
	if input.Capabilities != nil && !input.Capabilities.Valid() {
		return Model{}, ErrInvalidInput
	}
	if input.Aliases != nil {
		aliases, err := normalizeAliases(*input.Aliases, current.PublicID)
		if err != nil {
			return Model{}, err
		}
		input.Aliases = &aliases
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if current.BillingClass == BillingDisabled && current.Enabled {
		return Model{}, ErrInvalidInput
	}
	return service.repository.update(ctx, actorID, modelID, input, requestID)
}

// Get returns one model for administrator inspection.
func (service *Service) Get(ctx context.Context, modelID uuid.UUID) (Model, error) {
	if modelID == uuid.Nil {
		return Model{}, ErrInvalidInput
	}
	return service.repository.get(ctx, modelID)
}

// ListPublic returns only enabled public models.
func (service *Service) ListPublic(ctx context.Context, cursor string, limit int) (Page, error) {
	cursor, limit, err := validatePage(cursor, limit)
	if err != nil {
		return Page{}, err
	}
	return service.repository.list(ctx, true, cursor, limit)
}

// ListAdmin returns all non-deleted models in stable public-ID order.
func (service *Service) ListAdmin(ctx context.Context, cursor string, limit int) (Page, error) {
	cursor, limit, err := validatePage(cursor, limit)
	if err != nil {
		return Page{}, err
	}
	return service.repository.list(ctx, false, cursor, limit)
}

// ResolvePublic resolves a canonical public ID or enabled alias.
func (service *Service) ResolvePublic(ctx context.Context, identifier string) (Model, error) {
	identifier, err := normalizeIdentifier(identifier)
	if err != nil {
		return Model{}, err
	}
	return service.repository.resolvePublic(ctx, identifier)
}

func validateCreate(input CreateInput) (CreateInput, error) {
	publicID, err := normalizeIdentifier(input.PublicID)
	if err != nil {
		return CreateInput{}, err
	}
	aliases, err := normalizeAliases(input.Aliases, publicID)
	if err != nil {
		return CreateInput{}, err
	}
	displayName, err := normalizeDisplayName(input.DisplayName)
	if err != nil {
		return CreateInput{}, err
	}
	input.PublicID = publicID
	input.Aliases = aliases
	input.DisplayName = displayName
	input.Visibility = strings.TrimSpace(input.Visibility)
	input.BillingClass = strings.TrimSpace(input.BillingClass)
	if !validVisibility(input.Visibility) || !validBillingClass(input.BillingClass) || !input.Capabilities.Valid() {
		return CreateInput{}, ErrInvalidInput
	}
	if input.BillingClass == BillingDisabled && input.Enabled {
		return CreateInput{}, ErrInvalidInput
	}
	return input, nil
}

func validatePage(cursor string, limit int) (string, int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		var err error
		cursor, err = normalizeIdentifier(cursor)
		if err != nil {
			return "", 0, err
		}
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > maximumPageSize {
		return "", 0, ErrInvalidInput
	}
	return cursor, limit, nil
}

func emptyUpdate(input UpdateInput) bool {
	return input.PublicID == nil && input.Aliases == nil && input.DisplayName == nil &&
		input.Visibility == nil && input.BillingClass == nil && input.Capabilities == nil && input.Enabled == nil
}
