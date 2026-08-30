package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

// Repository persists model catalog facts in PostgreSQL.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a model repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("model repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

type rowScanner interface {
	Scan(...any) error
}

const modelColumns = `
	models.id, models.public_model_id, models.display_name, models.visibility, models.billing_class,
	models.capabilities, models.enabled, models.created_at, models.updated_at,
	EXISTS (
		SELECT 1 FROM model_routes route
		WHERE route.model_id = models.id
		  AND route.enabled AND route.deleted_at IS NULL
		  AND route.active_version_id IS NOT NULL
	) AS route_configured`

func scanModel(row rowScanner) (Model, error) {
	var value Model
	var capabilitiesJSON []byte
	if err := row.Scan(
		&value.ID,
		&value.PublicID,
		&value.DisplayName,
		&value.Visibility,
		&value.BillingClass,
		&capabilitiesJSON,
		&value.Enabled,
		&value.CreatedAt,
		&value.UpdatedAt,
		&value.RouteConfigured,
	); err != nil {
		return Model{}, err
	}
	if err := json.Unmarshal(capabilitiesJSON, &value.Capabilities); err != nil {
		return Model{}, fmt.Errorf("decode model capabilities: %w", err)
	}
	value.Aliases = []string{}
	return value, nil
}

func (repository *Repository) create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Model, error) {
	modelID, err := id.New()
	if err != nil {
		return Model{}, fmt.Errorf("generate model UUIDv7: %w", err)
	}
	capabilitiesJSON, err := json.Marshal(input.Capabilities)
	if err != nil {
		return Model{}, fmt.Errorf("encode model capabilities: %w", err)
	}
	var created Model
	err = repository.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO models (
				id, public_model_id, display_name, visibility, billing_class, capabilities, enabled
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			modelID, input.PublicID, input.DisplayName, input.Visibility, input.BillingClass, capabilitiesJSON, input.Enabled,
		); err != nil {
			return mapRepositoryError(err)
		}
		if err := replaceAliases(ctx, q, modelID, input.Aliases); err != nil {
			return err
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "model.create",
			TargetType:  "model",
			TargetID:    modelID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		created, err = getModel(ctx, q, modelID, false)
		return err
	})
	if err != nil {
		return Model{}, err
	}
	return created, nil
}

