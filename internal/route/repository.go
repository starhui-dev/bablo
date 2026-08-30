package route

import (
	"context"
	"crypto/sha256"
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
)

// Repository persists route identities and immutable route snapshots.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a Route repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("route repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

type rowScanner interface {
	Scan(...any) error
}

const routeColumns = `
	r.id, r.model_id, m.public_model_id, r.match_type, r.match_value, r.enabled,
	r.metadata, r.active_version_id, r.created_at, r.updated_at`

func scanRoute(row rowScanner) (Route, error) {
	var value Route
	var metadataJSON []byte
	var activeVersionID pgtype.UUID
	if err := row.Scan(
		&value.ID,
		&value.ModelID,
		&value.ModelPublicID,
		&value.MatchType,
		&value.MatchValue,
		&value.Enabled,
		&metadataJSON,
		&activeVersionID,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return Route{}, err
	}
	value.Metadata = map[string]string{}
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &value.Metadata); err != nil {
			return Route{}, fmt.Errorf("decode route metadata: %w", err)
		}
	}
	if activeVersionID.Valid {
		parsed := uuid.UUID(activeVersionID.Bytes)
		value.ActiveVersionID = &parsed
	}
	return value, nil
}

func scanVersion(row rowScanner) (Version, error) {
	var value Version
	var createdAt, effectiveTo pgtype.Timestamptz
	var createdByID pgtype.UUID
	if err := row.Scan(
		&value.ID,
		&value.RouteID,
		&value.VersionNo,
		&value.EffectiveFrom,
		&effectiveTo,
		&value.SnapshotHash,
		&createdByID,
		&createdAt,
	); err != nil {
		return Version{}, err
	}
	if effectiveTo.Valid {
		parsed := effectiveTo.Time.UTC()
		value.EffectiveTo = &parsed
	}
	if createdByID.Valid {
		parsed := uuid.UUID(createdByID.Bytes)
		value.CreatedBy = &parsed
	}
	if createdAt.Valid {
		value.CreatedAt = createdAt.Time.UTC()
	}
	value.Targets = []Target{}
	return value, nil
}

func scanTarget(row rowScanner) (Target, error) {
	var value Target
	var capabilitiesJSON, metadataJSON []byte
	if err := row.Scan(
		&value.ID,
		&value.RouteVersionID,
		&value.TargetNo,
		&value.ProviderModelID,
		&value.CredentialPoolID,
		&value.ProviderID,
		&value.ProviderSlug,
		&value.ProviderResourceType,
		&value.ProviderCommercialAllowed,
		&value.UpstreamModelID,
		&value.Protocol,
		&capabilitiesJSON,
		&value.ProviderModelEnabled,
		&value.ReviewStatus,
		&value.PoolEnabled,
		&value.Priority,
		&value.Weight,
		&value.CommercialPolicy,
		&value.EffectiveCommercialAllowed,
		&value.Enabled,
		&metadataJSON,
	); err != nil {
		return Target{}, err
	}
	if err := json.Unmarshal(capabilitiesJSON, &value.Capabilities); err != nil {
		return Target{}, fmt.Errorf("decode route target capabilities: %w", err)
	}
	value.Metadata = map[string]string{}
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &value.Metadata); err != nil {
			return Target{}, fmt.Errorf("decode route target metadata: %w", err)
		}
	}
	return value, nil
}

func (r *Repository) create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Route, error) {
	routeID, err := id.New()
	if err != nil {
		return Route{}, fmt.Errorf("generate route UUIDv7: %w", err)
	}
	versionID, err := id.New()
	if err != nil {
		return Route{}, fmt.Errorf("generate route version UUIDv7: %w", err)
	}
	metadataJSON, err := json.Marshal(input.Metadata)
	if err != nil {
		return Route{}, fmt.Errorf("encode route metadata: %w", err)
	}
	var created Route
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		if err := validateRouteMatch(ctx, q, input.ModelID, input.MatchValue); err != nil {
			return err
		}
		if err := validateTargets(ctx, q, input.ModelID, input.Targets); err != nil {
			return err
		}
		snapshotHash, err := routeSnapshotHash(routeID, input.ModelID, input.MatchType, input.MatchValue, input.Targets)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO model_routes (id, model_id, match_type, match_value, enabled, metadata)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			routeID, input.ModelID, input.MatchType, input.MatchValue, input.Enabled, metadataJSON); err != nil {
			return mapRepositoryError(err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO route_versions (id, route_id, version_no, effective_from, snapshot_hash, created_by)
			VALUES ($1, $2, 1, clock_timestamp(), $3, $4)`,
			versionID, routeID, snapshotHash, actorID); err != nil {
			return mapRepositoryError(err)
		}
		if err := insertTargets(ctx, q, versionID, input.Targets); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE model_routes SET active_version_id = $2 WHERE id = $1`, routeID, versionID); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "route.created",
			TargetType:  "route",
			TargetID:    routeID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "route.version_published",
			TargetType:  "route_version",
			TargetID:    versionID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		created, err = getRoute(ctx, q, routeID, true)
		return err
	})
	if err != nil {
		return Route{}, err
	}
	return created, nil
}

