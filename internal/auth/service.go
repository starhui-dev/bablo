package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"

	"github.com/starhui-dev/bablo/internal/data"
)

const recoveryCodeCount = 10

// ServiceConfig controls Web Session authentication.
type ServiceConfig struct {
	SessionTTL      time.Duration
	Issuer          string
	RequireAdminMFA bool
	SecretBox       *SecretBox
	LoginLimiter    AttemptLimiter
	MFALimiter      AttemptLimiter
	Now             func() time.Time
}

// Service owns password, Session, CSRF, RBAC, and MFA behavior.
type Service struct {
	repository      *Repository
	sessionTTL      time.Duration
	issuer          string
	requireAdminMFA bool
	secretBox       *SecretBox
	loginLimiter    AttemptLimiter
	mfaLimiter      AttemptLimiter
	now             func() time.Time
	dummyHash       string
}

// NewService constructs the authentication service.
func NewService(repository *Repository, cfg ServiceConfig) (*Service, error) {
	if repository == nil {
		return nil, errors.New("auth service requires a repository")
	}
	if cfg.SessionTTL < 5*time.Minute || cfg.SessionTTL > 7*24*time.Hour {
		return nil, errors.New("auth session TTL must be between 5 minutes and 7 days")
	}
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("auth TOTP issuer is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.LoginLimiter == nil {
		cfg.LoginLimiter = NewMemoryAttemptLimiter(8, 40, 5*time.Minute, 10_000)
	}
	if cfg.MFALimiter == nil {
		cfg.MFALimiter = NewMemoryAttemptLimiter(8, 40, 5*time.Minute, 10_000)
	}
	dummyHash, _, err := HashPassword("Bablo-invalid-password-only")
	if err != nil {
		return nil, fmt.Errorf("initialize password verifier: %w", err)
	}
	return &Service{
		repository:      repository,
		sessionTTL:      cfg.SessionTTL,
		issuer:          strings.TrimSpace(cfg.Issuer),
		requireAdminMFA: cfg.RequireAdminMFA,
		secretBox:       cfg.SecretBox,
		loginLimiter:    cfg.LoginLimiter,
		mfaLimiter:      cfg.MFALimiter,
		now:             cfg.Now,
		dummyHash:       dummyHash,
	}, nil
}

// CreateLocalUser creates a user through a trusted local administrative path.
func (s *Service) CreateLocalUser(ctx context.Context, email, password string, admin bool, requestID string) (User, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	hash, version, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	roles := []string{"user"}
	if admin {
		roles = append(roles, "admin")
	}
	return s.repository.createUser(ctx, normalized, hash, version, roles, requestID)
}

// LocalResetPassword resets a password from a trusted local administrative
// process and revokes every active session for the account.
func (s *Service) LocalResetPassword(ctx context.Context, email, newPassword, requestID string) error {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	user, err := s.repository.findUserByEmail(ctx, normalized)
	if err != nil {
		return err
	}
	hash, version, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.repository.updatePassword(ctx, nil, user.ID, hash, version, requestID, "auth.local_password_reset")
}

// Login verifies credentials and atomically replaces any browser-provided prior
// session to prevent session fixation.
func (s *Service) Login(ctx context.Context, email, password, previousToken string, meta LoginMetadata) (SessionBundle, error) {
	normalized, normalizeErr := normalizeEmail(email)
	if normalizeErr != nil {
		normalized = strings.ToLower(strings.TrimSpace(email))
	}
	now := s.now().UTC()
	if !s.loginLimiter.Allow(ctx, normalized, meta.RemoteAddr, now) {
		_ = s.repository.recordLoginDenied(ctx, nil, meta.RequestID, "denied")
		return SessionBundle{}, ErrRateLimited
	}

	user, err := s.repository.findUserByEmail(ctx, normalized)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			_, _ = VerifyPassword(password, s.dummyHash)
			_ = s.repository.recordLoginDenied(ctx, nil, meta.RequestID, "denied")
			return SessionBundle{}, ErrInvalidCredentials
		}
		return SessionBundle{}, err
	}
	valid, err := VerifyPassword(password, user.PasswordHash)
	if err != nil {
		return SessionBundle{}, fmt.Errorf("verify stored password: %w", err)
	}
	if normalizeErr != nil || !valid || user.Status != "active" {
		_ = s.repository.recordLoginDenied(ctx, &user.ID, meta.RequestID, "denied")
		return SessionBundle{}, ErrInvalidCredentials
	}

	var newHash, newParamsVersion string
	if PasswordNeedsRehash(user.PasswordHash, user.PasswordParamsVersion) {
		newHash, newParamsVersion, err = HashPassword(password)
		if err != nil {
			return SessionBundle{}, err
		}
	}
	bundle, err := s.issueLoginSession(ctx, user, previousToken, meta, newHash, newParamsVersion, now)
	if err != nil {
		return SessionBundle{}, err
	}
	return bundle, nil
}

