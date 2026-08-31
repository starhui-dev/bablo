// Package credential owns provider credential metadata, encrypted secret lifecycle,
// health, and pool membership. Plaintext secrets are transient and never part of
// Credential responses.
package credential

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SourceOAuth          = "oauth"
	SourceAPIKey         = "api_key"
	SourceServiceAccount = "service_account"
	SourceCustom         = "custom"

	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusRevoked  = "revoked"
	StatusError    = "error"

	SecretOAuthAccess  = "oauth_access"
	SecretOAuthRefresh = "oauth_refresh"
	SecretOAuthID      = "oauth_id"
	SecretAPIKey       = "api_key"
	SecretServiceAcct  = "service_account"
	SecretCustom       = "custom"
)

var (
	ErrInvalidInput       = errors.New("invalid credential input")
	ErrNotFound           = errors.New("credential not found")
	ErrConflict           = errors.New("credential conflict")
	ErrSecretUnavailable  = errors.New("credential secret unavailable")
	ErrSecretVersion      = errors.New("credential key version unavailable")
	ErrCredentialInactive = errors.New("credential is not active")
	ErrUnsupported        = errors.New("credential operation unsupported")

	stableIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,254}$`)
	regionPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	proxyRefPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	metadataKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// Credential is non-secret provider credential metadata.
type Credential struct {
	ID                uuid.UUID          `json:"id"`
	ProviderID        uuid.UUID          `json:"provider_id"`
	ProviderSlug      string             `json:"provider_slug"`
	ResourceType      string             `json:"resource_type"`
	CommercialAllowed bool               `json:"commercial_allowed"`
	ExternalStableID  string             `json:"external_stable_id"`
	SourceKind        string             `json:"source_kind"`
	Status            string             `json:"status"`
	MaxConcurrency    int                `json:"max_concurrency"`
	Region            string             `json:"region"`
	ProxyRef          string             `json:"proxy_ref"`
	Metadata          map[string]string  `json:"metadata"`
	Secrets           []SecretDescriptor `json:"secrets"`
	Pools             []PoolMembership   `json:"pools"`
	Health            Health             `json:"health"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// SecretDescriptor identifies an encrypted secret version without exposing its value.
type SecretDescriptor struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	VersionNo  int64      `json:"version_no"`
	KeyVersion string     `json:"key_version"`
	CreatedAt  time.Time  `json:"created_at"`
	RotatedAt  *time.Time `json:"rotated_at"`
}

// Health is runtime state that can be rebuilt or refreshed from observations.
type Health struct {
	LastSuccessAt  *time.Time        `json:"last_success_at"`
	LastErrorAt    *time.Time        `json:"last_error_at"`
	LastErrorClass string            `json:"last_error_class"`
	CooldownUntil  *time.Time        `json:"cooldown_until"`
	ObservedAt     time.Time         `json:"observed_at"`
	Metadata       map[string]string `json:"metadata"`
}

// PoolMembership is a credential's placement in a provider-owned pool.
type PoolMembership struct {
	PoolID   uuid.UUID `json:"pool_id"`
	PoolName string    `json:"pool_name"`
	Priority int       `json:"priority"`
	Weight   int       `json:"weight"`
	Enabled  bool      `json:"enabled"`
}

