// Package auth implements Bablo Web Session authentication. Inference API Keys
// remain a separate authentication surface.
package auth

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput       = errors.New("invalid authentication input")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("login rate limited")
	ErrSessionInvalid     = errors.New("session is invalid")
	ErrCSRFInvalid        = errors.New("csrf token is invalid")
	ErrMFARequired        = errors.New("multi-factor authentication required")
	ErrMFAInvalid         = errors.New("multi-factor authentication code is invalid")
	ErrMFAUnavailable     = errors.New("multi-factor authentication is not configured")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrConflict           = errors.New("resource conflict")
	ErrUserNotFound       = errors.New("user not found")
)

// User is the stable identity representation used by the auth service.
type User struct {
	ID                    uuid.UUID
	Email                 string
	PasswordHash          string
	PasswordParamsVersion string
	Status                string
	Roles                 []string
	MFAEnabled            bool
}

// Session is an authenticated Web Session. It never contains the plaintext
// session or CSRF token.
type Session struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Email       string
	Roles       []string
	ExpiresAt   time.Time
	MFAEnabled  bool
	MFAVerified bool
	csrfHash    [32]byte
}

// MFARequired reports whether the session must complete MFA before normal use.
func (s Session) MFARequired() bool {
	return s.MFAEnabled && !s.MFAVerified
}

// HasRole reports whether the session owns a named RBAC role.
func (s Session) HasRole(role string) bool {
	for _, assigned := range s.Roles {
		if assigned == role {
			return true
		}
	}
	return false
}

// SessionBundle contains newly issued plaintext tokens. Callers must send them
// to the browser once and must never persist or log them.
type SessionBundle struct {
	Session      Session
	SessionToken string
	CSRFToken    string
}

// LoginMetadata contains non-secret request context stored with the session and
// audit record.
type LoginMetadata struct {
	UserAgent  string
	RemoteAddr string
	RequestID  string
}

// TOTPBinding is returned once during enrollment.
type TOTPBinding struct {
	Secret       string
	ProvisionURL string
}
