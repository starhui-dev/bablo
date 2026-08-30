package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
	"github.com/starhui-dev/bablo/internal/model"
)

// Repository persists provider catalog facts in PostgreSQL.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a provider repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("provider repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

type rowScanner interface {
	Scan(...any) error
}

const providerColumns = `
	id, slug, display_name, resource_type, commercial_allowed, enabled, created_at, updated_at`

const providerModelColumns = `
	id, provider_id, model_id, upstream_model_id, protocol, capabilities, enabled,
	review_status, discovery_status, discovered_at, last_seen_at, created_at, updated_at`

func scanProvider(row rowScanner) (Provider, error) {
	var value Provider
	if err := row.Scan(
		&value.ID,
		&value.Slug,
		&value.DisplayName,
		&value.ResourceType,
		&value.CommercialAllowed,
		&value.Enabled,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return Provider{}, err
	}
	return value, nil
}

func scanProviderModel(row rowScanner) (ProviderModel, error) {
	var value ProviderModel
	var modelID pgtype.UUID
	var discoveredAt, lastSeenAt pgtype.Timestamptz
	var capabilitiesJSON []byte
	if err := row.Scan(
		&value.ID,
		&value.ProviderID,
		&modelID,
		&value.UpstreamModelID,
		&value.Protocol,
		&capabilitiesJSON,
		&value.Enabled,
		&value.ReviewStatus,
		&value.DiscoveryStatus,
		&discoveredAt,
		&lastSeenAt,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return ProviderModel{}, err
	}
	if modelID.Valid {
		parsed := uuid.UUID(modelID.Bytes)
		value.ModelID = &parsed
	}
	if discoveredAt.Valid {
		parsed := discoveredAt.Time
		value.DiscoveredAt = &parsed
	}
	if lastSeenAt.Valid {
		parsed := lastSeenAt.Time
		value.LastSeenAt = &parsed
	}
	if err := json.Unmarshal(capabilitiesJSON, &value.Capabilities); err != nil {
		return ProviderModel{}, fmt.Errorf("decode provider model capabilities: %w", err)
	}
	return value, nil
}

func (repository *Repository) create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Provider, error) {
	providerID, err := id.New()
	if err != nil {
		return Provider{}, fmt.Errorf("generate provider UUIDv7: %w", err)
	}
	var created Provider
	err = repository.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO providers (
				id, slug, display_name, resource_type, commercial_allowed, enabled
			)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			providerID, input.Slug, input.DisplayName, input.ResourceType, input.CommercialAllowed, input.Enabled,
		); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "provider.create",
			TargetType:  "provider",
			TargetID:    providerID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		created, err = getProvider(ctx, q, providerID, false)
		return err
	})
	if err != nil {
		return Provider{}, err
	}
	return created, nil
}

func (repository *Repository) update(ctx context.Context, actorID, providerID uuid.UUID, input UpdateInput, requestID string) (Provider, error) {
	var updated Provider
	err := repository.store.WithTx(ctx, func(q data.Querier) error {
		current, err := getProvider(ctx, q, providerID, true)
		if err != nil {
			return err
		}
		if input.Slug != nil {
			current.Slug = *input.Slug
		}
		if input.DisplayName != nil {
			current.DisplayName = *input.DisplayName
		}
		if input.ResourceType != nil {
			current.ResourceType = *input.ResourceType
		}
		if input.CommercialAllowed != nil {
			current.CommercialAllowed = *input.CommercialAllowed
		}
		if input.Enabled != nil {
			current.Enabled = *input.Enabled
		}
		if _, err := q.Exec(ctx, `
			UPDATE providers
			SET slug = $2,
				display_name = $3,
				resource_type = $4,
				commercial_allowed = $5,
				enabled = $6,
				updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL`,
			providerID,
			current.Slug,
			current.DisplayName,
			current.ResourceType,
			current.CommercialAllowed,
			current.Enabled,
		); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "provider.update",
			TargetType:  "provider",
			TargetID:    providerID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated, err = getProvider(ctx, q, providerID, false)
		return err
	})
	if err != nil {
		return Provider{}, err
	}
	return updated, nil
}

func (repository *Repository) get(ctx context.Context, providerID uuid.UUID) (Provider, error) {
	return getProvider(ctx, repository.store.Queryer(), providerID, false)
}

