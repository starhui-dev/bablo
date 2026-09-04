package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

// Persistence is the durable boundary used by Service. Implementations must
// keep snapshot inserts and the corresponding probe-state transition atomic.
type Persistence interface {
	PersistObservation(context.Context, ProbeRequest, Observation, ProbeState) error
	UpsertProbeState(context.Context, ProbeState) error
	GetProbeState(context.Context, uuid.UUID) (ProbeState, bool, error)
	ListSnapshots(context.Context, uuid.UUID, string, int) ([]Snapshot, error)
	GetCredentialRef(context.Context, uuid.UUID) (ProbeRequest, error)
	ListDue(context.Context, time.Time, int) ([]DueCredential, error)
}

// Repository persists immutable quota snapshots and rebuildable probe state.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a quota repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("quota repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

var _ Persistence = (*Repository)(nil)

// PersistObservation appends all windows and advances probe state in one
// transaction. No UPDATE/DELETE is ever issued against quota_snapshots.
func (r *Repository) PersistObservation(ctx context.Context, request ProbeRequest, observation Observation, state ProbeState) error {
	if r == nil || r.store == nil || request.CredentialID == uuid.Nil || len(observation.Windows) == 0 {
		return ErrInvalidInput
	}
	request.ProviderSlug = strings.ToLower(strings.TrimSpace(request.ProviderSlug))
	request.Model = strings.TrimSpace(request.Model)
	observation.ObservedAt = observation.ObservedAt.UTC()
	observation.Source = strings.TrimSpace(observation.Source)
	observation.Confidence = strings.ToLower(strings.TrimSpace(observation.Confidence))
	observation.ErrorClass = strings.TrimSpace(observation.ErrorClass)
	observation.ObservationKey = strings.TrimSpace(observation.ObservationKey)
	if request.ProviderSlug == "" || len(request.ProviderSlug) > maxProviderSlugBytes || containsControl(request.ProviderSlug) ||
		len(request.Model) > 255 || containsControl(request.Model) ||
		observation.ObservedAt.IsZero() || observation.Source == "" || len(observation.Source) > 120 || containsControl(observation.Source) ||
		!validConfidence(observation.Confidence) || observation.ObservationKey == "" || len(observation.ObservationKey) > maxObservationKeyBytes || containsControl(observation.ObservationKey) ||
		(observation.ErrorClass != "" && (len(observation.ErrorClass) > 80 || containsControl(observation.ErrorClass))) ||
		state.CredentialID != request.CredentialID || strings.ToLower(strings.TrimSpace(state.ProviderSlug)) != request.ProviderSlug {
		return ErrInvalidInput
	}
	metadataMap, err := normalizeObservationMetadata(observation.Metadata)
	if err != nil {
		return err
	}
	observation.Metadata = metadataMap
	metadata, err := json.Marshal(metadataMap)
	if err != nil {
		return fmt.Errorf("encode quota observation metadata: %w", err)
	}
	if len(metadata) == 0 || string(metadata) == "null" {
		metadata = []byte("{}")
	}
	if len(metadata) > maxObservationMetadataBytes {
		return ErrInvalidInput
	}
	observationKey := observation.ObservationKey
	return r.store.WithTx(ctx, func(q data.Querier) error {
		for _, originalWindow := range observation.Windows {
			window := cloneWindow(originalWindow)
			window.Kind = strings.ToLower(strings.TrimSpace(window.Kind))
			if !validWindowKind(window.Kind) {
				return ErrInvalidInput
			}
			for _, value := range []*int64{window.UsedTokens, window.RemainingTokens, window.LimitTokens} {
				if value != nil && *value < 0 {
					return ErrInvalidInput
				}
			}
			window.Metadata, err = normalizeObservationMetadata(window.Metadata)
			if err != nil {
				return err
			}
			windowMetadata := metadata
			if len(window.Metadata) > 0 {
				merged := make(map[string]string, len(observation.Metadata)+len(window.Metadata))
				for key, value := range observation.Metadata {
					merged[key] = value
				}
				for key, value := range window.Metadata {
					merged[key] = value
				}
				windowMetadata, err = json.Marshal(merged)
				if err != nil {
					return fmt.Errorf("encode quota window metadata: %w", err)
				}
				if len(windowMetadata) > maxObservationMetadataBytes {
					return ErrInvalidInput
				}
			}
			snapshotID, idErr := id.New()
			if idErr != nil {
				return fmt.Errorf("generate quota snapshot UUIDv7: %w", idErr)
			}
			tag, execErr := q.Exec(ctx, `
				INSERT INTO quota_snapshots (
					id, credential_id, provider_slug, model, observation_key,
					window_kind, used_tokens, remaining_tokens, limit_tokens,
					reset_at, observed_at, source, confidence, error_class, metadata
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
				ON CONFLICT (credential_id, observation_key, window_kind) DO NOTHING`,
				snapshotID,
				request.CredentialID,
				request.ProviderSlug,
				request.Model,
				observationKey,
				window.Kind,
				window.UsedTokens,
				window.RemainingTokens,
				window.LimitTokens,
				window.ResetAt,
				observation.ObservedAt,
				observation.Source,
				observation.Confidence,
				nullableText(observation.ErrorClass),
				windowMetadata,
			)
			if execErr != nil {
				return mapRepositoryError(execErr)
			}
			if tag.RowsAffected() == 0 {
				var same bool
				if scanErr := q.QueryRow(ctx, `
					SELECT provider_slug = $4
						AND model = $5
						AND used_tokens IS NOT DISTINCT FROM $6
						AND remaining_tokens IS NOT DISTINCT FROM $7
						AND limit_tokens IS NOT DISTINCT FROM $8
						AND reset_at IS NOT DISTINCT FROM $9
						AND observed_at = $10
						AND source = $11
						AND confidence = $12
						AND error_class IS NOT DISTINCT FROM $13
						AND metadata = $14::jsonb
					FROM quota_snapshots
					WHERE credential_id = $1 AND observation_key = $2 AND window_kind = $3`,
					request.CredentialID,
					observationKey,
					window.Kind,
					request.ProviderSlug,
					request.Model,
					window.UsedTokens,
					window.RemainingTokens,
					window.LimitTokens,
					window.ResetAt,
					observation.ObservedAt,
					observation.Source,
					observation.Confidence,
					nullableText(observation.ErrorClass),
					windowMetadata,
				).Scan(&same); scanErr != nil {
					if errors.Is(scanErr, pgx.ErrNoRows) {
						return ErrConflict
					}
					return mapRepositoryError(scanErr)
				}
				if !same {
					return ErrConflict
				}
			}
		}
		return r.upsertProbeState(ctx, q, state)
	})
}

