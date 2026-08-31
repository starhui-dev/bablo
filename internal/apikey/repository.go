package apikey

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
)

// Repository persists API Key and policy facts in PostgreSQL.
type Repository struct {
	store *data.Store
}

// NewRepository constructs an API Key repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("API key repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

type ipPolicy struct {
	Allow []string `json:"allow"`
}

type rowScanner interface {
	Scan(...any) error
}

const keyColumns = `
	id, user_id, name, key_prefix, status, expires_at, ip_policy,
	rpm_limit, tpm_limit, daily_budget_minor, monthly_budget_minor,
	last_used_at, created_at, updated_at, rotated_at, secret_version`

const qualifiedKeyColumns = `
	api_keys.id, api_keys.user_id, api_keys.name, api_keys.key_prefix, api_keys.status,
	api_keys.expires_at, api_keys.ip_policy, api_keys.rpm_limit, api_keys.tpm_limit,
	api_keys.daily_budget_minor, api_keys.monthly_budget_minor, api_keys.last_used_at,
	api_keys.created_at, api_keys.updated_at, api_keys.rotated_at, api_keys.secret_version`

func scanKey(row rowScanner) (Key, error) {
	var key Key
	var expiresAt, lastUsedAt, rotatedAt pgtype.Timestamptz
	var rpm, tpm, daily, monthly pgtype.Int8
	var policyJSON []byte
	if err := row.Scan(
		&key.ID,
		&key.UserID,
		&key.Name,
		&key.Prefix,
		&key.Status,
		&expiresAt,
		&policyJSON,
		&rpm,
		&tpm,
		&daily,
		&monthly,
		&lastUsedAt,
		&key.CreatedAt,
		&key.UpdatedAt,
		&rotatedAt,
		&key.SecretVersion,
	); err != nil {
		return Key{}, err
	}
	key.ExpiresAt = timestamptzPointer(expiresAt)
	key.LastUsedAt = timestamptzPointer(lastUsedAt)
	key.RotatedAt = timestamptzPointer(rotatedAt)
	key.RPMLimit = int64Pointer(rpm)
	key.TPMLimit = int64Pointer(tpm)
	key.DailyBudgetMinor = int64Pointer(daily)
	key.MonthlyBudgetMinor = int64Pointer(monthly)
	var policy ipPolicy
	if len(policyJSON) != 0 {
		if err := json.Unmarshal(policyJSON, &policy); err != nil {
			return Key{}, fmt.Errorf("decode API key IP policy: %w", err)
		}
	}
	key.IPAllowlist = append([]string(nil), policy.Allow...)
	return key, nil
}

func timestamptzPointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func int64Pointer(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func (r *Repository) list(ctx context.Context, userID uuid.UUID) ([]Key, error) {
	rows, err := r.store.Queryer().Query(ctx, `SELECT `+keyColumns+`
		FROM api_keys
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list API keys: %w", err)
	}
	keys := make([]Key, 0)
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan API key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate API keys: %w", err)
	}
	rows.Close()
	for index := range keys {
		keys[index].AllowedModels, err = listAllowedModels(ctx, r.store.Queryer(), keys[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func listAllowedModels(ctx context.Context, q data.Querier, keyID uuid.UUID) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT m.public_model_id
		FROM api_key_policies akp
		JOIN policy_model_entitlements entitlement ON entitlement.policy_id = akp.policy_id AND entitlement.effect = 'allow'
		JOIN models m ON m.id = entitlement.model_id
		WHERE akp.api_key_id = $1
		  AND m.enabled AND m.deleted_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1
		      FROM api_key_policies denied_key_policy
		      JOIN policy_model_entitlements denied ON denied.policy_id = denied_key_policy.policy_id
		      WHERE denied_key_policy.api_key_id = akp.api_key_id
		        AND denied.model_id = m.id AND denied.effect = 'deny'
		  )
		ORDER BY m.public_model_id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("list API key models: %w", err)
	}
	defer rows.Close()
	models := make([]string, 0)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("scan API key model: %w", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate API key models: %w", err)
	}
	return models, nil
}

func (r *Repository) create(ctx context.Context, userID uuid.UUID, material storedSecret, input CreateInput, requestID string) (Key, error) {
	if err := validateStoredSecret(material); err != nil {
		return Key{}, err
	}
	keyID, err := id.New()
	if err != nil {
		return Key{}, fmt.Errorf("generate API key UUIDv7: %w", err)
	}
	policyID, err := id.New()
	if err != nil {
		return Key{}, fmt.Errorf("generate API key policy UUIDv7: %w", err)
	}
	policyJSON, err := json.Marshal(ipPolicy{Allow: input.IPAllowlist})
	if err != nil {
		return Key{}, fmt.Errorf("encode API key IP policy: %w", err)
	}
	metadata, err := json.Marshal(map[string]string{"managed_by": "apikey", "api_key_id": keyID.String()})
	if err != nil {
		return Key{}, fmt.Errorf("encode API key policy metadata: %w", err)
	}

	key := Key{
		ID:                 keyID,
		UserID:             userID,
		Name:               input.Name,
		Prefix:             material.Prefix,
		Status:             "active",
		ExpiresAt:          input.ExpiresAt,
		IPAllowlist:        append([]string(nil), input.IPAllowlist...),
		RPMLimit:           cloneInt64(input.RPMLimit),
		TPMLimit:           cloneInt64(input.TPMLimit),
		DailyBudgetMinor:   cloneInt64(input.DailyBudgetMinor),
		MonthlyBudgetMinor: cloneInt64(input.MonthlyBudgetMinor),
		AllowedModels:      append([]string(nil), input.AllowedModels...),
		SecretVersion:      1,
	}
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		modelIDs, err := requireModels(ctx, q, input.AllowedModels)
		if err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `
			INSERT INTO api_keys (
				id, user_id, name, key_prefix, secret_hash, expires_at, ip_policy,
				rpm_limit, tpm_limit, daily_budget_minor, monthly_budget_minor
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING created_at, updated_at`,
			key.ID,
			key.UserID,
			key.Name,
			key.Prefix,
			material.Hash[:],
			key.ExpiresAt,
			policyJSON,
			key.RPMLimit,
			key.TPMLimit,
			key.DailyBudgetMinor,
			key.MonthlyBudgetMinor,
		).Scan(&key.CreatedAt, &key.UpdatedAt); err != nil {
			return mapRepositoryError("insert API key", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO policies (id, name, default_action, metadata)
			VALUES ($1, $2, 'deny', $3)`, policyID, "apikey:"+key.ID.String(), metadata); err != nil {
			return mapRepositoryError("insert API key policy", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO api_key_policies (api_key_id, policy_id, priority)
			VALUES ($1, $2, 0)`, key.ID, policyID); err != nil {
			return fmt.Errorf("assign API key policy: %w", err)
		}
		for _, modelID := range modelIDs {
			if _, err := q.Exec(ctx, `
				INSERT INTO policy_model_entitlements (policy_id, model_id, effect)
				VALUES ($1, $2, 'allow')`, policyID, modelID); err != nil {
				return mapRepositoryError("insert API key entitlement", err)
			}
		}
		return audit.Insert(ctx, q, audit.Event{
			ActorUserID: &key.UserID,
			Action:      "apikey.created",
			TargetType:  "api_key",
			TargetID:    key.ID.String(),
			RequestID:   requestID,
			Result:      "success",
		})
	})
	if err != nil {
		return Key{}, err
	}
	return key, nil
}

func requireModels(ctx context.Context, q data.Querier, publicIDs []string) ([]uuid.UUID, error) {
	if len(publicIDs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT id, public_model_id
		FROM models
		WHERE public_model_id = ANY($1)
		  AND enabled AND visibility = 'public' AND deleted_at IS NULL`, publicIDs)
	if err != nil {
		return nil, fmt.Errorf("query API key models: %w", err)
	}
	defer rows.Close()
	found := make(map[string]uuid.UUID, len(publicIDs))
	for rows.Next() {
		var modelID uuid.UUID
		var publicID string
		if err := rows.Scan(&modelID, &publicID); err != nil {
			return nil, fmt.Errorf("scan API key catalog model: %w", err)
		}
		found[publicID] = modelID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate API key catalog models: %w", err)
	}
	if len(found) != len(publicIDs) {
		return nil, ErrInvalidInput
	}
	modelIDs := make([]uuid.UUID, 0, len(publicIDs))
	for _, publicID := range publicIDs {
		modelIDs = append(modelIDs, found[publicID])
	}
	return modelIDs, nil
}

func (r *Repository) findByHash(ctx context.Context, hash [32]byte) (Key, error) {
	key, err := scanKey(r.store.Queryer().QueryRow(ctx, `SELECT `+qualifiedKeyColumns+`
		FROM api_keys
		JOIN users ON users.id = api_keys.user_id
		WHERE api_keys.secret_hash = $1
		  AND users.status = 'active' AND users.deleted_at IS NULL`, hash[:]))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Key{}, ErrInvalidKey
		}
		return Key{}, fmt.Errorf("query API key: %w", err)
	}
	return key, nil
}

func findOwnedKey(ctx context.Context, q data.Querier, userID, keyID uuid.UUID, forUpdate bool) (Key, error) {
	query := `SELECT ` + keyColumns + ` FROM api_keys WHERE id = $1 AND user_id = $2`
	if forUpdate {
		query += " FOR UPDATE"
	}
	key, err := scanKey(q.QueryRow(ctx, query, keyID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Key{}, ErrNotFound
		}
		return Key{}, fmt.Errorf("query owned API key: %w", err)
	}
	return key, nil
}

func (r *Repository) update(ctx context.Context, userID, keyID uuid.UUID, input UpdateInput, requestID string) (Key, error) {
	var updated Key
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		key, err := findOwnedKey(ctx, q, userID, keyID, true)
		if err != nil {
			return err
		}
		if key.Status != "active" {
			return ErrConflict
		}
		if input.Name != nil {
			key.Name = *input.Name
		}
		if input.ExpiresAt.Set {
			key.ExpiresAt = input.ExpiresAt.Value
		}
		if input.IPAllowlist != nil {
			key.IPAllowlist = append([]string(nil), (*input.IPAllowlist)...)
		}
		if input.RPMLimit.Set {
			key.RPMLimit = cloneInt64(input.RPMLimit.Value)
		}
		if input.TPMLimit.Set {
			key.TPMLimit = cloneInt64(input.TPMLimit.Value)
		}
		if input.DailyBudgetMinor.Set {
			key.DailyBudgetMinor = cloneInt64(input.DailyBudgetMinor.Value)
		}
		if input.MonthlyBudgetMinor.Set {
			key.MonthlyBudgetMinor = cloneInt64(input.MonthlyBudgetMinor.Value)
		}
		policyJSON, err := json.Marshal(ipPolicy{Allow: key.IPAllowlist})
		if err != nil {
			return fmt.Errorf("encode updated API key IP policy: %w", err)
		}
		if err := q.QueryRow(ctx, `
			UPDATE api_keys
			SET name = $3, expires_at = $4, ip_policy = $5,
			    rpm_limit = $6, tpm_limit = $7,
			    daily_budget_minor = $8, monthly_budget_minor = $9,
			    updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING updated_at`,
			key.ID,
			key.UserID,
			key.Name,
			key.ExpiresAt,
			policyJSON,
			key.RPMLimit,
			key.TPMLimit,
			key.DailyBudgetMinor,
			key.MonthlyBudgetMinor,
		).Scan(&key.UpdatedAt); err != nil {
			return fmt.Errorf("update API key: %w", err)
		}
		if input.AllowedModels != nil {
			modelIDs, err := requireModels(ctx, q, *input.AllowedModels)
			if err != nil {
				return err
			}
			var policyID uuid.UUID
			if err := q.QueryRow(ctx, `
				SELECT policy.id
				FROM api_key_policies assignment
				JOIN policies policy ON policy.id = assignment.policy_id
				WHERE assignment.api_key_id = $1
				  AND policy.metadata->>'managed_by' = 'apikey'
				  AND policy.metadata->>'api_key_id' = $2`, key.ID, key.ID.String()).Scan(&policyID); err != nil {
				return fmt.Errorf("query managed API key policy: %w", err)
			}
			if _, err := q.Exec(ctx, `DELETE FROM policy_model_entitlements WHERE policy_id = $1`, policyID); err != nil {
				return fmt.Errorf("replace API key entitlements: %w", err)
			}
			for _, modelID := range modelIDs {
				if _, err := q.Exec(ctx, `
					INSERT INTO policy_model_entitlements (policy_id, model_id, effect)
					VALUES ($1, $2, 'allow')`, policyID, modelID); err != nil {
					return mapRepositoryError("insert API key entitlement", err)
				}
			}
			key.AllowedModels = append([]string(nil), (*input.AllowedModels)...)
		} else {
			key.AllowedModels, err = listAllowedModels(ctx, q, key.ID)
			if err != nil {
				return err
			}
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &key.UserID,
			Action:      "apikey.updated",
			TargetType:  "api_key",
			TargetID:    key.ID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		updated = key
		return nil
	})
	if err != nil {
		return Key{}, err
	}
	return updated, nil
}