func (s *Service) issueLoginSession(
	ctx context.Context,
	user User,
	previousToken string,
	meta LoginMetadata,
	newPasswordHash, newParamsVersion string,
	now time.Time,
) (SessionBundle, error) {
	sessionToken, tokenHash, err := newOpaqueToken()
	if err != nil {
		return SessionBundle{}, err
	}
	csrfToken, csrfHash, err := newOpaqueToken()
	if err != nil {
		return SessionBundle{}, err
	}
	var previousHash *[32]byte
	if previousToken != "" {
		hashed := hashToken(previousToken)
		previousHash = &hashed
	}
	session, err := s.repository.createLoginSession(
		ctx,
		user,
		tokenHash,
		csrfHash,
		now.Add(s.sessionTTL),
		meta,
		newPasswordHash,
		newParamsVersion,
		previousHash,
	)
	if err != nil {
		return SessionBundle{}, err
	}
	return SessionBundle{Session: session, SessionToken: sessionToken, CSRFToken: csrfToken}, nil
}

// Authenticate resolves a Web Session from its opaque cookie token.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, ErrSessionInvalid
	}
	return s.repository.findSession(ctx, hashToken(token), s.now().UTC())
}

// ValidateCSRF verifies the double-submit token against the hash bound to the
// server-side session.
func (s *Service) ValidateCSRF(session Session, token string) error {
	if len(token) != 43 {
		return ErrCSRFInvalid
	}
	hash := hashToken(token)
	if subtle.ConstantTimeCompare(hash[:], session.csrfHash[:]) != 1 {
		return ErrCSRFInvalid
	}
	return nil
}

// RequireFullSession rejects sessions waiting for MFA.
func (s *Service) RequireFullSession(session Session) error {
	if session.MFARequired() {
		return ErrMFARequired
	}
	return nil
}

// AuthorizeRole enforces RBAC and production admin MFA policy.
func (s *Service) AuthorizeRole(session Session, role string) error {
	if err := s.RequireFullSession(session); err != nil {
		return err
	}
	if !session.HasRole(role) {
		return ErrPermissionDenied
	}
	if role == "admin" && s.requireAdminMFA && (!session.MFAEnabled || !session.MFAVerified) {
		return ErrMFARequired
	}
	return nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, session Session, requestID string) error {
	return s.repository.revokeSession(ctx, session, requestID, "auth.logout")
}

// LogoutAll revokes every active session for the current user.
func (s *Service) LogoutAll(ctx context.Context, session Session, requestID string) error {
	if err := s.RequireFullSession(session); err != nil {
		return err
	}
	return s.repository.revokeAllSessions(ctx, session, requestID)
}

// ChangePassword verifies the current password, updates it, and revokes all
// sessions including the current one.
func (s *Service) ChangePassword(ctx context.Context, session Session, currentPassword, newPassword, requestID string) error {
	if err := s.RequireFullSession(session); err != nil {
		return err
	}
	user, err := s.repository.findUserByID(ctx, session.UserID)
	if err != nil {
		return err
	}
	valid, err := VerifyPassword(currentPassword, user.PasswordHash)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !valid {
		return ErrInvalidCredentials
	}
	hash, version, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.repository.updatePassword(ctx, &session.UserID, session.UserID, hash, version, requestID, "auth.password_changed")
}

// AdminResetPassword performs an RBAC- and MFA-gated password reset and revokes
// all target sessions.
func (s *Service) AdminResetPassword(ctx context.Context, actor Session, targetID uuid.UUID, newPassword, requestID string) error {
	if err := s.AuthorizeRole(actor, "admin"); err != nil {
		return err
	}
	hash, version, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.repository.updatePassword(ctx, &actor.UserID, targetID, hash, version, requestID, "auth.admin_password_reset")
}

