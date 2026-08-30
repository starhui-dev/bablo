package credential

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

	"github.com/starhui-dev/bablo/internal/audit"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
	"github.com/starhui-dev/bablo/internal/secret"
)

// Repository persists credential metadata and encrypted secret versions.
type Repository struct {
	store   *data.Store
	keyring *secret.Keyring
}

// NewRepository constructs a Credential repository.
func NewRepository(store *data.Store, keyring *secret.Keyring) (*Repository, error) {
	if store == nil || store.Queryer() == nil || keyring == nil || keyring.CurrentVersion() == "" {
		return nil, errors.New("credential repository requires an initialized database store and keyring")
	}
	return &Repository{store: store, keyring: keyring}, nil
}

type rowScanner interface {
	Scan(...any) error
}

const credentialColumns = `
	c.id, c.provider_id, p.slug, p.resource_type, p.commercial_allowed,
	c.external_stable_id, c.source_kind, c.status, c.region, c.proxy_ref, c.metadata,
	c.created_at, c.updated_at`

func scanCredential(row rowScanner) (Credential, error) {
	var value Credential
	var metadataJSON []byte
	if err := row.Scan(
		&value.ID,
		&value.ProviderID,
		&value.ProviderSlug,
		&value.ResourceType,
		&value.CommercialAllowed,
		&value.ExternalStableID,
		&value.SourceKind,
		&value.Status,
		&value.Region,
		&value.ProxyRef,
		&metadataJSON,
		&value.CreatedAt,
		&value.UpdatedAt,
	); err != nil {
		return Credential{}, err
	}
	value.Metadata = map[string]string{}
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &value.Metadata); err != nil {
			return Credential{}, fmt.Errorf("decode credential metadata: %w", err)
		}
	}
	value.Secrets = []SecretDescriptor{}
	value.Pools = []PoolMembership{}
	value.Health = Health{Metadata: map[string]string{}}
	return value, nil
}