func (r *Repository) rotate(ctx context.Context, userID, keyID uuid.UUID, material storedSecret, requestID string) (Key, error) {
	if err := validateStoredSecret(material); err != nil {
		return Key{}, err
	}
	var rotated Key
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		key, err := findOwnedKey(ctx, q, userID, keyID, true)
		if err != nil {
			return err
		}
		var rotatedAt time.Time
		err = q.QueryRow(ctx, `
			UPDATE api_keys
			SET key_prefix = $3, secret_hash = $4,
			    secret_version = secret_version + 1,
			    rotated_at = observed.now, updated_at = observed.now
			FROM (SELECT clock_timestamp() AS now) observed
			WHERE id = $1 AND user_id = $2
			  AND status = 'active'
			  AND (expires_at IS NULL OR expires_at > observed.now)
			RETURNING secret_version, rotated_at, updated_at`,
			key.ID, key.UserID, material.Prefix, material.Hash[:],
		).Scan(&key.SecretVersion, &rotatedAt, &key.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return mapRepositoryError("rotate API key", err)
		}
		key.RotatedAt = &rotatedAt
		key.Prefix = material.Prefix
		key.AllowedModels, err = listAllowedModels(ctx, q, key.ID)
		if err != nil {
			return err
		}
		if err := audit.Insert(ctx, q, audit.Event{
			ActorUserID: &key.UserID,
			Action:      "apikey.rotated",
			TargetType:  "api_key",
			TargetID:    key.ID.String(),
			RequestID:   requestID,
			Result:      "success",
		}); err != nil {
			return err
		}
		rotated = key
		return nil
	})
	if err != nil {
		return Key{}, err
	}
	return rotated, nil
}

