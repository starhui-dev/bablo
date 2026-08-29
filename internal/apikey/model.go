// Package apikey implements Bablo inference API Key lifecycle and authorization.
package apikey

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput         = errors.New("invalid API key input")
	ErrNotFound             = errors.New("API key not found")
	ErrConflict             = errors.New("API key conflict")
	ErrInvalidKey           = errors.New("invalid API key")
	ErrIPDenied             = errors.New("API key source IP is not allowed")
	ErrModelDenied          = errors.New("API key model access is denied")
	ErrRateLimited          = errors.New("API key rate limit exceeded")
	ErrRateLimitUnavailable = errors.New("API key rate limiter is unavailable")
)

// Key is the non-secret representation returned to users and services.
type Key struct {
	ID                 uuid.UUID
	UserID             uuid.UUID
	Name               string
	Prefix             string
	Status             string
	ExpiresAt          *time.Time
	IPAllowlist        []string
	RPMLimit           *int64
	TPMLimit           *int64
	DailyBudgetMinor   *int64
	MonthlyBudgetMinor *int64
	AllowedModels      []string
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	RotatedAt          *time.Time
	SecretVersion      int64
}

// EffectiveStatus includes time-based expiry without requiring a background job.
func (k Key) EffectiveStatus(now time.Time) string {
	if k.Status == "revoked" {
		return "revoked"
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return "expired"
	}
	return k.Status
}

// Principal is safe request context. It never contains a raw Key or hash.
type Principal struct {
	UserID             uuid.UUID
	APIKeyID           uuid.UUID
	KeyPrefix          string
	SecretVersion      int64
	RPMLimit           *int64
	TPMLimit           *int64
	DailyBudgetMinor   *int64
	MonthlyBudgetMinor *int64
}

// CreateInput describes a user-managed API Key.
type CreateInput struct {
	Name               string
	ExpiresAt          *time.Time
	AllowedModels      []string
	IPAllowlist        []string
	RPMLimit           *int64
	TPMLimit           *int64
	DailyBudgetMinor   *int64
	MonthlyBudgetMinor *int64
}

// OptionalTime distinguishes an omitted field from an explicit null.
type OptionalTime struct {
	Set   bool
	Value *time.Time
}

// OptionalInt64 distinguishes an omitted field from an explicit null.
type OptionalInt64 struct {
	Set   bool
	Value *int64
}

// UpdateInput is an atomic patch of a user-owned API Key and its managed policy.
type UpdateInput struct {
	Name               *string
	ExpiresAt          OptionalTime
	AllowedModels      *[]string
	IPAllowlist        *[]string
	RPMLimit           OptionalInt64
	TPMLimit           OptionalInt64
	DailyBudgetMinor   OptionalInt64
	MonthlyBudgetMinor OptionalInt64
}

// CreatedKey carries plaintext material exactly once after a committed write.
type CreatedKey struct {
	Key    Key
	Secret string `json:"-"`
}