func (repository *Repository) update(ctx context.Context, actorID, modelID uuid.UUID, input UpdateInput, requestID string) (Model, error) {
	var updated Model
	err := repository.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "model-capabilities:"+modelID.String()); err != nil {
			return fmt.Errorf("lock model capability update: %w", err)
		}
		current, err := getModel(ctx, q, modelID, true)
		if err != nil {
			return err
		}
		if input.PublicID != nil {
			current.PublicID = *input.PublicID
		}
		if input.DisplayName != nil {
			current.DisplayName = *input.DisplayName
		}
		if input.Visibility != nil {
			current.Visibility = *input.Visibility
		}
		if input.BillingClass != nil {
			current.BillingClass = *input.BillingClass
		}
		if input.Capabilities != nil {
			current.Capabilities = *input.Capabilities
		}
		if input.Enabled != nil {
			current.Enabled = *input.Enabled
		}
		if err := validateProviderCapabilities(ctx, q, modelID, current.Capabilities); err != nil {
			return err
		}
		capabilitiesJSON, err := json.Marshal(current.Capabilities)
		if err != nil {
			return fmt.Errorf("encode model capabilities: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE models
			SET public_model_id = $2,
				display_name = $3,
				visibility = $4,
				billing_class = $5,
				capabilities = $6,
				enabled = $7,
				updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL`,
			modelID,
			current.PublicID,
			current.DisplayName,
			current.Visibility,
			current.BillingClass,
			capabilitiesJSON,
			current.Enabled,
		); err != nil {
			return mapRepositoryError(err)
		}
		if input.Aliases != nil {
			if err := replaceAliases(ctx, q, modelID, *input.Aliases); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `
			UPDATE model_aliases
			SET enabled = false, updated_at = now()
			WHERE model_id = $1 AND alias = $2 AND enabled`, modelID, current.PublicID); err != nil {
			return fmt.Errorf("disable alias promoted to canonical id: %w", err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "model.update",
			TargetType:  "model",
			TargetID:    modelID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated, err = getModel(ctx, q, modelID, false)
		return err
	})
	if err != nil {
		return Model{}, err
	}
	return updated, nil
}

func replaceAliases(ctx context.Context, q data.Querier, modelID uuid.UUID, aliases []string) error {
	if _, err := q.Exec(ctx, `
		UPDATE model_aliases SET enabled = false, updated_at = now()
		WHERE model_id = $1 AND enabled`, modelID); err != nil {
		return fmt.Errorf("disable model aliases: %w", err)
	}
	for _, alias := range aliases {
		aliasID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate model alias UUIDv7: %w", err)
		}
		tag, err := q.Exec(ctx, `
			INSERT INTO model_aliases (id, model_id, alias, enabled)
			VALUES ($1, $2, $3, true)
			ON CONFLICT (alias) DO UPDATE
			SET enabled = true, updated_at = now()
			WHERE model_aliases.model_id = EXCLUDED.model_id`, aliasID, modelID, alias)
		if err != nil {
			return mapRepositoryError(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	return nil
}

func validateProviderCapabilities(ctx context.Context, q data.Querier, modelID uuid.UUID, publicCapabilities Capabilities) error {
	rows, err := q.Query(ctx, `SELECT capabilities FROM provider_models WHERE model_id = $1`, modelID)
	if err != nil {
		return fmt.Errorf("list provider model capabilities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var capabilitiesJSON []byte
		if err := rows.Scan(&capabilitiesJSON); err != nil {
			return fmt.Errorf("scan provider model capabilities: %w", err)
		}
		var providerCapabilities Capabilities
		if err := json.Unmarshal(capabilitiesJSON, &providerCapabilities); err != nil {
			return fmt.Errorf("decode provider model capabilities: %w", err)
		}
		if !publicCapabilities.Supports(providerCapabilities) {
			return ErrInvalidInput
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate provider model capabilities: %w", err)
	}
	return nil
}

func (repository *Repository) get(ctx context.Context, modelID uuid.UUID) (Model, error) {
	return getModel(ctx, repository.store.Queryer(), modelID, false)
}

func getModel(ctx context.Context, q data.Querier, modelID uuid.UUID, lock bool) (Model, error) {
	query := `SELECT ` + modelColumns + ` FROM models WHERE id = $1 AND deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	value, err := scanModel(q.QueryRow(ctx, query, modelID))
	if err != nil {
		return Model{}, mapRepositoryError(err)
	}
	models := []Model{value}
	if err := loadAliases(ctx, q, models); err != nil {
		return Model{}, err
	}
	return models[0], nil
}

func (repository *Repository) list(ctx context.Context, publicOnly bool, cursor string, limit int) (Page, error) {
	query := `SELECT ` + modelColumns + `
		FROM models
		WHERE deleted_at IS NULL AND public_model_id > $1`
	if publicOnly {
		query += ` AND enabled AND visibility = 'public'`
	}
	query += ` ORDER BY public_model_id LIMIT $2`
	rows, err := repository.store.Queryer().Query(ctx, query, cursor, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	values := make([]Model, 0, limit+1)
	for rows.Next() {
		value, err := scanModel(rows)
		if err != nil {
			return Page{}, fmt.Errorf("scan model: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate models: %w", err)
	}
	page := Page{Models: values}
	if len(values) > limit {
		page.NextCursor = values[limit-1].PublicID
		page.Models = values[:limit]
	}
	if err := loadAliases(ctx, repository.store.Queryer(), page.Models); err != nil {
		return Page{}, err
	}
	return page, nil
}

func loadAliases(ctx context.Context, q data.Querier, models []Model) error {
	if len(models) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(models))
	positions := make(map[uuid.UUID]int, len(models))
	for index := range models {
		models[index].Aliases = []string{}
		ids = append(ids, models[index].ID)
		positions[models[index].ID] = index
	}
	rows, err := q.Query(ctx, `
		SELECT model_id, alias
		FROM model_aliases
		WHERE model_id = ANY($1) AND enabled
		ORDER BY model_id, alias`, ids)
	if err != nil {
		return fmt.Errorf("list model aliases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var modelID uuid.UUID
		var alias string
		if err := rows.Scan(&modelID, &alias); err != nil {
			return fmt.Errorf("scan model alias: %w", err)
		}
		if index, ok := positions[modelID]; ok {
			models[index].Aliases = append(models[index].Aliases, alias)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate model aliases: %w", err)
	}
	return nil
}

func (repository *Repository) resolvePublic(ctx context.Context, identifier string) (Model, error) {
	row := repository.store.Queryer().QueryRow(ctx, `
		SELECT `+modelColumns+`
		FROM models
		WHERE public_model_id = $1
		  AND enabled AND visibility = 'public' AND deleted_at IS NULL
		UNION ALL
		SELECT `+qualifiedModelColumns("m")+`
		FROM model_aliases alias
		JOIN models m ON m.id = alias.model_id
		WHERE alias.alias = $1 AND alias.enabled
		  AND m.enabled AND m.visibility = 'public' AND m.deleted_at IS NULL
		LIMIT 1`, identifier)
	value, err := scanModel(row)
	if err != nil {
		return Model{}, mapRepositoryError(err)
	}
	values := []Model{value}
	if err := loadAliases(ctx, repository.store.Queryer(), values); err != nil {
		return Model{}, err
	}
	return values[0], nil
}

func qualifiedModelColumns(alias string) string {
	return alias + `.id, ` + alias + `.public_model_id, ` + alias + `.display_name, ` + alias + `.visibility, ` + alias + `.billing_class, ` + alias + `.capabilities, ` + alias + `.enabled, ` + alias + `.created_at, ` + alias + `.updated_at, EXISTS (` +
		`SELECT 1 FROM model_routes route WHERE route.model_id = ` + alias + `.id ` +
		`AND route.enabled AND route.deleted_at IS NULL AND route.active_version_id IS NOT NULL) AS route_configured`
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