func (r *Repository) revoke(ctx context.Context, userID, keyID uuid.UUID, now time.Time, requestID string) (Key, error) {
	var revoked Key
	err := r.store.WithTx(ctx, func(q data.Querier) error {
		key, err := findOwnedKey(ctx, q, userID, keyID, true)
		if err != nil {
			return err
		}
		if key.Status != "revoked" {
			if _, err := q.Exec(ctx, `
				UPDATE api_keys
				SET status = 'revoked', revoked_at = COALESCE(revoked_at, $3), updated_at = $3
				WHERE id = $1 AND user_id = $2`, key.ID, key.UserID, now); err != nil {
				return fmt.Errorf("revoke API key: %w", err)
			}
			key.Status = "revoked"
			key.UpdatedAt = now
			if err := audit.Insert(ctx, q, audit.Event{
				ActorUserID: &key.UserID,
				Action:      "apikey.revoked",
				TargetType:  "api_key",
				TargetID:    key.ID.String(),
				RequestID:   requestID,
				Result:      "success",
			}); err != nil {
				return err
			}
		}
		key.AllowedModels, err = listAllowedModels(ctx, q, key.ID)
		if err != nil {
			return err
		}
		revoked = key
		return nil
	})
	if err != nil {
		return Key{}, err
	}
	return revoked, nil
}