// Pool is a provider-specific credential pool.
type Pool struct {
	ID         uuid.UUID         `json:"id"`
	ProviderID uuid.UUID         `json:"provider_id"`
	Name       string            `json:"name"`
	Enabled    bool              `json:"enabled"`
	Metadata   map[string]string `json:"metadata"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// SecretInput is accepted only by create/rotate operations. It is never echoed.
type SecretInput struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// CreateInput creates one provider credential and its initial encrypted secrets.
type CreateInput struct {
	ProviderID       uuid.UUID         `json:"provider_id"`
	ExternalStableID string            `json:"external_stable_id"`
	SourceKind       string            `json:"source_kind"`
	Region           string            `json:"region"`
	ProxyRef         string            `json:"proxy_ref"`
	MaxConcurrency   int               `json:"max_concurrency"`
	Metadata         map[string]string `json:"metadata"`
	Secrets          []SecretInput     `json:"secrets"`
	Enabled          bool              `json:"enabled"`
}

// UpdateInput changes metadata or lifecycle state. Provider identity is immutable.
type UpdateInput struct {
	Region         *string            `json:"region"`
	ProxyRef       *string            `json:"proxy_ref"`
	Metadata       *map[string]string `json:"metadata"`
	Status         *string            `json:"status"`
	MaxConcurrency *int               `json:"max_concurrency"`
}

// PoolInput creates or updates a provider-owned pool.
type PoolInput struct {
	ProviderID uuid.UUID         `json:"provider_id"`
	Name       string            `json:"name"`
	Metadata   map[string]string `json:"metadata"`
	Enabled    bool              `json:"enabled"`
}

// MembershipInput adds or updates one pool member.
type MembershipInput struct {
	CredentialID uuid.UUID `json:"credential_id"`
	Priority     int       `json:"priority"`
	Weight       int       `json:"weight"`
	Enabled      bool      `json:"enabled"`
}

// HealthInput records one provider observation. Older observations are ignored.
type HealthInput struct {
	Succeeded     bool              `json:"succeeded"`
	ErrorClass    string            `json:"error_class"`
	CooldownUntil *time.Time        `json:"cooldown_until"`
	ObservedAt    time.Time         `json:"observed_at"`
	Metadata      map[string]string `json:"metadata"`
}

// RuntimeCredential contains transient plaintext for the CPA adapter only.
// Callers must not log, serialize, or retain it beyond adapter registration.
type RuntimeCredential struct {
	CredentialID     uuid.UUID
	ProviderID       uuid.UUID
	ProviderSlug     string
	ExternalStableID string
	SourceKind       string
	Region           string
	ProxyRef         string
	Metadata         map[string]string
	Secrets          map[string][]byte
}

func (r RuntimeCredential) Close() {
	for key, value := range r.Secrets {
		for index := range value {
			value[index] = 0
		}
		delete(r.Secrets, key)
	}
}

// Page is a stable external-ID ordered credential page.
type Page struct {
	Credentials []Credential `json:"data"`
	NextCursor  string       `json:"next_cursor"`
}

// PoolPage is a stable name-ordered pool page for one provider.
type PoolPage struct {
	Pools      []Pool `json:"data"`
	NextCursor string `json:"next_cursor"`
}

func validSourceKind(value string) bool {
	switch value {
	case SourceOAuth, SourceAPIKey, SourceServiceAccount, SourceCustom:
		return true
	default:
		return false
	}
}

func validStatus(value string) bool {
	switch value {
	case StatusActive, StatusDisabled, StatusRevoked, StatusError:
		return true
	default:
		return false
	}
}

func validSecretKind(value string) bool {
	switch value {
	case SecretOAuthAccess, SecretOAuthRefresh, SecretOAuthID, SecretAPIKey, SecretServiceAcct, SecretCustom:
		return true
	default:
		return false
	}
}

func sourceAllowsSecret(source, kind string) bool {
	switch source {
	case SourceOAuth:
		return kind == SecretOAuthAccess || kind == SecretOAuthRefresh || kind == SecretOAuthID
	case SourceAPIKey:
		return kind == SecretAPIKey
	case SourceServiceAccount:
		return kind == SecretServiceAcct
	case SourceCustom:
		return kind == SecretCustom
	default:
		return false
	}
}

func normalizeStableID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !stableIDPattern.MatchString(value) {
		return "", ErrInvalidInput
	}
	return value, nil
}

func normalizeRegion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" && !regionPattern.MatchString(value) {
		return "", ErrInvalidInput
	}
	return value, nil
}

func normalizeProxyRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !proxyRefPattern.MatchString(value) {
		return "", ErrInvalidInput
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", ErrInvalidInput
		}
	}
	return value, nil
}

func normalizeMetadata(input map[string]string) (map[string]string, error) {
	if len(input) > 32 {
		return nil, ErrInvalidInput
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if !metadataKeyPattern.MatchString(key) || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
			return nil, ErrInvalidInput
		}
		for _, forbidden := range []string{"secret", "token", "password", "api_key", "private_key"} {
			if strings.Contains(key, forbidden) {
				return nil, ErrInvalidInput
			}
		}
		if _, exists := output[key]; exists {
			return nil, ErrInvalidInput
		}
		output[key] = value
	}
	return output, nil
}

func cloneMetadata(value map[string]string) map[string]string {
	if len(value) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func formatListCursor(value Credential) string {
	return value.ExternalStableID + "\x00" + value.ID.String()
}

func parseListCursor(value string) (string, uuid.UUID, error) {
	parts := strings.Split(value, "\x00")
	if len(parts) != 2 {
		return "", uuid.Nil, ErrInvalidInput
	}
	stableID, err := normalizeStableID(parts[0])
	if err != nil || stableID != parts[0] {
		return "", uuid.Nil, ErrInvalidInput
	}
	credentialID, err := uuid.Parse(parts[1])
	if err != nil || credentialID == uuid.Nil {
		return "", uuid.Nil, ErrInvalidInput
	}
	return stableID, credentialID, nil
}