// BeginTOTP starts authenticator enrollment. The plaintext secret and URI are
// returned once and only encrypted bytes are persisted.
func (s *Service) BeginTOTP(ctx context.Context, session Session, requestID string) (TOTPBinding, error) {
	if err := s.RequireFullSession(session); err != nil {
		return TOTPBinding{}, err
	}
	if session.MFAEnabled {
		return TOTPBinding{}, ErrConflict
	}
	if s.secretBox == nil {
		return TOTPBinding{}, ErrMFAUnavailable
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      s.issuer,
		AccountName: session.Email,
		Period:      30,
		SecretSize:  20,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return TOTPBinding{}, fmt.Errorf("generate TOTP secret: %w", err)
	}
	factorID, err := newUUID()
	if err != nil {
		return TOTPBinding{}, err
	}
	ciphertext, nonce, keyVersion, err := s.secretBox.Seal(factorID, session.UserID, []byte(key.Secret()))
	if err != nil {
		return TOTPBinding{}, err
	}
	if err := s.repository.storePendingTOTP(ctx, totpFactor{
		ID:         factorID,
		UserID:     session.UserID,
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: keyVersion,
	}, requestID); err != nil {
		return TOTPBinding{}, err
	}
	return TOTPBinding{Secret: key.Secret(), ProvisionURL: key.URL()}, nil
}

// ConfirmTOTP enables a pending factor, issues recovery codes, and rotates the
// current session into an MFA-verified session.
func (s *Service) ConfirmTOTP(ctx context.Context, session Session, passcode string, meta LoginMetadata) (SessionBundle, []string, error) {
	if err := s.RequireFullSession(session); err != nil {
		return SessionBundle{}, nil, err
	}
	if !s.mfaLimiter.Allow(ctx, session.Email, meta.RemoteAddr, s.now().UTC()) {
		return SessionBundle{}, nil, ErrRateLimited
	}
	plainCodes, codeHashes, err := generateRecoveryCodes()
	if err != nil {
		return SessionBundle{}, nil, err
	}
	bundle, err := s.rotateWithMFA(ctx, session, passcode, meta, true, codeHashes)
	if err != nil {
		return SessionBundle{}, nil, err
	}
	return bundle, plainCodes, nil
}

// VerifyMFA validates TOTP or a single-use recovery code and rotates the partial
// session into an MFA-verified session.
func (s *Service) VerifyMFA(ctx context.Context, session Session, code string, meta LoginMetadata) (SessionBundle, error) {
	if !session.MFARequired() {
		return SessionBundle{}, ErrMFAUnavailable
	}
	if !s.mfaLimiter.Allow(ctx, session.Email, meta.RemoteAddr, s.now().UTC()) {
		return SessionBundle{}, ErrRateLimited
	}
	bundle, err := s.rotateWithMFA(ctx, session, code, meta, false, nil)
	if err != nil {
		return SessionBundle{}, err
	}
	return bundle, nil
}

func (s *Service) rotateWithMFA(
	ctx context.Context,
	oldSession Session,
	code string,
	meta LoginMetadata,
	confirm bool,
	newRecoveryHashes [][32]byte,
) (SessionBundle, error) {
	now := s.now().UTC()
	sessionToken, tokenHash, err := newOpaqueToken()
	if err != nil {
		return SessionBundle{}, err
	}
	csrfToken, csrfHash, err := newOpaqueToken()
	if err != nil {
		return SessionBundle{}, err
	}
	sessionID, err := newUUID()
	if err != nil {
		return SessionBundle{}, err
	}
	newSession := Session{
		ID:          sessionID,
		UserID:      oldSession.UserID,
		Email:       oldSession.Email,
		Roles:       append([]string(nil), oldSession.Roles...),
		ExpiresAt:   now.Add(s.sessionTTL),
		MFAEnabled:  true,
		MFAVerified: true,
		csrfHash:    csrfHash,
	}

	err = s.repository.store.WithTx(ctx, func(q data.Querier) error {
		factor, err := findTOTPFactor(ctx, q, oldSession.UserID, true)
		if err != nil {
			return err
		}
		if confirm == factor.Enabled {
			return ErrConflict
		}
		plaintext, err := s.secretBox.Open(factor.ID, factor.UserID, factor.Ciphertext, factor.Nonce, factor.KeyVersion)
		if err != nil {
			return err
		}
		secret := string(plaintext)
		if confirm {
			counter, err := validateTOTP(secret, code, now, factor.LastCounter)
			if err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `
				UPDATE mfa_factors
				SET enabled = true, confirmed_at = $2, last_totp_counter = $3
				WHERE id = $1`, factor.ID, now, int64(counter)); err != nil {
				return fmt.Errorf("confirm TOTP factor: %w", err)
			}
			if err := replaceRecoveryCodes(ctx, q, factor.ID, newRecoveryHashes); err != nil {
				return err
			}
		} else if isTOTPPasscode(code) {
			counter, err := validateTOTP(secret, code, now, factor.LastCounter)
			if err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `UPDATE mfa_factors SET last_totp_counter = $2 WHERE id = $1`, factor.ID, int64(counter)); err != nil {
				return fmt.Errorf("advance TOTP counter: %w", err)
			}
		} else {
			recoveryHash := hashRecoveryCode(code)
			command, err := q.Exec(ctx, `
				UPDATE mfa_recovery_codes
				SET consumed_at = $3
				WHERE factor_id = $1 AND code_hash = $2 AND consumed_at IS NULL`, factor.ID, recoveryHash[:], now)
			if err != nil {
				return fmt.Errorf("consume recovery code: %w", err)
			}
			if command.RowsAffected() != 1 {
				return ErrMFAInvalid
			}
		}
		if _, err := q.Exec(ctx, `UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1`, oldSession.ID, now); err != nil {
			return fmt.Errorf("rotate partial session: %w", err)
		}
		if err := insertSession(ctx, q, newSession, tokenHash, meta, &now); err != nil {
			return err
		}
		action := "auth.mfa_verified"
		if confirm {
			action = "auth.mfa_enabled"
		}
		return insertAudit(ctx, q, &oldSession.UserID, action, "user_session", newSession.ID, meta.RequestID, "success")
	})
	if err != nil {
		if errors.Is(err, ErrMFAInvalid) {
			return SessionBundle{}, ErrMFAInvalid
		}
		return SessionBundle{}, err
	}
	return SessionBundle{Session: newSession, SessionToken: sessionToken, CSRFToken: csrfToken}, nil
}

