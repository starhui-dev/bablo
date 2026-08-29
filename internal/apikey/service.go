package apikey

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maximumKeyNameBytes  = 80
	maximumModelsPerKey  = 200
	maximumIPRulesPerKey = 100
)

// Service owns API Key validation, lifecycle, and authorization policy.
type Service struct {
	repository *Repository
	limiter    Limiter
	now        func() time.Time
}

// NewService constructs an API Key service.
func NewService(repository *Repository, limiter Limiter) (*Service, error) {
	if repository == nil {
		return nil, errors.New("API key service requires a repository")
	}
	if limiter == nil {
		return nil, errors.New("API key service requires a limiter")
	}
	return &Service{repository: repository, limiter: limiter, now: time.Now}, nil
}

// List returns non-secret API Keys owned by one user.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Key, error) {
	if userID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	keys, err := s.repository.list(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	for index := range keys {
		keys[index].Status = keys[index].EffectiveStatus(now)
	}
	return keys, nil
}

// Create generates and commits a Key whose plaintext is returned once.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, input CreateInput, requestID string) (CreatedKey, error) {
	if userID == uuid.Nil {
		return CreatedKey{}, ErrInvalidInput
	}
	validated, err := s.validateCreate(input)
	if err != nil {
		return CreatedKey{}, err
	}
	material, err := generateSecret()
	if err != nil {
		return CreatedKey{}, err
	}
	key, err := s.repository.create(ctx, userID, material.stored(), validated, requestID)
	if err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{Key: key, Secret: material.Plaintext}, nil
}

// Update atomically patches one owned Key and its managed model entitlements.
func (s *Service) Update(ctx context.Context, userID, keyID uuid.UUID, input UpdateInput, requestID string) (Key, error) {
	if userID == uuid.Nil || keyID == uuid.Nil {
		return Key{}, ErrInvalidInput
	}
	validated, err := s.validateUpdate(input)
	if err != nil {
		return Key{}, err
	}
	key, err := s.repository.update(ctx, userID, keyID, validated, requestID)
	if err != nil {
		return Key{}, err
	}
	key.Status = key.EffectiveStatus(s.now().UTC())
	return key, nil
}

// Rotate atomically replaces secret material. The previous secret stops working
// when the transaction commits.
func (s *Service) Rotate(ctx context.Context, userID, keyID uuid.UUID, requestID string) (CreatedKey, error) {
	if userID == uuid.Nil || keyID == uuid.Nil {
		return CreatedKey{}, ErrInvalidInput
	}
	material, err := generateSecret()
	if err != nil {
		return CreatedKey{}, err
	}
	key, err := s.repository.rotate(ctx, userID, keyID, material.stored(), requestID)
	if err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{Key: key, Secret: material.Plaintext}, nil
}

// Revoke makes one owned Key unusable. Repeated calls are idempotent.
func (s *Service) Revoke(ctx context.Context, userID, keyID uuid.UUID, requestID string) (Key, error) {
	if userID == uuid.Nil || keyID == uuid.Nil {
		return Key{}, ErrInvalidInput
	}
	return s.repository.revoke(ctx, userID, keyID, s.now().UTC(), requestID)
}

// Authenticate validates secret, owner state, expiry, and direct source IP.
func (s *Service) Authenticate(ctx context.Context, secret string, sourceIP netip.Addr) (Principal, error) {
	hash, err := hashSecret(secret)
	if err != nil {
		return Principal{}, ErrInvalidKey
	}
	key, err := s.repository.findByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrInvalidKey) {
			return Principal{}, ErrInvalidKey
		}
		return Principal{}, err
	}
	now := s.now().UTC()
	if key.EffectiveStatus(now) != "active" {
		return Principal{}, ErrInvalidKey
	}
	if !sourceIP.IsValid() || !allowsIP(key.IPAllowlist, sourceIP.Unmap()) {
		return Principal{}, ErrIPDenied
	}
	s.repository.touchLastUsed(ctx, key.ID, now)
	return Principal{
		UserID:             key.UserID,
		APIKeyID:           key.ID,
		KeyPrefix:          key.Prefix,
		SecretVersion:      key.SecretVersion,
		RPMLimit:           cloneInt64(key.RPMLimit),
		TPMLimit:           cloneInt64(key.TPMLimit),
		DailyBudgetMinor:   cloneInt64(key.DailyBudgetMinor),
		MonthlyBudgetMinor: cloneInt64(key.MonthlyBudgetMinor),
	}, nil
}

