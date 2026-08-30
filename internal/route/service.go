package route

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

const maximumPageSize = 100

// Service owns route validation and delegates durable snapshots to PostgreSQL.
type Service struct {
	repository *Repository
}

// NewService constructs a Route service.
func NewService(repository *Repository) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalidInput
	}
	return &Service{repository: repository}, nil
}

// Create atomically creates a route and its first active snapshot.
func (s *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Route, error) {
	if actorID == uuid.Nil {
		return Route{}, ErrInvalidInput
	}
	validated, err := normalizeCreate(input)
	if err != nil {
		return Route{}, err
	}
	return s.repository.create(ctx, actorID, validated, requestID)
}

// Get returns a route and its active immutable snapshot.
func (s *Service) Get(ctx context.Context, routeID uuid.UUID) (Route, error) {
	if routeID == uuid.Nil {
		return Route{}, ErrInvalidInput
	}
	return s.repository.get(ctx, routeID)
}

// List returns routes in stable match-value order.
func (s *Service) List(ctx context.Context, modelID *uuid.UUID, cursor string, limit int) (Page, error) {
	if modelID != nil && *modelID == uuid.Nil {
		return Page{}, ErrInvalidInput
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		if _, err := normalizeIdentifier(cursor); err != nil {
			return Page{}, err
		}
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > maximumPageSize {
		return Page{}, ErrInvalidInput
	}
	return s.repository.list(ctx, modelID, cursor, limit)
}

// Update changes only route metadata or the enabled switch. Snapshot contents
// remain immutable and are changed through PublishVersion.
func (s *Service) Update(ctx context.Context, actorID, routeID uuid.UUID, input UpdateInput, requestID string) (Route, error) {
	if actorID == uuid.Nil || routeID == uuid.Nil {
		return Route{}, ErrInvalidInput
	}
	validated, err := normalizeUpdate(input)
	if err != nil {
		return Route{}, err
	}
	return s.repository.update(ctx, actorID, routeID, validated, requestID)
}

// PublishVersion closes the previous active snapshot exactly once and makes a
// new target snapshot active in one PostgreSQL transaction.
func (s *Service) PublishVersion(ctx context.Context, actorID, routeID uuid.UUID, input PublishInput, requestID string) (Route, error) {
	if actorID == uuid.Nil || routeID == uuid.Nil {
		return Route{}, ErrInvalidInput
	}
	validated, err := normalizePublish(input)
	if err != nil {
		return Route{}, err
	}
	return s.repository.publish(ctx, actorID, routeID, validated, requestID)
}

// ListVersions returns immutable route history, newest first.
func (s *Service) ListVersions(ctx context.Context, routeID uuid.UUID) ([]Version, error) {
	if routeID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	return s.repository.listVersions(ctx, routeID, maximumPageSize)
}

// Resolve binds a requested model identifier to one enabled route snapshot.
// It produces scheduler candidates but deliberately does not choose a
// Credential from a pool.
func (s *Service) Resolve(ctx context.Context, identifier string) (Resolution, error) {
	identifier, err := normalizeIdentifier(identifier)
	if err != nil {
		return Resolution{}, err
	}
	return s.repository.resolve(ctx, identifier)
}