func (r *Repository) authorizeModel(ctx context.Context, keyID, userID uuid.UUID, secretVersion int64, publicModelID string, now time.Time) (bool, error) {
	var keyValid, modelExists, denied, allowed, defaultAllow bool
	if err := r.store.Queryer().QueryRow(ctx, `
		WITH valid_key AS (
			SELECT api_keys.id
			FROM api_keys
			JOIN users ON users.id = api_keys.user_id
			WHERE api_keys.id = $1
			  AND api_keys.user_id = $4
			  AND api_keys.secret_version = $5
			  AND api_keys.status = 'active'
			  AND (api_keys.expires_at IS NULL OR api_keys.expires_at > $3)
			  AND users.status = 'active' AND users.deleted_at IS NULL
		), target_model AS (
			SELECT id
			FROM models
			WHERE public_model_id = $2 AND enabled AND visibility = 'public' AND deleted_at IS NULL
		)
		SELECT
			EXISTS (SELECT 1 FROM valid_key),
			EXISTS (SELECT 1 FROM target_model),
			EXISTS (
				SELECT 1 FROM valid_key
				JOIN api_key_policies assignment ON assignment.api_key_id = valid_key.id
				JOIN policy_model_entitlements entitlement ON entitlement.policy_id = assignment.policy_id
				JOIN target_model ON target_model.id = entitlement.model_id
				WHERE entitlement.effect = 'deny'
			),
			EXISTS (
				SELECT 1 FROM valid_key
				JOIN api_key_policies assignment ON assignment.api_key_id = valid_key.id
				JOIN policy_model_entitlements entitlement ON entitlement.policy_id = assignment.policy_id
				JOIN target_model ON target_model.id = entitlement.model_id
				WHERE entitlement.effect = 'allow'
			),
			EXISTS (
				SELECT 1 FROM valid_key
				JOIN api_key_policies assignment ON assignment.api_key_id = valid_key.id
				JOIN policies policy ON policy.id = assignment.policy_id
				WHERE policy.default_action = 'allow'
			)`, keyID, publicModelID, now, userID, secretVersion).Scan(&keyValid, &modelExists, &denied, &allowed, &defaultAllow); err != nil {
		return false, fmt.Errorf("authorize API key model: %w", err)
	}
	if !keyValid {
		return false, ErrInvalidKey
	}
	if !modelExists || denied {
		return false, nil
	}
	return allowed || defaultAllow, nil
}

func (r *Repository) listAuthorizedModels(ctx context.Context, keyID, userID uuid.UUID, secretVersion int64, now time.Time) ([]string, error) {
	var keyValid bool
	if err := r.store.Queryer().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM api_keys
			JOIN users ON users.id = api_keys.user_id
			WHERE api_keys.id = $1
			  AND api_keys.user_id = $2
			  AND api_keys.secret_version = $3
			  AND api_keys.status = 'active'
			  AND (api_keys.expires_at IS NULL OR api_keys.expires_at > $4)
			  AND users.status = 'active' AND users.deleted_at IS NULL
		)`, keyID, userID, secretVersion, now).Scan(&keyValid); err != nil {
		return nil, fmt.Errorf("validate API key for model list: %w", err)
	}
	if !keyValid {
		return nil, ErrInvalidKey
	}
	rows, err := r.store.Queryer().Query(ctx, `
		SELECT m.public_model_id
		FROM models m
		WHERE m.enabled AND m.visibility = 'public' AND m.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM api_key_policies assignment
			JOIN policies policy ON policy.id = assignment.policy_id
			WHERE assignment.api_key_id = $1
			  AND (
				policy.default_action = 'allow'
				OR EXISTS (
					SELECT 1
					FROM policy_model_entitlements allow_entitlement
					WHERE allow_entitlement.policy_id = policy.id
					  AND allow_entitlement.model_id = m.id
					  AND allow_entitlement.effect = 'allow'
				)
			  )
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM api_key_policies denied_assignment
			JOIN policy_model_entitlements denied_entitlement ON denied_entitlement.policy_id = denied_assignment.policy_id
			WHERE denied_assignment.api_key_id = $1
			  AND denied_entitlement.model_id = m.id
			  AND denied_entitlement.effect = 'deny'
		  )
		ORDER BY m.public_model_id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("list authorized API key models: %w", err)
	}
	defer rows.Close()
	models := make([]string, 0)
	for rows.Next() {
		var publicID string
		if err := rows.Scan(&publicID); err != nil {
			return nil, fmt.Errorf("scan authorized API key model: %w", err)
		}
		models = append(models, publicID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized API key models: %w", err)
	}
	return models, nil
}

func (r *Repository) touchLastUsed(ctx context.Context, keyID uuid.UUID, now time.Time) {
	_, _ = r.store.Queryer().Exec(ctx, `
		UPDATE api_keys
		SET last_used_at = $2
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < $2 - interval '5 minutes')`, keyID, now)
}

func mapRepositoryError(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