func (r *Repository) update(ctx context.Context, actorID, routeID uuid.UUID, input UpdateInput, requestID string) (Route, error) {
	var updated Route
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		current, err := getRoute(ctx, q, routeID, false)
		if err != nil {
			return err
		}
		metadata := current.Metadata
		if input.Metadata != nil {
			metadata = *input.Metadata
		}
		enabled := current.Enabled
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		metadataJSON, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("encode route metadata: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE model_routes
			SET metadata = $2, enabled = $3, updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL`, routeID, metadataJSON, enabled); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "route.updated",
			TargetType:  "route",
			TargetID:    routeID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated, err = getRoute(ctx, q, routeID, true)
		return err
	})
	if err != nil {
		return Route{}, err
	}
	return updated, nil
}

func (r *Repository) publish(ctx context.Context, actorID, routeID uuid.UUID, input PublishInput, requestID string) (Route, error) {
	versionID, err := id.New()
	if err != nil {
		return Route{}, fmt.Errorf("generate route version UUIDv7: %w", err)
	}
	var published Route
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `SELECT id FROM model_routes WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, routeID); err != nil {
			return mapRepositoryError(err)
		}
		current, err := getRoute(ctx, q, routeID, false)
		if err != nil {
			return err
		}
		if err := validateTargets(ctx, q, current.ModelID, input.Targets); err != nil {
			return err
		}
		var oldVersionID uuid.UUID
		oldVersionErr := q.QueryRow(ctx, `
			SELECT id
			FROM route_versions
			WHERE route_id = $1 AND effective_to IS NULL
			FOR UPDATE`, routeID).Scan(&oldVersionID)
		if oldVersionErr != nil && !errors.Is(oldVersionErr, pgx.ErrNoRows) {
			return fmt.Errorf("lock active route version: %w", oldVersionErr)
		}
		var effectiveFrom time.Time
		if oldVersionErr == nil {
			if err := q.QueryRow(ctx, `
				UPDATE route_versions
				SET effective_to = GREATEST(clock_timestamp(), effective_from + interval '1 microsecond')
				WHERE id = $1
				RETURNING effective_to`, oldVersionID).Scan(&effectiveFrom); err != nil {
				return mapRepositoryError(err)
			}
		} else {
			if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&effectiveFrom); err != nil {
				return fmt.Errorf("allocate route version effective time: %w", err)
			}
		}
		var nextVersionNo int64
		if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(version_no), 0) + 1 FROM route_versions WHERE route_id = $1`, routeID).Scan(&nextVersionNo); err != nil {
			return fmt.Errorf("allocate route version number: %w", err)
		}
		snapshotHash, err := routeSnapshotHash(routeID, current.ModelID, current.MatchType, current.MatchValue, input.Targets)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO route_versions (id, route_id, version_no, effective_from, snapshot_hash, created_by)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			versionID, routeID, nextVersionNo, effectiveFrom, snapshotHash, actorID); err != nil {
			return mapRepositoryError(err)
		}
		if err := insertTargets(ctx, q, versionID, input.Targets); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE model_routes SET active_version_id = $2, updated_at = now() WHERE id = $1`, routeID, versionID); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "route.version_published",
			TargetType:  "route_version",
			TargetID:    versionID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		published, err = getRoute(ctx, q, routeID, true)
		return err
	})
	if err != nil {
		return Route{}, err
	}
	return published, nil
}

func (r *Repository) get(ctx context.Context, routeID uuid.UUID) (Route, error) {
	return getRoute(ctx, r.store.Queryer(), routeID, true)
}

