package scheduler

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

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

// Repository reads live credential-pool state and persists immutable decisions.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a Scheduler repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("scheduler repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

func (r *Repository) loadPoolMembers(ctx context.Context, poolID uuid.UUID, quota QuotaPolicy) ([]poolMember, error) {
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT pm.credential_id, pm.priority, pm.weight, pm.enabled,
			c.status, c.max_concurrency, c.region, c.proxy_ref,
			p.enabled, ch.cooldown_until,
			quota.remaining_tokens, quota.reset_at, quota.observed_at, quota.confidence
		FROM pool_members pm
		JOIN credentials c ON c.id = pm.credential_id AND c.deleted_at IS NULL
		JOIN providers p ON p.id = c.provider_id
		LEFT JOIN credential_health ch ON ch.credential_id = c.id
		LEFT JOIN LATERAL (
			SELECT qs.remaining_tokens, qs.reset_at, qs.observed_at, qs.confidence
			FROM quota_snapshots qs
			WHERE $2 <> '' AND qs.credential_id = c.id AND qs.window_kind = $2
			ORDER BY qs.observed_at DESC, qs.id DESC
			LIMIT 1
		) quota ON true
		WHERE pm.pool_id = $1
		ORDER BY pm.priority, pm.credential_id`, poolID, quota.WindowKind)
	if err != nil {
		return nil, fmt.Errorf("load scheduler pool members: %w", err)
	}
	defer rows.Close()

	members := make([]poolMember, 0)
	for rows.Next() {
		var member poolMember
		var cooldown, resetAt, observedAt pgtype.Timestamptz
		var remaining pgtype.Int8
		var confidence pgtype.Text
		if err := rows.Scan(
			&member.CredentialID,
			&member.MemberPriority,
			&member.MemberWeight,
			&member.Enabled,
			&member.Status,
			&member.MaxConcurrency,
			&member.Region,
			&member.ProxyRef,
			&member.ProviderEnabled,
			&cooldown,
			&remaining,
			&resetAt,
			&observedAt,
			&confidence,
		); err != nil {
			return nil, fmt.Errorf("scan scheduler pool member: %w", err)
		}
		member.CooldownUntil = nullableTime(cooldown)
		member.Quota = quotaSnapshot{
			Found:      remaining.Valid || resetAt.Valid || observedAt.Valid || confidence.Valid,
			Remaining:  nullableInt64(remaining),
			ResetAt:    nullableTime(resetAt),
			ObservedAt: nullableTime(observedAt),
		}
		if confidence.Valid {
			member.Quota.Confidence = confidence.String
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scheduler pool members: %w", err)
	}
	return members, nil
}

func (r *Repository) RecordDecision(ctx context.Context, request Request, decision Decision) error {
	candidates, err := json.Marshal(nonNilCandidates(decision.Candidates))
	if err != nil {
		return fmt.Errorf("encode scheduler candidates: %w", err)
	}
	fallback, err := json.Marshal(nonNilFallback(decision.FallbackChain))
	if err != nil {
		return fmt.Errorf("encode scheduler fallback chain: %w", err)
	}
	decisionID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate scheduler decision UUIDv7: %w", err)
	}
	var routeVersionID any
	if request.Resolution.Version.ID != uuid.Nil {
		routeVersionID = request.Resolution.Version.ID
	}
	var selectedTargetID, selectedProviderID, selectedCredentialID any
	if decision.SelectedTargetID != nil {
		selectedTargetID = *decision.SelectedTargetID
	}
	if decision.SelectedProviderID != nil {
		selectedProviderID = *decision.SelectedProviderID
	}
	if decision.SelectedCredentialID != nil {
		selectedCredentialID = *decision.SelectedCredentialID
	}
	_, err = r.store.Queryer().Exec(ctx, `
		INSERT INTO scheduler_decisions (
			id, request_record_id, request_id, attempt_no, decision_no,
			strategy_version, candidates, route_version_id, selected_target_id,
			selected_provider_id, selected_credential_id, fallback_chain
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		decisionID,
		request.RequestRecordID,
		request.RequestID,
		request.AttemptNo,
		request.DecisionNo,
		decision.StrategyVersion,
		candidates,
		routeVersionID,
		selectedTargetID,
		selectedProviderID,
		selectedCredentialID,
		fallback,
	)
	if err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func nonNilCandidates(values []CandidateDecision) []CandidateDecision {
	if values == nil {
		return []CandidateDecision{}
	}
	return values
}

func nonNilFallback(values []FallbackEntry) []FallbackEntry {
	if values == nil {
		return []FallbackEntry{}
	}
	return values
}

func nullableTime(value pgtype.Timestamptz) *time.Time {
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
		return ErrNoEligible
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
