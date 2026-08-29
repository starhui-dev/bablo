package auth

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

// Repository persists authentication facts in PostgreSQL.
type Repository struct {
	store *data.Store
}

// NewRepository constructs an authentication repository.
func NewRepository(store *data.Store) (*Repository, error) {
	if store == nil || store.Queryer() == nil {
		return nil, errors.New("auth repository requires an initialized database store")
	}
	return &Repository{store: store}, nil
}

func (r *Repository) findUserByEmail(ctx context.Context, email string) (User, error) {
	return findUser(ctx, r.store.Queryer(), `
		SELECT u.id, u.email_normalized, u.password_hash, u.password_params_version, u.status,
		       EXISTS (
		           SELECT 1 FROM mfa_factors mf
		           WHERE mf.user_id = u.id AND mf.factor_type = 'totp' AND mf.enabled
		       )
		FROM users u
		WHERE u.email_normalized = $1 AND u.deleted_at IS NULL`, email)
}
func (r *Repository) findUserByID(ctx context.Context, userID uuid.UUID) (User, error) {
	return findUser(ctx, r.store.Queryer(), `
		SELECT u.id, u.email_normalized, u.password_hash, u.password_params_version, u.status,
		       EXISTS (
		           SELECT 1 FROM mfa_factors mf
		           WHERE mf.user_id = u.id AND mf.factor_type = 'totp' AND mf.enabled
		       )
		FROM users u
		WHERE u.id = $1 AND u.deleted_at IS NULL`, userID)
}

func findUser(ctx context.Context, q data.Querier, query string, args ...any) (User, error) {
	var user User
	if err := q.QueryRow(ctx, query, args...).Scan(
		&user.ID,
		&user.Email,
		&user.PasswordHash,
		&user.PasswordParamsVersion,
		&user.Status,
		&user.MFAEnabled,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, fmt.Errorf("query user: %w", err)
	}
	roles, err := findRoles(ctx, q, user.ID)
	if err != nil {
		return User{}, err
	}
	user.Roles = roles
	return user, nil
}

func findRoles(ctx context.Context, q data.Querier, userID uuid.UUID) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT r.name
		FROM roles r
		JOIN user_roles ur ON ur.role_id = r.id
		WHERE ur.user_id = $1
		ORDER BY r.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("query user roles: %w", err)
	}
	defer rows.Close()

	roles := make([]string, 0, 2)
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("scan user role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user roles: %w", err)
	}
	return roles, nil
}