func (r *Repository) list(ctx context.Context, modelID *uuid.UUID, cursor string, limit int) (Page, error) {
	query := `SELECT ` + routeColumns + `
		FROM model_routes r
		JOIN models m ON m.id = r.model_id
		WHERE r.deleted_at IS NULL AND r.match_value > $1`
	args := []any{cursor}
	if modelID != nil {
		query += ` AND r.model_id = $2`
		args = append(args, *modelID)
	}
	query += fmt.Sprintf(` ORDER BY r.match_value, r.id LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)
	rows, err := r.store.Queryer().Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list routes: %w", err)
	}
	values := make([]Route, 0, limit+1)
	for rows.Next() {
		value, err := scanRoute(rows)
		if err != nil {
			return Page{}, fmt.Errorf("scan route: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate routes: %w", err)
	}
	rows.Close()
	for index := range values {
		if values[index].ActiveVersionID == nil {
			continue
		}
		version, err := getVersion(ctx, r.store.Queryer(), *values[index].ActiveVersionID, true)
		if err != nil {
			return Page{}, err
		}
		values[index].ActiveVersion = &version
	}
	page := Page{Routes: values}
	if len(values) > limit {
		page.NextCursor = values[limit-1].MatchValue
		page.Routes = values[:limit]
	}
	return page, nil
}

func (r *Repository) resolve(ctx context.Context, identifier string) (Resolution, error) {
	var routeID, modelID uuid.UUID
	var modelPublicID string
	var enabled bool
	err := r.store.Queryer().QueryRow(ctx, `
		SELECT r.id, r.model_id, m.public_model_id, r.enabled
		FROM model_routes r
		JOIN models m ON m.id = r.model_id AND m.deleted_at IS NULL
		WHERE r.match_type = 'exact'
		  AND r.match_value = $1
		  AND (m.public_model_id = $1 OR EXISTS (
			SELECT 1 FROM model_aliases a
			WHERE a.model_id = m.id AND a.alias = $1 AND a.enabled
		  ))
		  AND r.deleted_at IS NULL`, identifier).Scan(&routeID, &modelID, &modelPublicID, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, ErrNoRoute
	}
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve route: %w", err)
	}
	if !enabled {
		return Resolution{}, ErrRouteDisabled
	}
	value, err := r.get(ctx, routeID)
	if err != nil {
		return Resolution{}, err
	}
	if value.ActiveVersion == nil {
		return Resolution{}, ErrNoRoute
	}
	return Resolution{
		RequestedModel: identifier,
		ModelID:        modelID,
		ModelPublicID:  modelPublicID,
		Route:          value,
		Version:        *value.ActiveVersion,
		Candidates:     append([]Target(nil), value.ActiveVersion.Targets...),
	}, nil
}

func (r *Repository) listVersions(ctx context.Context, routeID uuid.UUID, limit int) ([]Version, error) {
	if _, err := getRoute(ctx, r.store.Queryer(), routeID, false); err != nil {
		return nil, err
	}
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT id, route_id, version_no, effective_from, effective_to, snapshot_hash, created_by, created_at
		FROM route_versions
		WHERE route_id = $1
		ORDER BY version_no DESC
		LIMIT $2`, routeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list route versions: %w", err)
	}
	defer rows.Close()
	values := make([]Version, 0, limit)
	for rows.Next() {
		value, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan route version: %w", err)
		}
		value.Targets, err = loadTargets(ctx, r.store.Queryer(), value.ID)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route versions: %w", err)
	}
	return values, nil
}

func getRoute(ctx context.Context, q data.Querier, routeID uuid.UUID, includeVersion bool) (Route, error) {
	value, err := scanRoute(q.QueryRow(ctx, `
		SELECT `+routeColumns+`
		FROM model_routes r
		JOIN models m ON m.id = r.model_id
		WHERE r.id = $1 AND r.deleted_at IS NULL`, routeID))
	if err != nil {
		return Route{}, mapRepositoryError(err)
	}
	if includeVersion && value.ActiveVersionID != nil {
		version, err := getVersion(ctx, q, *value.ActiveVersionID, true)
		if err != nil {
			return Route{}, err
		}
		value.ActiveVersion = &version
	}
	return value, nil
}

func getVersion(ctx context.Context, q data.Querier, versionID uuid.UUID, includeTargets bool) (Version, error) {
	value, err := scanVersion(q.QueryRow(ctx, `
		SELECT id, route_id, version_no, effective_from, effective_to, snapshot_hash, created_by, created_at
		FROM route_versions WHERE id = $1`, versionID))
	if err != nil {
		return Version{}, mapRepositoryError(err)
	}
	if includeTargets {
		value.Targets, err = loadTargets(ctx, q, versionID)
		if err != nil {
			return Version{}, err
		}
	}
	return value, nil
}