func (r *Repository) create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Credential, error) {
	credentialID, err := id.New()
	if err != nil {
		return Credential{}, fmt.Errorf("generate credential UUIDv7: %w", err)
	}
	sealed, err := r.sealInputs(credentialID, input.Secrets)
	if err != nil {
		return Credential{}, err
	}
	metadataJSON, err := json.Marshal(input.Metadata)
	if err != nil {
		return Credential{}, fmt.Errorf("encode credential metadata: %w", err)
	}
	created := Credential{ID: credentialID}
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO credentials (
				id, provider_id, external_stable_id, source_kind, status, region, proxy_ref, metadata
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			credentialID,
			input.ProviderID,
			input.ExternalStableID,
			input.SourceKind,
			statusForCreate(input.Enabled),
			input.Region,
			input.ProxyRef,
			metadataJSON,
		); err != nil {
			return mapRepositoryError(err)
		}
		for _, item := range sealed {
			if _, err := q.Exec(ctx, `
				INSERT INTO credential_secrets (
					id, credential_id, secret_kind, version_no, ciphertext, nonce, key_version
				)
				VALUES ($1, $2, $3, 1, $4, $5, $6)`,
				item.ID, credentialID, item.Kind, item.Ciphertext, item.Nonce, item.KeyVersion,
			); err != nil {
				return mapRepositoryError(err)
			}
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO credential_health (credential_id)
			VALUES ($1)
			ON CONFLICT (credential_id) DO NOTHING`, credentialID); err != nil {
			return fmt.Errorf("initialize credential health: %w", err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "credential.created",
			TargetType:  "credential",
			TargetID:    credentialID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		created, err = r.get(ctx, q, credentialID, false)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return created, nil
}

func statusForCreate(enabled bool) string {
	if enabled {
		return StatusActive
	}
	return StatusDisabled
}

func (r *Repository) get(ctx context.Context, q data.Querier, credentialID uuid.UUID, lock bool) (Credential, error) {
	query := `SELECT ` + credentialColumns + `
		FROM credentials c JOIN providers p ON p.id = c.provider_id
		WHERE c.id = $1 AND c.deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE OF c`
	}
	value, err := scanCredential(q.QueryRow(ctx, query, credentialID))
	if err != nil {
		return Credential{}, mapRepositoryError(err)
	}
	if err := loadDetails(ctx, q, &value); err != nil {
		return Credential{}, err
	}
	return value, nil
}

func (r *Repository) Get(ctx context.Context, credentialID uuid.UUID) (Credential, error) {
	return r.get(ctx, r.store.Queryer(), credentialID, false)
}

func (r *Repository) list(ctx context.Context, cursor string, limit int) (Page, error) {
	stableID := ""
	credentialID := uuid.Nil
	var err error
	if cursor != "" {
		stableID, credentialID, err = parseListCursor(cursor)
		if err != nil {
			return Page{}, ErrInvalidInput
		}
	}
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT `+credentialColumns+`
		FROM credentials c JOIN providers p ON p.id = c.provider_id
		WHERE c.deleted_at IS NULL
		  AND ($1 = '' OR c.external_stable_id > $1 OR (c.external_stable_id = $1 AND c.id > $2))
		ORDER BY c.external_stable_id, c.id
		LIMIT $3`, stableID, credentialID, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()
	values := make([]Credential, 0, limit+1)
	for rows.Next() {
		value, err := scanCredential(rows)
		if err != nil {
			return Page{}, fmt.Errorf("scan credential: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate credentials: %w", err)
	}
	page := Page{Credentials: values}
	if len(values) > limit {
		page.NextCursor = formatListCursor(values[limit-1])
		page.Credentials = values[:limit]
	}
	for index := range page.Credentials {
		if err := loadDetails(ctx, r.store.Queryer(), &page.Credentials[index]); err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func loadDetails(ctx context.Context, q data.Querier, value *Credential) error {
	if err := loadSecrets(ctx, q, value); err != nil {
		return err
	}
	if err := loadPools(ctx, q, value); err != nil {
		return err
	}
	return loadHealth(ctx, q, value)
}

func loadSecrets(ctx context.Context, q data.Querier, value *Credential) error {
	rows, err := q.Query(ctx, `
		SELECT id, secret_kind, version_no, key_version, created_at, rotated_at
		FROM credential_secrets
		WHERE credential_id = $1
		ORDER BY secret_kind, version_no DESC`, value.ID)
	if err != nil {
		return fmt.Errorf("list credential secrets: %w", err)
	}
	defer rows.Close()
	value.Secrets = make([]SecretDescriptor, 0)
	for rows.Next() {
		var item SecretDescriptor
		if err := rows.Scan(&item.ID, &item.Kind, &item.VersionNo, &item.KeyVersion, &item.CreatedAt, &item.RotatedAt); err != nil {
			return fmt.Errorf("scan credential secret descriptor: %w", err)
		}
		value.Secrets = append(value.Secrets, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate credential secrets: %w", err)
	}
	return nil
}

func loadPools(ctx context.Context, q data.Querier, value *Credential) error {
	rows, err := q.Query(ctx, `
		SELECT pm.pool_id, p.name, pm.priority, pm.weight, pm.enabled
		FROM pool_members pm JOIN credential_pools p ON p.id = pm.pool_id
		WHERE pm.credential_id = $1
		ORDER BY pm.priority, p.name, pm.pool_id`, value.ID)
	if err != nil {
		return fmt.Errorf("list credential pools: %w", err)
	}
	defer rows.Close()
	value.Pools = make([]PoolMembership, 0)
	for rows.Next() {
		var item PoolMembership
		if err := rows.Scan(&item.PoolID, &item.PoolName, &item.Priority, &item.Weight, &item.Enabled); err != nil {
			return fmt.Errorf("scan credential pool membership: %w", err)
		}
		value.Pools = append(value.Pools, item)
	}
	return rows.Err()
}

func loadHealth(ctx context.Context, q data.Querier, value *Credential) error {
	var health Health
	var lastSuccess, lastError, cooldown pgtype.Timestamptz
	var lastErrorClass *string
	var metadataJSON []byte
	err := q.QueryRow(ctx, `
		SELECT last_success_at, last_error_at, last_error_class, cooldown_until, observed_at, metadata
		FROM credential_health WHERE credential_id = $1`, value.ID).Scan(
		&lastSuccess, &lastError, &lastErrorClass, &cooldown, &health.ObservedAt, &metadataJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		value.Health = Health{Metadata: map[string]string{}}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load credential health: %w", err)
	}
	if lastErrorClass != nil {
		health.LastErrorClass = *lastErrorClass
	}
	health.LastSuccessAt = timestamptzPointer(lastSuccess)
	health.LastErrorAt = timestamptzPointer(lastError)
	health.CooldownUntil = timestamptzPointer(cooldown)
	health.Metadata = map[string]string{}
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &health.Metadata); err != nil {
			return fmt.Errorf("decode credential health metadata: %w", err)
		}
	}
	value.Health = health
	return nil
}

func timestamptzPointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func (r *Repository) update(ctx context.Context, actorID, credentialID uuid.UUID, input UpdateInput, requestID string) (Credential, error) {
	var updated Credential
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		current, err := r.get(ctx, q, credentialID, true)
		if err != nil {
			return err
		}
		if input.Region != nil {
			current.Region = *input.Region
		}
		if input.ProxyRef != nil {
			current.ProxyRef = *input.ProxyRef
		}
		if input.Metadata != nil {
			current.Metadata = cloneMetadata(*input.Metadata)
		}
		if input.Status != nil {
			current.Status = *input.Status
		}
		metadataJSON, err := json.Marshal(current.Metadata)
		if err != nil {
			return fmt.Errorf("encode credential metadata: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE credentials
			SET status = $2, region = $3, proxy_ref = $4, metadata = $5, updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL`, credentialID, current.Status, current.Region, current.ProxyRef, metadataJSON); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "credential.updated",
			TargetType:  "credential",
			TargetID:    credentialID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated, err = r.get(ctx, q, credentialID, false)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return updated, nil
}

type sealedSecret struct {
	ID         uuid.UUID
	Kind       string
	Ciphertext []byte
	Nonce      []byte
	KeyVersion string
}

type storedSecret struct {
	ID           uuid.UUID
	CredentialID uuid.UUID
	Kind         string
	VersionNo    int64
	Ciphertext   []byte
	Nonce        []byte
	KeyVersion   string
}

func (r *Repository) reveal(ctx context.Context, credentialID uuid.UUID, kind string, requireActive bool) (storedSecret, Credential, error) {
	var result storedSecret
	var owner Credential
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		var err error
		owner, err = r.get(ctx, q, credentialID, true)
		if err != nil {
			return err
		}
		if requireActive && owner.Status != StatusActive {
			return ErrCredentialInactive
		}
		err = q.QueryRow(ctx, `
			SELECT id, credential_id, secret_kind, version_no, ciphertext, nonce, key_version
			FROM credential_secrets
			WHERE credential_id = $1 AND secret_kind = $2 AND rotated_at IS NULL`, credentialID, kind).Scan(
			&result.ID, &result.CredentialID, &result.Kind, &result.VersionNo, &result.Ciphertext, &result.Nonce, &result.KeyVersion,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSecretUnavailable
		}
		return err
	})
	if err != nil {
		return storedSecret{}, Credential{}, err
	}
	return result, owner, nil
}

func (r *Repository) loadRuntime(ctx context.Context, credentialID uuid.UUID) ([]storedSecret, Credential, error) {
	var values []storedSecret
	var owner Credential
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		var err error
		owner, err = r.get(ctx, q, credentialID, true)
		if err != nil {
			return err
		}
		if owner.Status != StatusActive {
			return ErrCredentialInactive
		}
		rows, err := q.Query(ctx, `
			SELECT id, credential_id, secret_kind, version_no, ciphertext, nonce, key_version
			FROM credential_secrets
			WHERE credential_id = $1 AND rotated_at IS NULL
			ORDER BY secret_kind`, credentialID)
		if err != nil {
			return fmt.Errorf("load active credential secrets: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value storedSecret
			if err := rows.Scan(&value.ID, &value.CredentialID, &value.Kind, &value.VersionNo, &value.Ciphertext, &value.Nonce, &value.KeyVersion); err != nil {
				return fmt.Errorf("scan active credential secret: %w", err)
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate active credential secrets: %w", err)
		}
		if len(values) == 0 {
			return ErrSecretUnavailable
		}
		return nil
	})
	if err != nil {
		return nil, Credential{}, err
	}
	return values, owner, nil
}

func (r *Repository) activeCredentialIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT id FROM credentials
		WHERE status = 'active' AND deleted_at IS NULL
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list active credential IDs: %w", err)
	}
	defer rows.Close()
	values := make([]uuid.UUID, 0)
	for rows.Next() {
		var credentialID uuid.UUID
		if err := rows.Scan(&credentialID); err != nil {
			return nil, fmt.Errorf("scan active credential ID: %w", err)
		}
		values = append(values, credentialID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active credential IDs: %w", err)
	}
	return values, nil
}

func (r *Repository) rotate(ctx context.Context, actorID, credentialID uuid.UUID, input SecretInput, sealed sealedSecret, requestID string) (Credential, error) {
	var updated Credential
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		current, err := r.get(ctx, q, credentialID, true)
		if err != nil {
			return err
		}
		if current.Status == StatusRevoked {
			return ErrCredentialInactive
		}
		var version int64
		err = q.QueryRow(ctx, `
			SELECT COALESCE(MAX(version_no), 0) + 1 FROM credential_secrets
			WHERE credential_id = $1 AND secret_kind = $2`, credentialID, input.Kind).Scan(&version)
		if err != nil {
			return fmt.Errorf("allocate credential secret version: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE credential_secrets SET rotated_at = clock_timestamp()
			WHERE credential_id = $1 AND secret_kind = $2 AND rotated_at IS NULL`, credentialID, input.Kind); err != nil {
			return fmt.Errorf("rotate prior credential secret: %w", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO credential_secrets (
				id, credential_id, secret_kind, version_no, ciphertext, nonce, key_version
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, sealed.ID, credentialID, input.Kind, version, sealed.Ciphertext, sealed.Nonce, sealed.KeyVersion); err != nil {
			return mapRepositoryError(err)
		}
		if _, err := q.Exec(ctx, `UPDATE credentials SET updated_at = now() WHERE id = $1`, credentialID); err != nil {
			return fmt.Errorf("touch credential after secret rotation: %w", err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "credential.secret_rotated",
			TargetType:  "credential",
			TargetID:    credentialID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated, err = r.get(ctx, q, credentialID, false)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return updated, nil
}

func (r *Repository) reencrypt(ctx context.Context, actorID, credentialID uuid.UUID, kind string, expectedVersion int64, sealed sealedSecret, requestID string) (Credential, error) {
	var updated Credential
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := r.get(ctx, q, credentialID, true); err != nil {
			return err
		}
		var activeID uuid.UUID
		var activeVersion int64
		if err := q.QueryRow(ctx, `
			SELECT id, version_no
			FROM credential_secrets
			WHERE credential_id = $1 AND secret_kind = $2 AND rotated_at IS NULL
			FOR UPDATE`, credentialID, kind).Scan(&activeID, &activeVersion); err != nil {
			return mapRepositoryError(err)
		}
		if activeVersion != expectedVersion {
			return ErrConflict
		}
		if _, err := q.Exec(ctx, `
			UPDATE credential_secrets SET rotated_at = clock_timestamp()
			WHERE id = $1 AND rotated_at IS NULL`, activeID); err != nil {
			return fmt.Errorf("retire credential secret before re-encryption: %w", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO credential_secrets (
				id, credential_id, secret_kind, version_no, ciphertext, nonce, key_version
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, sealed.ID, credentialID, kind, activeVersion+1, sealed.Ciphertext, sealed.Nonce, sealed.KeyVersion); err != nil {
			return mapRepositoryError(err)
		}
		if _, err := q.Exec(ctx, `UPDATE credentials SET updated_at = now() WHERE id = $1`, credentialID); err != nil {
			return fmt.Errorf("touch credential after re-encryption: %w", err)
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &actorID,
			Action:      "credential.secret_reencrypted",
			TargetType:  "credential",
			TargetID:    credentialID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		var err error
		updated, err = r.get(ctx, q, credentialID, false)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return updated, nil
}

func (r *Repository) recordHealth(ctx context.Context, credentialID uuid.UUID, input HealthInput) error {
	metadataJSON, err := json.Marshal(input.Metadata)
	if err != nil {
		return fmt.Errorf("encode credential health metadata: %w", err)
	}
	var lastSuccess, lastError any
	if input.Succeeded {
		lastSuccess = input.ObservedAt
	} else {
		lastError = input.ObservedAt
	}
	_, err = r.store.Queryer().Exec(ctx, `
		INSERT INTO credential_health (
			credential_id, last_success_at, last_error_at, last_error_class, cooldown_until, observed_at, metadata
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (credential_id) DO UPDATE
		SET last_success_at = CASE WHEN $2 IS NOT NULL THEN EXCLUDED.last_success_at ELSE credential_health.last_success_at END,
			last_error_at = CASE WHEN $3 IS NOT NULL THEN EXCLUDED.last_error_at ELSE credential_health.last_error_at END,
			last_error_class = CASE WHEN $3 IS NOT NULL THEN EXCLUDED.last_error_class ELSE credential_health.last_error_class END,
			cooldown_until = EXCLUDED.cooldown_until,
			observed_at = EXCLUDED.observed_at,
			metadata = EXCLUDED.metadata
		WHERE credential_health.observed_at <= EXCLUDED.observed_at`,
		credentialID, lastSuccess, lastError, nullableString(input.ErrorClass), input.CooldownUntil, input.ObservedAt, metadataJSON)
	if err != nil {
		return fmt.Errorf("record credential health: %w", err)
	}
	return nil
}

func (r *Repository) createPool(ctx context.Context, actorID uuid.UUID, input PoolInput, requestID string) (Pool, error) {
	poolID, err := id.New()
	if err != nil {
		return Pool{}, fmt.Errorf("generate credential pool UUIDv7: %w", err)
	}
	metadataJSON, err := json.Marshal(input.Metadata)
	if err != nil {
		return Pool{}, fmt.Errorf("encode credential pool metadata: %w", err)
	}
	var pool Pool
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO credential_pools (id, provider_id, name, enabled, metadata)
			VALUES ($1, $2, $3, $4, $5)`, poolID, input.ProviderID, input.Name, input.Enabled, metadataJSON); err != nil {
			return mapRepositoryError(err)
		}
		if err := audit.Insert(ctx, q, audit.Event{ActorUserID: &actorID, Action: "credential_pool.created", TargetType: "credential_pool", TargetID: poolID.String(), RequestID: requestID, Result: "success"}); err != nil {
			return err
		}
		pool, err = getPool(ctx, q, poolID)
		return err
	})
	if err != nil {
		return Pool{}, err
	}
	return pool, nil
}

func getPool(ctx context.Context, q data.Querier, poolID uuid.UUID) (Pool, error) {
	var pool Pool
	var metadataJSON []byte
	err := q.QueryRow(ctx, `SELECT id, provider_id, name, enabled, metadata, created_at, updated_at FROM credential_pools WHERE id = $1`, poolID).Scan(&pool.ID, &pool.ProviderID, &pool.Name, &pool.Enabled, &metadataJSON, &pool.CreatedAt, &pool.UpdatedAt)
	if err != nil {
		return Pool{}, mapRepositoryError(err)
	}
	pool.Metadata = map[string]string{}
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &pool.Metadata); err != nil {
			return Pool{}, fmt.Errorf("decode credential pool metadata: %w", err)
		}
	}
	return pool, nil
}

func (r *Repository) listPools(ctx context.Context, providerID uuid.UUID, cursor string, limit int) (PoolPage, error) {
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT id, provider_id, name, enabled, metadata, created_at, updated_at
		FROM credential_pools
		WHERE provider_id = $1 AND name > $2
		ORDER BY name, id LIMIT $3`, providerID, cursor, limit+1)
	if err != nil {
		return PoolPage{}, fmt.Errorf("list credential pools: %w", err)
	}
	defer rows.Close()
	pools := make([]Pool, 0, limit+1)
	for rows.Next() {
		var pool Pool
		var metadataJSON []byte
		if err := rows.Scan(&pool.ID, &pool.ProviderID, &pool.Name, &pool.Enabled, &metadataJSON, &pool.CreatedAt, &pool.UpdatedAt); err != nil {
			return PoolPage{}, fmt.Errorf("scan credential pool: %w", err)
		}
		pool.Metadata = map[string]string{}
		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &pool.Metadata); err != nil {
				return PoolPage{}, fmt.Errorf("decode credential pool metadata: %w", err)
			}
		}
		pools = append(pools, pool)
	}
	if err := rows.Err(); err != nil {
		return PoolPage{}, err
	}
	page := PoolPage{Pools: pools}
	if len(pools) > limit {
		page.NextCursor = pools[limit-1].Name
		page.Pools = pools[:limit]
	}
	return page, nil
}

func (r *Repository) addMember(ctx context.Context, actorID, poolID uuid.UUID, input MembershipInput, requestID string) error {
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO pool_members (pool_id, credential_id, priority, weight, enabled)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (pool_id, credential_id) DO UPDATE
			SET priority = EXCLUDED.priority, weight = EXCLUDED.weight, enabled = EXCLUDED.enabled`, poolID, input.CredentialID, input.Priority, input.Weight, input.Enabled); err != nil {
			return mapRepositoryError(err)
		}
		return audit.Insert(ctx, q, audit.Event{ActorUserID: &actorID, Action: "credential_pool.member_upserted", TargetType: "credential_pool", TargetID: poolID.String(), RequestID: requestID, Result: "success"})
	})
	return err
}

func (r *Repository) removeMember(ctx context.Context, actorID, poolID, credentialID uuid.UUID, requestID string) error {
	return r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM pool_members WHERE pool_id = $1 AND credential_id = $2`, poolID, credentialID); err != nil {
			return fmt.Errorf("remove credential pool member: %w", err)
		}
		return audit.Insert(ctx, q, audit.Event{ActorUserID: &actorID, Action: "credential_pool.member_removed", TargetType: "credential_pool", TargetID: poolID.String(), RequestID: requestID, Result: "success"})
	})
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
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

func sealSecret(keyring *secret.Keyring, credentialID, secretID uuid.UUID, kind, plaintext string) (sealedSecret, error) {
	sealed, err := keyring.Seal([]byte(plaintext), aad(credentialID, secretID, kind, keyring.CurrentVersion()))
	if err != nil {
		return sealedSecret{}, err
	}
	return sealedSecret{ID: secretID, Kind: kind, Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, KeyVersion: sealed.KeyVersion}, nil
}

func aad(credentialID, secretID uuid.UUID, kind, keyVersion string) []byte {
	return []byte("bablo:credential:" + credentialID.String() + ":" + secretID.String() + ":" + kind + ":" + keyVersion)
}