func (r *Repository) createUser(ctx context.Context, email, passwordHash, paramsVersion string, roles []string, requestID string) (User, error) {
	userID, err := newUUID()
	if err != nil {
		return User{}, err
	}
	user := User{
		ID:                    userID,
		Email:                 email,
		PasswordHash:          passwordHash,
		PasswordParamsVersion: paramsVersion,
		Status:                "active",
		Roles:                 append([]string(nil), roles...),
	}
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO users (id, email_normalized, password_hash, password_params_version, status)
			VALUES ($1, $2, $3, $4, 'active')`, user.ID, user.Email, user.PasswordHash, user.PasswordParamsVersion); err != nil {
			return mapConflict("insert user", err)
		}
		for _, role := range roles {
			roleID, err := newUUID()
			if err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO roles (id, name, description)
				VALUES ($1, $2, $3)
				ON CONFLICT (name) DO NOTHING`, roleID, role, "Bablo "+role+" role"); err != nil {
				return fmt.Errorf("ensure role %q: %w", role, err)
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO user_roles (user_id, role_id)
				SELECT $1, id FROM roles WHERE name = $2
				ON CONFLICT DO NOTHING`, user.ID, role); err != nil {
				return fmt.Errorf("assign role %q: %w", role, err)
			}
		}
		return insertAudit(ctx, q, nil, "auth.user_created", "user", user.ID, requestID, "success")
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (r *Repository) recordLoginDenied(ctx context.Context, targetID *uuid.UUID, requestID, result string) error {
	return r.store.WithTx(ctx, func(q data.Querier) error {
		return insertAuditPointer(ctx, q, nil, "auth.login_denied", "user", targetID, requestID, result)
	})
}

func (r *Repository) createLoginSession(
	ctx context.Context,
	user User,
	tokenHash, csrfHash [32]byte,
	expiresAt time.Time,
	meta LoginMetadata,
	newPasswordHash, newParamsVersion string,
	previousTokenHash *[32]byte,
) (Session, error) {
	sessionID, err := newUUID()
	if err != nil {
		return Session{}, err
	}
	session := Session{
		ID:         sessionID,
		UserID:     user.ID,
		Email:      user.Email,
		Roles:      append([]string(nil), user.Roles...),
		ExpiresAt:  expiresAt,
		MFAEnabled: user.MFAEnabled,
		csrfHash:   csrfHash,
	}
	err = r.store.WithTx(ctx, func(q data.Querier) error {
		if newPasswordHash != "" {
			if _, err := q.Exec(ctx, `
				UPDATE users
				SET password_hash = $2, password_params_version = $3, password_changed_at = now(), updated_at = now()
				WHERE id = $1`, user.ID, newPasswordHash, newParamsVersion); err != nil {
				return fmt.Errorf("rehash password: %w", err)
			}
		}
		if previousTokenHash != nil {
			if _, err := q.Exec(ctx, `
				UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now())
				WHERE token_hash = $1 AND user_id = $2`, previousTokenHash[:], user.ID); err != nil {
				return fmt.Errorf("revoke replaced session: %w", err)
			}
		}
		if err := insertSession(ctx, q, session, tokenHash, meta, nil); err != nil {
			return err
		}
		return insertAudit(ctx, q, &user.ID, "auth.login_succeeded", "user_session", session.ID, meta.RequestID, "success")
	})
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

func insertSession(ctx context.Context, q data.Querier, session Session, tokenHash [32]byte, meta LoginMetadata, mfaVerifiedAt *time.Time) error {
	var remoteAddr any
	if meta.RemoteAddr != "" {
		remoteAddr = meta.RemoteAddr
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO user_sessions (
			id, user_id, token_hash, csrf_token_hash, expires_at,
			user_agent, remote_addr, mfa_verified_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		session.ID,
		session.UserID,
		tokenHash[:],
		session.csrfHash[:],
		session.ExpiresAt,
		meta.UserAgent,
		remoteAddr,
		mfaVerifiedAt,
	); err != nil {
		return fmt.Errorf("insert user session: %w", err)
	}
	return nil
}

func (r *Repository) findSession(ctx context.Context, tokenHash [32]byte, now time.Time) (Session, error) {
	var session Session
	var csrfHash []byte
	var mfaVerifiedAt pgtype.Timestamptz
	if err := r.store.Queryer().QueryRow(ctx, `
		SELECT s.id, s.user_id, u.email_normalized, s.csrf_token_hash, s.expires_at,
		       s.mfa_verified_at,
		       EXISTS (
		           SELECT 1 FROM mfa_factors mf
		           WHERE mf.user_id = u.id AND mf.factor_type = 'totp' AND mf.enabled
		       )
		FROM user_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.csrf_token_hash IS NOT NULL
		  AND s.revoked_at IS NULL
		  AND s.expires_at > $2
		  AND u.status = 'active'
		  AND u.deleted_at IS NULL`, tokenHash[:], now).Scan(
		&session.ID,
		&session.UserID,
		&session.Email,
		&csrfHash,
		&session.ExpiresAt,
		&mfaVerifiedAt,
		&session.MFAEnabled,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrSessionInvalid
		}
		return Session{}, fmt.Errorf("query user session: %w", err)
	}
	if len(csrfHash) != len(session.csrfHash) {
		return Session{}, ErrSessionInvalid
	}
	copy(session.csrfHash[:], csrfHash)
	session.MFAVerified = mfaVerifiedAt.Valid
	roles, err := findRoles(ctx, r.store.Queryer(), session.UserID)
	if err != nil {
		return Session{}, err
	}
	session.Roles = roles
	_, _ = r.store.Queryer().Exec(ctx, `
		UPDATE user_sessions SET last_seen_at = $2
		WHERE id = $1 AND last_seen_at < $2 - interval '5 minutes'`, session.ID, now)
	return session, nil
}

func (r *Repository) revokeSession(ctx context.Context, session Session, requestID, action string) error {
	return r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, session.ID); err != nil {
			return fmt.Errorf("revoke session: %w", err)
		}
		return insertAudit(ctx, q, &session.UserID, action, "user_session", session.ID, requestID, "success")
	})
}

func (r *Repository) updatePassword(ctx context.Context, actorID *uuid.UUID, targetID uuid.UUID, hash, paramsVersion, requestID, action string) error {
	return r.store.WithTx(ctx, func(q data.Querier) error {
		command, err := q.Exec(ctx, `
			UPDATE users
			SET password_hash = $2, password_params_version = $3, password_changed_at = now(), updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL`, targetID, hash, paramsVersion)
		if err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		if command.RowsAffected() != 1 {
			return ErrUserNotFound
		}
		if _, err := q.Exec(ctx, `
			UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now())
			WHERE user_id = $1 AND revoked_at IS NULL`, targetID); err != nil {
			return fmt.Errorf("revoke user sessions: %w", err)
		}
		return insertAudit(ctx, q, actorID, action, "user", targetID, requestID, "success")
	})
}

func (r *Repository) revokeAllSessions(ctx context.Context, session Session, requestID string) error {
	return r.store.WithTx(ctx, func(q data.Querier) error {
		if _, err := q.Exec(ctx, `
			UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now())
			WHERE user_id = $1 AND revoked_at IS NULL`, session.UserID); err != nil {
			return fmt.Errorf("revoke all user sessions: %w", err)
		}
		return insertAudit(ctx, q, &session.UserID, "auth.logout_all", "user", session.UserID, requestID, "success")
	})
}

type totpFactor struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Ciphertext  []byte
	Nonce       []byte
	KeyVersion  string
	Enabled     bool
	LastCounter *uint64
}

func findTOTPFactor(ctx context.Context, q data.Querier, userID uuid.UUID, forUpdate bool) (totpFactor, error) {
	query := `
		SELECT id, user_id, secret_ciphertext, secret_nonce, key_version,
		       enabled, last_totp_counter
		FROM mfa_factors
		WHERE user_id = $1 AND factor_type = 'totp'`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var factor totpFactor
	var lastCounter pgtype.Int8
	if err := q.QueryRow(ctx, query, userID).Scan(
		&factor.ID,
		&factor.UserID,
		&factor.Ciphertext,
		&factor.Nonce,
		&factor.KeyVersion,
		&factor.Enabled,
		&lastCounter,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return totpFactor{}, ErrMFAUnavailable
		}
		return totpFactor{}, fmt.Errorf("query TOTP factor: %w", err)
	}
	if lastCounter.Valid {
		counter := uint64(lastCounter.Int64)
		factor.LastCounter = &counter
	}
	return factor, nil
}

func (r *Repository) storePendingTOTP(ctx context.Context, factor totpFactor, requestID string) error {
	return r.store.WithTx(ctx, func(q data.Querier) error {
		existing, err := findTOTPFactor(ctx, q, factor.UserID, true)
		switch {
		case err == nil && existing.Enabled:
			return ErrConflict
		case err == nil:
			if _, err := q.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE factor_id = $1`, existing.ID); err != nil {
				return fmt.Errorf("delete pending recovery codes: %w", err)
			}
			if _, err := q.Exec(ctx, `DELETE FROM mfa_factors WHERE id = $1`, existing.ID); err != nil {
				return fmt.Errorf("replace pending TOTP factor: %w", err)
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO mfa_factors (
					id, user_id, factor_type, secret_ciphertext, secret_nonce, key_version
				)
				VALUES ($1, $2, 'totp', $3, $4, $5)`, factor.ID, factor.UserID, factor.Ciphertext, factor.Nonce, factor.KeyVersion); err != nil {
				return mapConflict("replace pending TOTP factor", err)
			}
		case errors.Is(err, ErrMFAUnavailable):
			if _, err := q.Exec(ctx, `
				INSERT INTO mfa_factors (
					id, user_id, factor_type, secret_ciphertext, secret_nonce, key_version
				)
				VALUES ($1, $2, 'totp', $3, $4, $5)`, factor.ID, factor.UserID, factor.Ciphertext, factor.Nonce, factor.KeyVersion); err != nil {
				return mapConflict("insert TOTP factor", err)
			}
		default:
			return err
		}
		return insertAudit(ctx, q, &factor.UserID, "auth.mfa_bind_started", "mfa_factor", factor.ID, requestID, "success")
	})
}

func insertAudit(ctx context.Context, q data.Querier, actorID *uuid.UUID, action, targetType string, targetID uuid.UUID, requestID, result string) error {
	return insertAuditPointer(ctx, q, actorID, action, targetType, &targetID, requestID, result)
}

func insertAuditPointer(ctx context.Context, q data.Querier, actorID *uuid.UUID, action, targetType string, targetID *uuid.UUID, requestID, result string) error {
	target := ""
	if targetID != nil {
		target = targetID.String()
	}
	return audit.Insert(ctx, q, audit.Event{
		ActorUserID: actorID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    target,
		RequestID:   requestID,
		Result:      result,
	})
}

func mapConflict(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func newUUID() (uuid.UUID, error) {
	generated, err := id.New()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate UUIDv7: %w", err)
	}
	return generated, nil
}