func loadTargets(ctx context.Context, q data.Querier, versionID uuid.UUID) ([]Target, error) {
	rows, err := q.Query(ctx, `
		SELECT rt.id, rt.route_version_id, rt.target_no, rt.provider_model_id, rt.credential_pool_id,
			pm.provider_id, p.slug, p.resource_type, p.commercial_allowed,
			pm.upstream_model_id, pm.protocol, pm.capabilities, pm.enabled, pm.review_status,
			cp.enabled, rt.priority, rt.weight, rt.commercial_policy,
			CASE rt.commercial_policy
				WHEN 'allow' THEN true
				WHEN 'deny' THEN false
				ELSE p.commercial_allowed
			END AS effective_commercial_allowed,
			rt.enabled, rt.metadata
		FROM route_targets rt
		JOIN provider_models pm ON pm.id = rt.provider_model_id
		JOIN providers p ON p.id = pm.provider_id
		JOIN credential_pools cp ON cp.id = rt.credential_pool_id
		WHERE rt.route_version_id = $1
		ORDER BY rt.target_no`, versionID)
	if err != nil {
		return nil, fmt.Errorf("list route targets: %w", err)
	}
	defer rows.Close()
	values := make([]Target, 0)
	for rows.Next() {
		value, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("scan route target: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route targets: %w", err)
	}
	return values, nil
}

func validateRouteMatch(ctx context.Context, q data.Querier, modelID uuid.UUID, matchValue string) error {
	var exists bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM models m
			WHERE m.id = $1 AND m.deleted_at IS NULL
			  AND (m.public_model_id = $2 OR EXISTS (
				SELECT 1 FROM model_aliases a
				WHERE a.model_id = m.id AND a.alias = $2 AND a.enabled
			  ))
		)`, modelID, matchValue).Scan(&exists); err != nil {
		return fmt.Errorf("validate route model match: %w", err)
	}
	if !exists {
		return ErrInvalidInput
	}
	return nil
}

func validateTargets(ctx context.Context, q data.Querier, modelID uuid.UUID, targets []TargetInput) error {
	for _, target := range targets {
		var targetModelID pgtype.UUID
		var resourceType string
		var commercialAllowed bool
		err := q.QueryRow(ctx, `
			SELECT pm.model_id, p.resource_type, p.commercial_allowed
			FROM provider_models pm
			JOIN providers p ON p.id = pm.provider_id
			JOIN credential_pools cp ON cp.id = $2 AND cp.provider_id = pm.provider_id
			WHERE pm.id = $1`, target.ProviderModelID, target.CredentialPoolID).Scan(&targetModelID, &resourceType, &commercialAllowed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidInput
		}
		if err != nil {
			return fmt.Errorf("validate route target: %w", err)
		}
		if !targetModelID.Valid || uuid.UUID(targetModelID.Bytes) != modelID {
			return ErrInvalidInput
		}
		if target.CommercialPolicy == CommercialAllow && (!commercialAllowed || resourceType == "subscription") {
			return ErrInvalidInput
		}
	}
	return nil
}

func insertTargets(ctx context.Context, q data.Querier, versionID uuid.UUID, targets []TargetInput) error {
	for index, target := range targets {
		targetID, err := id.New()
		if err != nil {
			return fmt.Errorf("generate route target UUIDv7: %w", err)
		}
		metadataJSON, err := json.Marshal(target.Metadata)
		if err != nil {
			return fmt.Errorf("encode route target metadata: %w", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO route_targets (
				id, route_version_id, target_no, provider_model_id, credential_pool_id,
				priority, weight, commercial_policy, enabled, metadata
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			targetID, versionID, index, target.ProviderModelID, target.CredentialPoolID,
			target.Priority, target.Weight, target.CommercialPolicy, target.Enabled, metadataJSON); err != nil {
			return mapRepositoryError(err)
		}
	}
	return nil
}

type snapshotTarget struct {
	TargetNo         int
	ProviderModelID  uuid.UUID
	CredentialPoolID uuid.UUID
	Priority         int
	Weight           int
	CommercialPolicy string
	Enabled          bool
	Metadata         map[string]string
}

type snapshotPayload struct {
	RouteID    uuid.UUID
	ModelID    uuid.UUID
	MatchType  string
	MatchValue string
	Targets    []snapshotTarget
}

func routeSnapshotHash(routeID, modelID uuid.UUID, matchType, matchValue string, targets []TargetInput) ([]byte, error) {
	payload := snapshotPayload{
		RouteID:    routeID,
		ModelID:    modelID,
		MatchType:  matchType,
		MatchValue: matchValue,
		Targets:    make([]snapshotTarget, len(targets)),
	}
	for index, target := range targets {
		payload.Targets[index] = snapshotTarget{
			TargetNo:         index,
			ProviderModelID:  target.ProviderModelID,
			CredentialPoolID: target.CredentialPoolID,
			Priority:         target.Priority,
			Weight:           target.Weight,
			CommercialPolicy: target.CommercialPolicy,
			Enabled:          target.Enabled,
			Metadata:         target.Metadata,
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode route snapshot: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hash[:], nil
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
		case "55000":
			return ErrConflict
		}
	}
	return err
}
