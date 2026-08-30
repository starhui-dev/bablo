package pricing

import (
	"context"
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

// Repository persists immutable price bundles in PostgreSQL.
type Repository struct {
	store *data.Store
}

// NewRepository constructs a pricing repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("pricing repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

type rowScanner interface {
	Scan(...any) error
}

const versionColumns = `
	id, scope, version_no, currency, effective_from, effective_to,
	status, created_by, created_at`

func scanVersion(row rowScanner) (Version, error) {
	var value Version
	var effectiveTo pgtype.Timestamptz
	var createdBy pgtype.UUID
	if err := row.Scan(
		&value.ID,
		&value.Scope,
		&value.VersionNo,
		&value.Currency,
		&value.EffectiveFrom,
		&effectiveTo,
		&value.Status,
		&createdBy,
		&value.CreatedAt,
	); err != nil {
		return Version{}, err
	}
	if effectiveTo.Valid {
		parsed := effectiveTo.Time
		value.EffectiveTo = &parsed
	}
	if createdBy.Valid {
		value.CreatedBy = uuid.UUID(createdBy.Bytes)
	}
	value.Prices = []Entry{}
	return value, nil
}

func (repository *Repository) create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Version, error) {
	versionID, err := id.New()
	if err != nil {
		return Version{}, fmt.Errorf("generate price version UUIDv7: %w", err)
	}
	var created Version
	err = repository.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('price-version:' || $1, 0))`, input.Scope); err != nil {
			return fmt.Errorf("lock price version scope: %w", err)
		}
		var versionNo int64
		if err := q.QueryRow(ctx, `
			SELECT COALESCE(MAX(version_no), 0) + 1
			FROM price_versions
			WHERE scope = $1`, input.Scope).Scan(&versionNo); err != nil {
			return fmt.Errorf("allocate price version number: %w", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO price_versions (
				id, scope, version_no, currency, effective_from, effective_to, status, created_by
			)
			VALUES ($1, $2, $3, $4, $5, $6, 'draft', $7)`,
			versionID,
			input.Scope,
			versionNo,
			input.Currency,
			input.EffectiveFrom,
			input.EffectiveTo,
			actorID,
		); err != nil {
			return mapRepositoryError(err)
		}
		for _, price := range input.Prices {
			entryID, err := id.New()
			if err != nil {
				return fmt.Errorf("generate model price UUIDv7: %w", err)
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO model_prices (
					id, price_version_id, pricing_scope, model_id, provider_model_id, dimension, unit_price
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7::numeric)`,
				entryID,
				versionID,
				input.Scope,
				price.ModelID,
				price.ProviderModelID,
				price.Dimension,
				price.UnitPrice,
			); err != nil {
				return mapRepositoryError(err)
			}
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "price_version.create",
			TargetType:  "price_version",
			TargetID:    versionID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		created, err = getVersion(ctx, q, versionID, false)
		return err
	})
	if err != nil {
		return Version{}, err
	}
	return created, nil
}

func (repository *Repository) get(ctx context.Context, versionID uuid.UUID) (Version, error) {
	return getVersion(ctx, repository.store.Queryer(), versionID, false)
}

func getVersion(ctx context.Context, q data.Querier, versionID uuid.UUID, lock bool) (Version, error) {
	query := `SELECT ` + versionColumns + ` FROM price_versions WHERE id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	value, err := scanVersion(q.QueryRow(ctx, query, versionID))
	if err != nil {
		return Version{}, mapRepositoryError(err)
	}
	if err := loadEntries(ctx, q, &value); err != nil {
		return Version{}, err
	}
	return value, nil
}

func loadEntries(ctx context.Context, q data.Querier, version *Version) error {
	rows, err := q.Query(ctx, `
		SELECT id, pricing_scope, model_id, provider_model_id, dimension, unit_price::text
		FROM model_prices
		WHERE price_version_id = $1
		ORDER BY pricing_scope, model_id, provider_model_id, dimension`, version.ID)
	if err != nil {
		return fmt.Errorf("list price entries: %w", err)
	}
	defer rows.Close()
	version.Prices = []Entry{}
	for rows.Next() {
		var entry Entry
		var modelID, providerModelID pgtype.UUID
		if err := rows.Scan(
			&entry.ID,
			&entry.PricingScope,
			&modelID,
			&providerModelID,
			&entry.Dimension,
			&entry.UnitPrice,
		); err != nil {
			return fmt.Errorf("scan price entry: %w", err)
		}
		amount, err := normalizeAmount(entry.UnitPrice)
		if err != nil {
			return fmt.Errorf("normalize stored price: %w", err)
		}
		entry.UnitPrice = amount
		if modelID.Valid {
			parsed := uuid.UUID(modelID.Bytes)
			entry.ModelID = &parsed
		}
		if providerModelID.Valid {
			parsed := uuid.UUID(providerModelID.Bytes)
			entry.ProviderModelID = &parsed
		}
		version.Prices = append(version.Prices, entry)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate price entries: %w", err)
	}
	return nil
}

func (repository *Repository) list(ctx context.Context, scope string, cursor int64, limit int) (Page, error) {
	if cursor == 0 {
		cursor = int64(^uint64(0) >> 1)
	}
	rows, err := repository.store.Queryer().Query(ctx, `
		SELECT `+versionColumns+`
		FROM price_versions
		WHERE scope = $1 AND version_no < $2
		ORDER BY version_no DESC
		LIMIT $3`, scope, cursor, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list price versions: %w", err)
	}
	defer rows.Close()
	versions := make([]Version, 0, limit+1)
	for rows.Next() {
		value, err := scanVersion(rows)
		if err != nil {
			return Page{}, fmt.Errorf("scan price version: %w", err)
		}
		versions = append(versions, value)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate price versions: %w", err)
	}
	page := Page{Versions: versions}
	if len(versions) > limit {
		page.NextCursor = versions[limit-1].VersionNo
		page.Versions = versions[:limit]
	}
	for index := range page.Versions {
		if err := loadEntries(ctx, repository.store.Queryer(), &page.Versions[index]); err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func (repository *Repository) activate(ctx context.Context, actorID, versionID uuid.UUID, requestID string) (Version, error) {
	var activated Version
	err := repository.store.WithTx(ctx, func(q data.Querier) error {
		current, err := getVersion(ctx, q, versionID, true)
		if err != nil {
			return err
		}
		if current.Status == StatusActive {
			activated = current
			return nil
		}
		if current.Status != StatusDraft || len(current.Prices) == 0 {
			return ErrConflict
		}
		if _, err := q.Exec(ctx, `
			UPDATE price_versions SET status = 'active' WHERE id = $1`, versionID); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "price_version.activate",
			TargetType:  "price_version",
			TargetID:    versionID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		activated, err = getVersion(ctx, q, versionID, false)
		return err
	})
	if err != nil {
		return Version{}, err
	}
	return activated, nil
}

func (repository *Repository) retire(ctx context.Context, actorID, versionID uuid.UUID, retiredAt time.Time, requestID string) (Version, error) {
	var retired Version
	err := repository.store.WithTx(ctx, func(q data.Querier) error {
		current, err := getVersion(ctx, q, versionID, true)
		if err != nil {
			return err
		}
		if current.Status == StatusRetired {
			retired = current
			return nil
		}
		if current.Status != StatusActive || !retiredAt.After(current.EffectiveFrom) {
			return ErrConflict
		}
		if _, err := q.Exec(ctx, `
			UPDATE price_versions
			SET status = 'retired', effective_to = $2
			WHERE id = $1`, versionID, retiredAt); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "price_version.retire",
			TargetType:  "price_version",
			TargetID:    versionID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		retired, err = getVersion(ctx, q, versionID, false)
		return err
	})
	if err != nil {
		return Version{}, err
	}
	return retired, nil
}

func (repository *Repository) billingClass(ctx context.Context, modelID uuid.UUID) (string, error) {
	var billingClass string
	if err := repository.store.Queryer().QueryRow(ctx, `
		SELECT billing_class
		FROM models
		WHERE id = $1 AND enabled AND deleted_at IS NULL`, modelID).Scan(&billingClass); err != nil {
		return "", mapRepositoryError(err)
	}
	return billingClass, nil
}

func (repository *Repository) resolve(ctx context.Context, scope string, modelID, providerModelID uuid.UUID, at time.Time) (Snapshot, bool, error) {
	query := `
		SELECT pv.id, pv.currency, pv.effective_from
		FROM price_versions pv
		WHERE pv.scope = $1
		  AND pv.status IN ('active', 'retired')
		  AND pv.effective_from <= $2
		  AND (pv.effective_to IS NULL OR pv.effective_to > $2)
		  AND EXISTS (
			SELECT 1
			FROM model_prices mp
			WHERE mp.price_version_id = pv.id
			  AND mp.pricing_scope = pv.scope
			  AND (($1 = 'global' AND mp.model_id IS NULL AND mp.provider_model_id IS NULL)
			    OR ($1 = 'model' AND mp.model_id = $3 AND mp.provider_model_id IS NULL)
			    OR ($1 = 'provider_model' AND mp.provider_model_id = $4 AND mp.model_id IS NULL))
		  )
		ORDER BY pv.version_no DESC
		LIMIT 1`
	var snapshot Snapshot
	if err := repository.store.Queryer().QueryRow(ctx, query, scope, at, modelID, providerModelID).Scan(
		&snapshot.VersionID,
		&snapshot.Currency,
		&snapshot.EffectiveFrom,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, fmt.Errorf("resolve price version: %w", err)
	}
	snapshot.Scope = scope
	snapshot.Prices = make(map[string]string)
	rows, err := repository.store.Queryer().Query(ctx, `
		SELECT dimension, unit_price::text
		FROM model_prices
		WHERE price_version_id = $1
		  AND (($2 = 'global' AND model_id IS NULL AND provider_model_id IS NULL)
		    OR ($2 = 'model' AND model_id = $3 AND provider_model_id IS NULL)
		    OR ($2 = 'provider_model' AND provider_model_id = $4 AND model_id IS NULL))
		ORDER BY dimension`, snapshot.VersionID, scope, modelID, providerModelID)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("resolve price entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dimension, amount string
		if err := rows.Scan(&dimension, &amount); err != nil {
			return Snapshot{}, false, fmt.Errorf("scan resolved price: %w", err)
		}
		amount, err = normalizeAmount(amount)
		if err != nil {
			return Snapshot{}, false, fmt.Errorf("normalize resolved price: %w", err)
		}
		snapshot.Prices[dimension] = amount
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, false, fmt.Errorf("iterate resolved prices: %w", err)
	}
	return snapshot, true, nil
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
