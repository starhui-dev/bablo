// Package route owns public-model route configuration, immutable snapshots, and
// candidate resolution. Scheduler selection is intentionally outside this package.
package route

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/model"
)

const (
	MatchExact = "exact"

	CommercialInherit = "inherit"
	CommercialAllow   = "allow"
	CommercialDeny    = "deny"
)

var (
	ErrInvalidInput      = errors.New("invalid route input")
	ErrNotFound          = errors.New("route not found")
	ErrConflict          = errors.New("route conflict")
	ErrNoRoute           = errors.New("no route configured")
	ErrRouteDisabled     = errors.New("route is disabled")
	ErrTargetUnavailable = errors.New("route target unavailable")

	identifierPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,127}$`)
	metadataKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// Route is the mutable route identity. Its active version is immutable once
// published; updates publish a new Version instead of editing targets in place.
type Route struct {
	ID              uuid.UUID         `json:"id"`
	ModelID         uuid.UUID         `json:"model_id"`
	ModelPublicID   string            `json:"model_public_id"`
	MatchType       string            `json:"match_type"`
	MatchValue      string            `json:"match_value"`
	Enabled         bool              `json:"enabled"`
	Metadata        map[string]string `json:"metadata"`
	ActiveVersionID *uuid.UUID        `json:"active_version_id"`
	ActiveVersion   *Version          `json:"active_version,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// Version is an immutable route snapshot. EffectiveTo is set exactly once when
// a newer version replaces it.
type Version struct {
	ID            uuid.UUID  `json:"id"`
	RouteID       uuid.UUID  `json:"route_id"`
	VersionNo     int64      `json:"version_no"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	SnapshotHash  []byte     `json:"snapshot_hash"`
	CreatedBy     *uuid.UUID `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	Targets       []Target   `json:"targets"`
}

// Target is a scheduler candidate. It identifies a provider model and a
// Credential pool but never selects an individual Credential.
type Target struct {
	ID                         uuid.UUID          `json:"id"`
	RouteVersionID             uuid.UUID          `json:"route_version_id"`
	TargetNo                   int                `json:"target_no"`
	ProviderModelID            uuid.UUID          `json:"provider_model_id"`
	CredentialPoolID           uuid.UUID          `json:"credential_pool_id"`
	ProviderID                 uuid.UUID          `json:"provider_id"`
	ProviderSlug               string             `json:"provider_slug"`
	ProviderResourceType       string             `json:"provider_resource_type"`
	ProviderCommercialAllowed  bool               `json:"provider_commercial_allowed"`
	UpstreamModelID            string             `json:"upstream_model_id"`
	Protocol                   string             `json:"protocol"`
	Capabilities               model.Capabilities `json:"capabilities"`
	ProviderModelEnabled       bool               `json:"provider_model_enabled"`
	ReviewStatus               string             `json:"review_status"`
	PoolEnabled                bool               `json:"pool_enabled"`
	Priority                   int                `json:"priority"`
	Weight                     int                `json:"weight"`
	CommercialPolicy           string             `json:"commercial_policy"`
	EffectiveCommercialAllowed bool               `json:"effective_commercial_allowed"`
	Enabled                    bool               `json:"enabled"`
	Metadata                   map[string]string  `json:"metadata"`
}

// TargetInput describes one target in a newly published snapshot. TargetNo is
// assigned from slice order so the request cannot create sparse or duplicate
// ordering values.
type TargetInput struct {
	ProviderModelID  uuid.UUID
	CredentialPoolID uuid.UUID
	Priority         int
	Weight           int
	CommercialPolicy string
	Enabled          bool
	Metadata         map[string]string
}

// CreateInput creates a route and its first active version atomically.
type CreateInput struct {
	ModelID    uuid.UUID
	MatchType  string
	MatchValue string
	Metadata   map[string]string
	Enabled    bool
	Targets    []TargetInput
}

// PublishInput creates the next immutable active version for one route.
type PublishInput struct {
	Targets []TargetInput
}

// UpdateInput changes only the mutable route identity fields.
type UpdateInput struct {
	Metadata *map[string]string
	Enabled  *bool
}