// UpsertProbeState persists rebuildable worker state. Newer updated_at values
// win, so a late result cannot overwrite a newer transition.
func (r *Repository) UpsertProbeState(ctx context.Context, state ProbeState) error {
	if r == nil || r.store == nil || state.CredentialID == uuid.Nil {
		return ErrInvalidInput
	}
	return r.store.WithTx(ctx, func(q data.Querier) error {
		return r.upsertProbeState(ctx, q, state)
	})
}

func (r *Repository) upsertProbeState(ctx context.Context, q data.Querier, state ProbeState) error {
	state.ProviderSlug = strings.ToLower(strings.TrimSpace(state.ProviderSlug))
	state.ProbeName = strings.ToLower(strings.TrimSpace(state.ProbeName))
	state.Status = strings.ToLower(strings.TrimSpace(state.Status))
	state.LastErrorClass = strings.ToLower(strings.TrimSpace(state.LastErrorClass))
	updatedAt := state.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	state.UpdatedAt = updatedAt
	if state.ProviderSlug == "" || len(state.ProviderSlug) > maxProviderSlugBytes || containsControl(state.ProviderSlug) ||
		len(state.ProbeName) > 120 || containsControl(state.ProbeName) || !validProbeStatus(state.Status) || state.FailureCount < 0 ||
		(state.LastErrorClass != "" && (len(state.LastErrorClass) > 80 || containsControl(state.LastErrorClass))) ||
		(state.LastHTTPStatus != 0 && (state.LastHTTPStatus < 100 || state.LastHTTPStatus > 599)) {
		return ErrInvalidInput
	}
	_, err := q.Exec(ctx, `
		INSERT INTO quota_probe_states (
			credential_id, provider_slug, probe_name, status, last_attempt_at,
			last_observed_at, next_attempt_at, failure_count, last_error_class,
			last_http_status, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (credential_id) DO UPDATE SET
			provider_slug = EXCLUDED.provider_slug,
			probe_name = EXCLUDED.probe_name,
			status = EXCLUDED.status,
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_observed_at = EXCLUDED.last_observed_at,
			next_attempt_at = EXCLUDED.next_attempt_at,
			failure_count = EXCLUDED.failure_count,
			last_error_class = EXCLUDED.last_error_class,
			last_http_status = EXCLUDED.last_http_status,
			updated_at = EXCLUDED.updated_at
		WHERE quota_probe_states.updated_at <= EXCLUDED.updated_at`,
		state.CredentialID,
		state.ProviderSlug,
		state.ProbeName,
		state.Status,
		nullableTime(state.LastAttemptAt),
		nullableTime(state.LastObservedAt),
		nullableTime(state.NextAttemptAt),
		state.FailureCount,
		nullableText(state.LastErrorClass),
		nullableInt(state.LastHTTPStatus),
		updatedAt,
	)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

// GetProbeState returns false when the worker has not written a state row.
func (r *Repository) GetProbeState(ctx context.Context, credentialID uuid.UUID) (ProbeState, bool, error) {
	if r == nil || r.store == nil || credentialID == uuid.Nil {
		return ProbeState{}, false, ErrInvalidInput
	}
	var state ProbeState
	var lastAttempt, lastObserved, nextAttempt pgtype.Timestamptz
	var lastError pgtype.Text
	var lastStatus pgtype.Int4
	err := r.store.Queryer().QueryRow(ctx, `
        SELECT credential_id, provider_slug, probe_name, status,
            last_attempt_at, last_observed_at, next_attempt_at, failure_count,
            last_error_class, last_http_status, updated_at
        FROM quota_probe_states
        WHERE credential_id = $1`, credentialID).Scan(
		&state.CredentialID,
		&state.ProviderSlug,
		&state.ProbeName,
		&state.Status,
		&lastAttempt,
		&lastObserved,
		&nextAttempt,
		&state.FailureCount,
		&lastError,
		&lastStatus,
		&state.UpdatedAt,
	)
	if lastError.Valid {
		state.LastErrorClass = lastError.String
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ProbeState{}, false, nil
	}
	if err != nil {
		return ProbeState{}, false, mapRepositoryError(err)
	}
	state.LastAttemptAt = nullableTimeFromPG(lastAttempt)
	state.LastObservedAt = nullableTimeFromPG(lastObserved)
	state.NextAttemptAt = nullableTimeFromPG(nextAttempt)
	if lastStatus.Valid {
		state.LastHTTPStatus = int(lastStatus.Int32)
	}
	return state, true, nil
}

// ListSnapshots returns newest observations first. A blank window kind returns
// all supported windows; callers still receive an explicit bounded result.
func (r *Repository) ListSnapshots(ctx context.Context, credentialID uuid.UUID, windowKind string, limit int) ([]Snapshot, error) {
	if r == nil || r.store == nil || credentialID == uuid.Nil || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	if windowKind != "" && !validWindowKind(windowKind) {
		return nil, ErrInvalidInput
	}
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT id, credential_id, provider_slug, model, observation_key,
			window_kind, used_tokens, remaining_tokens, limit_tokens, reset_at,
			observed_at, source, confidence, error_class, metadata
		FROM quota_snapshots
		WHERE credential_id = $1 AND ($2 = '' OR window_kind = $2)
		ORDER BY observed_at DESC, id DESC
		LIMIT $3`, credentialID, windowKind, limit)
	if err != nil {
		return nil, fmt.Errorf("list quota snapshots: %w", err)
	}
	defer rows.Close()
	result := make([]Snapshot, 0, limit)
	for rows.Next() {
		var value Snapshot
		var resetAt pgtype.Timestamptz
		var used, remaining, maximum pgtype.Int8
		var errorClass pgtype.Text
		var metadataJSON []byte
		if err := rows.Scan(
			&value.ID,
			&value.CredentialID,
			&value.ProviderSlug,
			&value.Model,
			&value.ObservationKey,
			&value.WindowKind,
			&used,
			&remaining,
			&maximum,
			&resetAt,
			&value.ObservedAt,
			&value.Source,
			&value.Confidence,
			&errorClass,
			&metadataJSON,
		); err != nil {
			return nil, fmt.Errorf("scan quota snapshot: %w", err)
		}
		value.UsedTokens = nullableInt64(used)
		value.RemainingTokens = nullableInt64(remaining)
		value.LimitTokens = nullableInt64(maximum)
		value.ResetAt = nullableTimeFromPG(resetAt)
		if errorClass.Valid {
			value.ErrorClass = errorClass.String
		}
		value.Metadata = map[string]string{}
		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &value.Metadata); err != nil {
				return nil, fmt.Errorf("decode quota snapshot metadata: %w", err)
			}
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quota snapshots: %w", err)
	}
	return result, nil
}

// GetCredentialRef resolves provider identity from PostgreSQL, not from a
// client-supplied provider string.
func (r *Repository) GetCredentialRef(ctx context.Context, credentialID uuid.UUID) (ProbeRequest, error) {
	if r == nil || r.store == nil || credentialID == uuid.Nil {
		return ProbeRequest{}, ErrInvalidInput
	}
	var request ProbeRequest
	err := r.store.Queryer().QueryRow(ctx, `
		SELECT c.id, p.slug
		FROM credentials c
		JOIN providers p ON p.id = c.provider_id
		WHERE c.id = $1 AND c.deleted_at IS NULL`, credentialID).Scan(&request.CredentialID, &request.ProviderSlug)
	if err != nil {
		return ProbeRequest{}, mapRepositoryError(err)
	}
	return request, nil
}

// ListDue returns active credentials whose next attempt has arrived. A missing
// state row is intentionally due immediately so new credentials are observed.
func (r *Repository) ListDue(ctx context.Context, now time.Time, limit int) ([]DueCredential, error) {
	if r == nil || r.store == nil || limit < 1 || limit > 1000 {
		return nil, ErrInvalidInput
	}
	now = now.UTC()
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT c.id, p.slug, s.next_attempt_at
		FROM credentials c
		JOIN providers p ON p.id = c.provider_id
		LEFT JOIN quota_probe_states s ON s.credential_id = c.id
		WHERE c.status = 'active' AND c.deleted_at IS NULL
		  AND p.enabled = true AND p.deleted_at IS NULL
		  AND (s.credential_id IS NULL OR s.next_attempt_at IS NULL OR s.next_attempt_at <= $1)
		ORDER BY COALESCE(s.next_attempt_at, '-infinity'::timestamptz), c.id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due quota probes: %w", err)
	}
	defer rows.Close()
	result := make([]DueCredential, 0, limit)
	for rows.Next() {
		var value DueCredential
		var next pgtype.Timestamptz
		if err := rows.Scan(&value.Request.CredentialID, &value.Request.ProviderSlug, &next); err != nil {
			return nil, fmt.Errorf("scan due quota probe: %w", err)
		}
		value.NextAttemptAt = nullableTimeFromPG(next)
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due quota probes: %w", err)
	}
	return result, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableTimeFromPG(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func nullableInt64(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
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
