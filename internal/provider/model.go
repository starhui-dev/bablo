// Package provider owns upstream provider metadata and reviewed provider-model discovery facts.
package provider

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/model"
)

const (
	ResourceOfficialAPI   = "official_api"
	ResourceEnterpriseAPI = "enterprise_api"
	ResourceSubscription  = "subscription"
	ResourceThirdParty    = "third_party"

	ProtocolOpenAIChat      = "openai_chat"
	ProtocolOpenAIResponses = "openai_responses"
	ProtocolClaudeMessages  = "claude_messages"
	ProtocolGemini          = "gemini"
	ProtocolCustom          = "custom"

	ReviewPending  = "pending"
	ReviewApproved = "approved"
	ReviewRejected = "rejected"

	DiscoveryUnknown = "unknown"
	DiscoveryPresent = "present"
	DiscoveryMissing = "missing"
)

var (
	ErrInvalidInput = errors.New("invalid provider input")
	ErrNotFound     = errors.New("provider not found")
	ErrConflict     = errors.New("provider conflict")

	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Provider is a resource-policy boundary, not a credential container.
type Provider struct {
	ID                uuid.UUID `json:"id"`
	Slug              string    `json:"slug"`
	DisplayName       string    `json:"display_name"`
	ResourceType      string    `json:"resource_type"`
	CommercialAllowed bool      `json:"commercial_allowed"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ProviderModel records an upstream identifier separately from the public model.
type ProviderModel struct {
	ID              uuid.UUID          `json:"id"`
	ProviderID      uuid.UUID          `json:"provider_id"`
	ModelID         *uuid.UUID         `json:"model_id"`
	UpstreamModelID string             `json:"upstream_model_id"`
	Protocol        string             `json:"protocol"`
	Capabilities    model.Capabilities `json:"capabilities"`
	Enabled         bool               `json:"enabled"`
	ReviewStatus    string             `json:"review_status"`
	DiscoveryStatus string             `json:"discovery_status"`
	DiscoveredAt    *time.Time         `json:"discovered_at"`
	LastSeenAt      *time.Time         `json:"last_seen_at"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// CreateInput contains administrator-owned provider metadata.
type CreateInput struct {
	Slug              string
	DisplayName       string
	ResourceType      string
	CommercialAllowed bool
	Enabled           bool
}

// UpdateInput patches provider policy metadata.
type UpdateInput struct {
	Slug              *string
	DisplayName       *string
	ResourceType      *string
	CommercialAllowed *bool
	Enabled           *bool
}

// CreateModelInput approves and maps one manually configured upstream model.
type CreateModelInput struct {
	ProviderID      uuid.UUID
	ModelID         uuid.UUID
	UpstreamModelID string
	Protocol        string
	Capabilities    model.Capabilities
	Enabled         bool
}

// OptionalUUID distinguishes an omitted model_id from an explicit null.
type OptionalUUID struct {
	Set   bool
	Value *uuid.UUID
}

// UpdateModelInput patches review and business configuration without accepting
// discovery status from an administrator.
type UpdateModelInput struct {
	ModelID      OptionalUUID
	Protocol     *string
	Capabilities *model.Capabilities
	Enabled      *bool
	ReviewStatus *string
}

// Discovery is one observed upstream model. It is a signal only: new rows are
// pending and disabled until an administrator maps and approves them.
type Discovery struct {
	UpstreamModelID string             `json:"upstream_model_id"`
	Protocol        string             `json:"protocol"`
	Capabilities    model.Capabilities `json:"capabilities"`
}

// ReconcileResult reports catalog signals without changing approved enablement.
type ReconcileResult struct {
	Discovered int       `json:"discovered"`
	Observed   int       `json:"observed"`
	Missing    int64     `json:"missing"`
	ObservedAt time.Time `json:"observed_at"`
}

// Page is a stable slug-ordered provider page.
type Page struct {
	Providers  []Provider
	NextCursor string
}

// ModelPage is a stable upstream-ID ordered provider-model page.
type ModelPage struct {
	Models     []ProviderModel
	NextCursor string
}

func normalizeSlug(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !slugPattern.MatchString(value) {
		return "", ErrInvalidInput
	}
	return value, nil
}

func normalizeDisplayName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 120 {
		return "", ErrInvalidInput
	}
	return value, nil
}

func normalizeUpstreamModelID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 255 || strings.ContainsAny(value, "\r\n\x00") {
		return "", ErrInvalidInput
	}
	return value, nil
}

func validResourceType(value string) bool {
	switch value {
	case ResourceOfficialAPI, ResourceEnterpriseAPI, ResourceSubscription, ResourceThirdParty:
		return true
	default:
		return false
	}
}

func validProtocol(value string) bool {
	switch value {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolClaudeMessages, ProtocolGemini, ProtocolCustom:
		return true
	default:
		return false
	}
}

func validReviewStatus(value string) bool {
	switch value {
	case ReviewPending, ReviewApproved, ReviewRejected:
		return true
	default:
		return false
	}
}