// Page is an opaque match-value ordered route page.
type Page struct {
	Routes     []Route
	NextCursor string
}

// Resolution is the output consumed by Scheduler. All candidates belong to
// one route version fixed at resolution time.
type Resolution struct {
	RequestedModel string    `json:"requested_model"`
	ModelID        uuid.UUID `json:"model_id"`
	ModelPublicID  string    `json:"model_public_id"`
	Route          Route     `json:"route"`
	Version        Version   `json:"version"`
	Candidates     []Target  `json:"candidates"`
}

func normalizeIdentifier(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !identifierPattern.MatchString(value) {
		return "", ErrInvalidInput
	}
	return value, nil
}

func normalizeMetadata(value map[string]string) (map[string]string, error) {
	if len(value) > 32 {
		return nil, ErrInvalidInput
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		key = strings.ToLower(strings.TrimSpace(key))
		item = strings.TrimSpace(item)
		if key == "" || len(key) > 64 || !metadataKeyPattern.MatchString(key) || len(item) > 256 || strings.ContainsAny(item, "\r\n\x00") {
			return nil, ErrInvalidInput
		}
		result[key] = item
	}
	return result, nil
}

func normalizeTargets(targets []TargetInput) ([]TargetInput, error) {
	if len(targets) == 0 || len(targets) > 100 {
		return nil, ErrInvalidInput
	}
	result := make([]TargetInput, len(targets))
	seen := make(map[[2]uuid.UUID]struct{}, len(targets))
	enabled := 0
	for index, target := range targets {
		if target.Weight == 0 {
			target.Weight = 1
		}
		if target.ProviderModelID == uuid.Nil || target.CredentialPoolID == uuid.Nil || target.Priority < 0 || target.Priority > 1_000_000 || target.Weight < 1 || target.Weight > 1000 {
			return nil, ErrInvalidInput
		}
		if target.CommercialPolicy == "" {
			target.CommercialPolicy = CommercialInherit
		}
		switch target.CommercialPolicy {
		case CommercialInherit, CommercialAllow, CommercialDeny:
		default:
			return nil, ErrInvalidInput
		}
		metadata, err := normalizeMetadata(target.Metadata)
		if err != nil {
			return nil, err
		}
		target.Metadata = metadata
		key := [2]uuid.UUID{target.ProviderModelID, target.CredentialPoolID}
		if _, exists := seen[key]; exists {
			return nil, ErrConflict
		}
		seen[key] = struct{}{}
		if target.Enabled {
			enabled++
		}
		result[index] = target
	}
	if enabled == 0 {
		return nil, ErrInvalidInput
	}
	return result, nil
}

func normalizeCreate(input CreateInput) (CreateInput, error) {
	if input.ModelID == uuid.Nil {
		return CreateInput{}, ErrInvalidInput
	}
	matchType := strings.TrimSpace(input.MatchType)
	if matchType == "" {
		matchType = MatchExact
	}
	if matchType != MatchExact {
		return CreateInput{}, ErrInvalidInput
	}
	matchValue, err := normalizeIdentifier(input.MatchValue)
	if err != nil {
		return CreateInput{}, err
	}
	metadata, err := normalizeMetadata(input.Metadata)
	if err != nil {
		return CreateInput{}, err
	}
	targets, err := normalizeTargets(input.Targets)
	if err != nil {
		return CreateInput{}, err
	}
	input.MatchType = matchType
	input.MatchValue = matchValue
	input.Metadata = metadata
	input.Targets = targets
	return input, nil
}

func normalizePublish(input PublishInput) (PublishInput, error) {
	targets, err := normalizeTargets(input.Targets)
	if err != nil {
		return PublishInput{}, err
	}
	input.Targets = targets
	return input, nil
}

func normalizeUpdate(input UpdateInput) (UpdateInput, error) {
	if input.Metadata == nil && input.Enabled == nil {
		return UpdateInput{}, ErrInvalidInput
	}
	if input.Metadata != nil {
		metadata, err := normalizeMetadata(*input.Metadata)
		if err != nil {
			return UpdateInput{}, err
		}
		input.Metadata = &metadata
	}
	return input, nil
}
