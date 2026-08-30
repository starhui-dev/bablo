// Package model owns Bablo public model identifiers, aliases, and canonical capabilities.
package model

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	VisibilityPublic   = "public"
	VisibilityPrivate  = "private"
	VisibilityInternal = "internal"

	BillingToken    = "token"
	BillingRequest  = "request"
	BillingFree     = "free"
	BillingDisabled = "disabled"
)

var (
	ErrInvalidInput = errors.New("invalid model input")
	ErrNotFound     = errors.New("model not found")
	ErrConflict     = errors.New("model conflict")

	identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,127}$`)
)

// Capabilities is the canonical Bablo capability declaration. It is independent
// of CPA SDK types and is shared by public and provider models.
type Capabilities struct {
	Chat      bool `json:"chat"`
	Responses bool `json:"responses"`
	Messages  bool `json:"messages"`
	Stream    bool `json:"stream"`
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// Valid requires at least one request protocol capability.
func (capabilities Capabilities) Valid() bool {
	return capabilities.Chat || capabilities.Responses || capabilities.Messages
}

// Supports reports whether every provider capability is allowed by the public model.
func (capabilities Capabilities) Supports(candidate Capabilities) bool {
	return (!candidate.Chat || capabilities.Chat) &&
		(!candidate.Responses || capabilities.Responses) &&
		(!candidate.Messages || capabilities.Messages) &&
		(!candidate.Stream || capabilities.Stream) &&
		(!candidate.Tools || capabilities.Tools) &&
		(!candidate.Vision || capabilities.Vision) &&
		(!candidate.Reasoning || capabilities.Reasoning)
}

// Model is a stable public Bablo model. PublicID is the canonical request name;
// aliases resolve to the same record but are never silently reassigned.
type Model struct {
	ID              uuid.UUID    `json:"id"`
	PublicID        string       `json:"public_model_id"`
	Aliases         []string     `json:"aliases"`
	DisplayName     string       `json:"display_name"`
	Visibility      string       `json:"visibility"`
	BillingClass    string       `json:"billing_class"`
	Capabilities    Capabilities `json:"capabilities"`
	Enabled         bool         `json:"enabled"`
	RouteConfigured bool         `json:"route_configured"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

// CreateInput contains administrator-owned model catalog fields.
type CreateInput struct {
	PublicID     string
	Aliases      []string
	DisplayName  string
	Visibility   string
	BillingClass string
	Capabilities Capabilities
	Enabled      bool
}

// UpdateInput patches a model. A non-nil Aliases slice replaces the enabled
// alias set; removed aliases remain reserved but disabled.
type UpdateInput struct {
	PublicID     *string
	Aliases      *[]string
	DisplayName  *string
	Visibility   *string
	BillingClass *string
	Capabilities *Capabilities
	Enabled      *bool
}

// Page is a stable public-ID ordered model page.
type Page struct {
	Models     []Model
	NextCursor string
}

func normalizeIdentifier(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !identifierPattern.MatchString(value) {
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

func normalizeAliases(values []string, publicID string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	aliases := make([]string, 0, len(values))
	for _, value := range values {
		alias, err := normalizeIdentifier(value)
		if err != nil || alias == publicID {
			return nil, ErrInvalidInput
		}
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	if len(aliases) > 100 {
		return nil, ErrInvalidInput
	}
	return aliases, nil
}

func validVisibility(value string) bool {
	switch value {
	case VisibilityPublic, VisibilityPrivate, VisibilityInternal:
		return true
	default:
		return false
	}
}

func validBillingClass(value string) bool {
	switch value {
	case BillingToken, BillingRequest, BillingFree, BillingDisabled:
		return true
	default:
		return false
	}
}