func getProvider(ctx context.Context, q data.Querier, providerID uuid.UUID, lock bool) (Provider, error) {
	query := `SELECT ` + providerColumns + ` FROM providers WHERE id = $1 AND deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	value, err := scanProvider(q.QueryRow(ctx, query, providerID))
	if err != nil {
		return Provider{}, mapRepositoryError(err)
	}
	return value, nil
}

func (repository *Repository) list(ctx context.Context, cursor string, limit int) (Page, error) {
	rows, err := repository.store.Queryer().Query(ctx, `
		SELECT `+providerColumns+`
		FROM providers
		WHERE deleted_at IS NULL AND slug > $1
		ORDER BY slug
		LIMIT $2`, cursor, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	values := make([]Provider, 0, limit+1)
	for rows.Next() {
		value, err := scanProvider(rows)
		if err != nil {
			return Page{}, fmt.Errorf("scan provider: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate providers: %w", err)
	}
	page := Page{Providers: values}
	if len(values) > limit {
		page.NextCursor = values[limit-1].Slug
		page.Providers = values[:limit]
	}
	return page, nil
}

func (repository *Repository) createModel(ctx context.Context, actorID uuid.UUID, input CreateModelInput, requestID string) (ProviderModel, error) {
	providerModelID, err := id.New()
	if err != nil {
		return ProviderModel{}, fmt.Errorf("generate provider model UUIDv7: %w", err)
	}
	capabilitiesJSON, err := json.Marshal(input.Capabilities)
	if err != nil {
		return ProviderModel{}, fmt.Errorf("encode provider model capabilities: %w", err)
	}
	var created ProviderModel
	err = repository.store.WithTx(ctx, func(q data.Querier) error {
		if err := validateModelCapabilities(ctx, q, input.ModelID, input.Capabilities); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO provider_models (
				id, provider_id, model_id, upstream_model_id, protocol, capabilities,
				enabled, review_status, discovery_status
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'approved', 'unknown')`,
			providerModelID,
			input.ProviderID,
			input.ModelID,
			input.UpstreamModelID,
			input.Protocol,
			capabilitiesJSON,
			input.Enabled,
		); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "provider_model.create",
			TargetType:  "provider_model",
			TargetID:    providerModelID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		created, err = getProviderModel(ctx, q, providerModelID, false)
		return err
	})
	if err != nil {
		return ProviderModel{}, err
	}
	return created, nil
}