// Authorize checks the model policy and consumes the configured RPM/TPM window.
// Budget thresholds are carried by Principal but are enforced only after the
// Usage/Billing stages provide a trustworthy consumption fact.
func (s *Service) Authorize(ctx context.Context, principal Principal, publicModelID string, tokens int64) error {
	publicModelID = strings.TrimSpace(publicModelID)
	if principal.UserID == uuid.Nil || principal.APIKeyID == uuid.Nil || principal.SecretVersion <= 0 || publicModelID == "" || tokens < 0 {
		return ErrInvalidInput
	}
	now := s.now().UTC()
	allowed, err := s.repository.authorizeModel(ctx, principal.APIKeyID, principal.UserID, principal.SecretVersion, publicModelID, now)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrModelDenied
	}
	return s.limiter.Allow(ctx, principal.APIKeyID, principal.RPMLimit, principal.TPMLimit, tokens, now)
}

func (s *Service) validateCreate(input CreateInput) (CreateInput, error) {
	name, err := normalizeName(input.Name)
	if err != nil {
		return CreateInput{}, err
	}
	models, err := normalizeModels(input.AllowedModels)
	if err != nil {
		return CreateInput{}, err
	}
	allowlist, err := normalizeIPAllowlist(input.IPAllowlist)
	if err != nil {
		return CreateInput{}, err
	}
	if input.ExpiresAt != nil {
		expiresAt := input.ExpiresAt.UTC()
		if !expiresAt.After(s.now().UTC()) {
			return CreateInput{}, ErrInvalidInput
		}
		input.ExpiresAt = &expiresAt
	}
	if err := validateLimits(input.RPMLimit, input.TPMLimit, input.DailyBudgetMinor, input.MonthlyBudgetMinor); err != nil {
		return CreateInput{}, err
	}
	input.Name = name
	input.AllowedModels = models
	input.IPAllowlist = allowlist
	return input, nil
}

func (s *Service) validateUpdate(input UpdateInput) (UpdateInput, error) {
	if input.Name != nil {
		name, err := normalizeName(*input.Name)
		if err != nil {
			return UpdateInput{}, err
		}
		input.Name = &name
	}
	if input.ExpiresAt.Set && input.ExpiresAt.Value != nil {
		expiresAt := input.ExpiresAt.Value.UTC()
		if !expiresAt.After(s.now().UTC()) {
			return UpdateInput{}, ErrInvalidInput
		}
		input.ExpiresAt.Value = &expiresAt
	}
	if input.AllowedModels != nil {
		models, err := normalizeModels(*input.AllowedModels)
		if err != nil {
			return UpdateInput{}, err
		}
		input.AllowedModels = &models
	}
	if input.IPAllowlist != nil {
		allowlist, err := normalizeIPAllowlist(*input.IPAllowlist)
		if err != nil {
			return UpdateInput{}, err
		}
		input.IPAllowlist = &allowlist
	}
	if err := validateLimits(input.RPMLimit.Value, input.TPMLimit.Value, input.DailyBudgetMinor.Value, input.MonthlyBudgetMinor.Value); err != nil {
		return UpdateInput{}, err
	}
	return input, nil
}

func normalizeName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximumKeyNameBytes {
		return "", ErrInvalidInput
	}
	return value, nil
}

func normalizeModels(values []string) ([]string, error) {
	if len(values) > maximumModelsPerKey {
		return nil, ErrInvalidInput
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, ErrInvalidInput
		}
		unique[value] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeIPAllowlist(values []string) ([]string, error) {
	if len(values) > maximumIPRulesPerKey {
		return nil, ErrInvalidInput
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, addressErr := netip.ParseAddr(value)
			if addressErr != nil {
				return nil, ErrInvalidInput
			}
			address = address.Unmap()
			prefix = netip.PrefixFrom(address, address.BitLen())
		} else if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, ErrInvalidInput
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefix = prefix.Masked()
		unique[prefix.String()] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func allowsIP(allowlist []string, source netip.Addr) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, value := range allowlist {
		prefix, err := netip.ParsePrefix(value)
		if err == nil && prefix.Contains(source) {
			return true
		}
	}
	return false
}

func validateLimits(rpm, tpm, daily, monthly *int64) error {
	if (rpm != nil && *rpm <= 0) || (tpm != nil && *tpm <= 0) {
		return ErrInvalidInput
	}
	if (daily != nil && *daily < 0) || (monthly != nil && *monthly < 0) {
		return ErrInvalidInput
	}
	return nil
}