// RegenerateRecoveryCodes invalidates every unused old recovery code.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, session Session, requestID string) ([]string, error) {
	if err := s.RequireFullSession(session); err != nil {
		return nil, err
	}
	if !session.MFAEnabled || !session.MFAVerified {
		return nil, ErrMFARequired
	}
	plainCodes, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = s.repository.store.WithTx(ctx, func(q data.Querier) error {
		factor, err := findTOTPFactor(ctx, q, session.UserID, true)
		if err != nil {
			return err
		}
		if !factor.Enabled {
			return ErrMFAUnavailable
		}
		if err := replaceRecoveryCodes(ctx, q, factor.ID, hashes); err != nil {
			return err
		}
		return insertAudit(ctx, q, &session.UserID, "auth.mfa_recovery_regenerated", "mfa_factor", factor.ID, requestID, "success")
	})
	if err != nil {
		return nil, err
	}
	return plainCodes, nil
}

func replaceRecoveryCodes(ctx context.Context, q data.Querier, factorID uuid.UUID, hashes [][32]byte) error {
	if _, err := q.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE factor_id = $1`, factorID); err != nil {
		return fmt.Errorf("delete recovery codes: %w", err)
	}
	for _, hash := range hashes {
		id, err := newUUID()
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO mfa_recovery_codes (id, factor_id, code_hash)
			VALUES ($1, $2, $3)`, id, factorID, hash[:]); err != nil {
			return fmt.Errorf("insert recovery code: %w", err)
		}
	}
	return nil
}

func generateRecoveryCodes() ([]string, [][32]byte, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([][32]byte, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code, hash, err := newRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		codes = append(codes, code)
		hashes = append(hashes, hash)
	}
	return codes, hashes, nil
}

func validateTOTP(secret, passcode string, now time.Time, lastCounter *uint64) (uint64, error) {
	if !isTOTPPasscode(passcode) {
		return 0, ErrMFAInvalid
	}
	current := uint64(now.Unix() / 30)
	candidates := []uint64{current}
	if current > 0 {
		candidates = append(candidates, current-1)
	}
	candidates = append(candidates, current+1)
	for _, counter := range candidates {
		if lastCounter != nil && counter <= *lastCounter {
			continue
		}
		valid, err := hotp.ValidateCustom(passcode, counter, secret, hotp.ValidateOpts{
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, ErrMFAInvalid
		}
		if valid {
			return counter, nil
		}
	}
	return 0, ErrMFAInvalid
}

func isTOTPPasscode(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func normalizeEmail(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || len(normalized) > 320 {
		return "", ErrInvalidInput
	}
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || strings.ToLower(parsed.Address) != normalized {
		return "", ErrInvalidInput
	}
	return normalized, nil
}