func (repository *Repository) updateModel(ctx context.Context, actorID, providerModelID uuid.UUID, input UpdateModelInput, requestID string) (ProviderModel, error) {
	var updated ProviderModel
	err := repository.store.WithTx(ctx, func(q data.Querier) error {
		current, err := getProviderModel(ctx, q, providerModelID, true)
		if err != nil {
			return err
		}
		if input.ModelID.Set {
			current.ModelID = input.ModelID.Value
		}
		if input.Protocol != nil {
			current.Protocol = *input.Protocol
		}
		if input.Capabilities != nil {
			current.Capabilities = *input.Capabilities
		}
		if input.Enabled != nil {
			current.Enabled = *input.Enabled
		}
		if input.ReviewStatus != nil {
			current.ReviewStatus = *input.ReviewStatus
		}
		if current.ReviewStatus == ReviewRejected {
			current.Enabled = false
		}
		if current.Enabled && (current.ModelID == nil || current.ReviewStatus != ReviewApproved) {
			return ErrInvalidInput
		}
		if current.ModelID != nil {
			if err := validateModelCapabilities(ctx, q, *current.ModelID, current.Capabilities); err != nil {
				return err
			}
		}
		capabilitiesJSON, err := json.Marshal(current.Capabilities)
		if err != nil {
			return fmt.Errorf("encode provider model capabilities: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE provider_models
			SET model_id = $2,
				protocol = $3,
				capabilities = $4,
				enabled = $5,
				review_status = $6,
				updated_at = now()
			WHERE id = $1`,
			providerModelID,
			current.ModelID,
			current.Protocol,
			capabilitiesJSON,
			current.Enabled,
			current.ReviewStatus,
		); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "provider_model.update",
			TargetType:  "provider_model",
			TargetID:    providerModelID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated, err = getProviderModel(ctx, q, providerModelID, false)
		return err
	})
	if err != nil {
		return ProviderModel{}, err
	}
	return updated, nil
}

func (repository *Repository) getModel(ctx context.Context, providerModelID uuid.UUID) (ProviderModel, error) {
	return getProviderModel(ctx, repository.store.Queryer(), providerModelID, false)
}

func getProviderModel(ctx context.Context, q data.Querier, providerModelID uuid.UUID, lock bool) (ProviderModel, error) {
	query := `SELECT ` + providerModelColumns + ` FROM provider_models WHERE id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	value, err := scanProviderModel(q.QueryRow(ctx, query, providerModelID))
	if err != nil {
		return ProviderModel{}, mapRepositoryError(err)
	}
	return value, nil
}

func (repository *Repository) listModels(ctx context.Context, providerID uuid.UUID, cursor string, limit int) (ModelPage, error) {
	rows, err := repository.store.Queryer().Query(ctx, `
		SELECT `+providerModelColumns+`
		FROM provider_models
		WHERE provider_id = $1 AND upstream_model_id > $2
		ORDER BY upstream_model_id
		LIMIT $3`, providerID, cursor, limit+1)
	if err != nil {
		return ModelPage{}, fmt.Errorf("list provider models: %w", err)
	}
	defer rows.Close()
	values := make([]ProviderModel, 0, limit+1)
	for rows.Next() {
		value, err := scanProviderModel(rows)
		if err != nil {
			return ModelPage{}, fmt.Errorf("scan provider model: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return ModelPage{}, fmt.Errorf("iterate provider models: %w", err)
	}
	page := ModelPage{Models: values}
	if len(values) > limit {
		page.NextCursor = values[limit-1].UpstreamModelID
		page.Models = values[:limit]
	}
	return page, nil
}

func (repository *Repository) reconcile(ctx context.Context, actorID, providerID uuid.UUID, discoveries []Discovery, observedAt time.Time, requestID string) (ReconcileResult, error) {
	result := ReconcileResult{Observed: len(discoveries), ObservedAt: observedAt}
	err := repository.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := getProvider(ctx, q, providerID, true); err != nil {
			return err
		}
		for _, discovery := range discoveries {
			var existingID uuid.UUID
			var reviewStatus string
			err := q.QueryRow(ctx, `
				SELECT id, review_status
				FROM provider_models
				WHERE provider_id = $1 AND upstream_model_id = $2
				FOR UPDATE`, providerID, discovery.UpstreamModelID).Scan(&existingID, &reviewStatus)
			capabilitiesJSON, marshalErr := json.Marshal(discovery.Capabilities)
			if marshalErr != nil {
				return fmt.Errorf("encode discovered capabilities: %w", marshalErr)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				existingID, err = id.New()
				if err != nil {
					return fmt.Errorf("generate discovered provider model UUIDv7: %w", err)
				}
				if _, err := q.Exec(ctx, `
					INSERT INTO provider_models (
						id, provider_id, upstream_model_id, protocol, capabilities, enabled,
						review_status, discovery_status, discovered_at, last_seen_at
					)
					VALUES ($1, $2, $3, $4, $5, false, 'pending', 'present', $6, $6)`,
					existingID,
					providerID,
					discovery.UpstreamModelID,
					discovery.Protocol,
					capabilitiesJSON,
					observedAt,
				); err != nil {
					return mapRepositoryError(err)
				}
				result.Discovered++
				continue
			}
			if err != nil {
				return fmt.Errorf("find discovered provider model: %w", err)
			}
			if reviewStatus == ReviewPending {
				_, err = q.Exec(ctx, `
					UPDATE provider_models
					SET protocol = $2,
						capabilities = $3,
						discovery_status = 'present',
						discovered_at = COALESCE(discovered_at, $4),
						last_seen_at = $4,
						updated_at = now()
					WHERE id = $1`, existingID, discovery.Protocol, capabilitiesJSON, observedAt)
			} else {
				_, err = q.Exec(ctx, `
					UPDATE provider_models
					SET discovery_status = 'present',
						discovered_at = COALESCE(discovered_at, $2),
						last_seen_at = $2,
						updated_at = now()
					WHERE id = $1`, existingID, observedAt)
			}
			if err != nil {
				return fmt.Errorf("update discovered provider model: %w", err)
			}
		}
		tag, err := q.Exec(ctx, `
			UPDATE provider_models
			SET discovery_status = 'missing', updated_at = now()
			WHERE provider_id = $1
			  AND discovered_at IS NOT NULL
			  AND (last_seen_at IS NULL OR last_seen_at < $2)
			  AND discovery_status <> 'missing'`, providerID, observedAt)
		if err != nil {
			return fmt.Errorf("mark missing provider models: %w", err)
		}
		result.Missing = tag.RowsAffected()
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "provider_model.reconcile",
			TargetType:  "provider",
			TargetID:    providerID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

func (repository *Repository) modelCapabilities(ctx context.Context, modelID uuid.UUID) (model.Capabilities, error) {
	return modelCapabilities(ctx, repository.store.Queryer(), modelID)
}

func validateModelCapabilities(ctx context.Context, q data.Querier, modelID uuid.UUID, providerCapabilities model.Capabilities) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "model-capabilities:"+modelID.String()); err != nil {
		return fmt.Errorf("lock model capability mapping: %w", err)
	}
	publicCapabilities, err := modelCapabilities(ctx, q, modelID)
	if err != nil {
		return err
	}
	if !publicCapabilities.Supports(providerCapabilities) {
		return ErrInvalidInput
	}
	return nil
}

func modelCapabilities(ctx context.Context, q data.Querier, modelID uuid.UUID) (model.Capabilities, error) {
	var capabilitiesJSON []byte
	if err := q.QueryRow(ctx, `
		SELECT capabilities
		FROM models
		WHERE id = $1 AND deleted_at IS NULL`, modelID).Scan(&capabilitiesJSON); err != nil {
		return model.Capabilities{}, mapRepositoryError(err)
	}
	var capabilities model.Capabilities
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
		return model.Capabilities{}, fmt.Errorf("decode public model capabilities: %w", err)
	}
	return capabilities, nil
}

func mapRepositoryError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23505":
			return ErrConflict
		case "23503", "23514", "22P02":
			return ErrInvalidInput
		}
	}
	return err
}
