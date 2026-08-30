package credential

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/id"
	"github.com/starhui-dev/bablo/internal/secret"
)

const (
	maximumPageSize       = 100
	maximumSecretBytes    = 1 << 20
	maximumMetadataFields = 32
)

// Service owns validation, encryption, lifecycle, and pool policy.
type Service struct {
	repository *Repository
	keyring    *secret.Keyring
	now        func() time.Time
}

// NewService constructs a Credential service.
func NewService(repository *Repository, keyring *secret.Keyring) (*Service, error) {
	if repository == nil || keyring == nil {
		return nil, errors.New("credential service requires repository and keyring")
	}
	return &Service{repository: repository, keyring: keyring, now: time.Now}, nil
}

// Create encrypts initial secrets before committing metadata and audit together.
func (s *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID string) (Credential, error) {
	if actorID == uuid.Nil {
		return Credential{}, ErrInvalidInput
	}
	validated, err := validateCreate(input)
	if err != nil {
		return Credential{}, err
	}
	return s.repository.create(ctx, actorID, validated, requestID)
}

// Get returns metadata, secret descriptors, pool memberships, and health only.
func (s *Service) Get(ctx context.Context, credentialID uuid.UUID) (Credential, error) {
	if credentialID == uuid.Nil {
		return Credential{}, ErrInvalidInput
	}
	return s.repository.Get(ctx, credentialID)
}

// List returns non-deleted credentials in stable external-ID and UUID order.
func (s *Service) List(ctx context.Context, cursor string, limit int) (Page, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		if _, _, err := parseListCursor(cursor); err != nil {
			return Page{}, ErrInvalidInput
		}
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > maximumPageSize {
		return Page{}, ErrInvalidInput
	}
	return s.repository.list(ctx, cursor, limit)
}

// Update changes only metadata or status; provider identity is immutable.
func (s *Service) Update(ctx context.Context, actorID, credentialID uuid.UUID, input UpdateInput, requestID string) (Credential, error) {
	if actorID == uuid.Nil || credentialID == uuid.Nil || emptyUpdate(input) {
		return Credential{}, ErrInvalidInput
	}
	validated, err := validateUpdate(input)
	if err != nil {
		return Credential{}, err
	}
	return s.repository.update(ctx, actorID, credentialID, validated, requestID)
}

// Rotate encrypts a new active version and atomically retires the previous one.
func (s *Service) Rotate(ctx context.Context, actorID, credentialID uuid.UUID, input SecretInput, requestID string) (Credential, error) {
	if actorID == uuid.Nil || credentialID == uuid.Nil {
		return Credential{}, ErrInvalidInput
	}
	input.Kind = strings.TrimSpace(input.Kind)
	if err := validateSecretInput(input, ""); err != nil {
		return Credential{}, err
	}
	secretID, err := id.New()
	if err != nil {
		return Credential{}, fmt.Errorf("generate credential secret UUIDv7: %w", err)
	}
	sealed, err := sealSecret(s.keyring, credentialID, secretID, input.Kind, input.Value)
	if err != nil {
		return Credential{}, err
	}
	return s.repository.rotate(ctx, actorID, credentialID, input, sealed, requestID)
}

// Reveal decrypts one active secret for the CPA adapter. It rejects inactive credentials.
func (s *Service) Reveal(ctx context.Context, credentialID uuid.UUID, kind string) (RuntimeCredential, error) {
	if credentialID == uuid.Nil || !validSecretKind(kind) {
		return RuntimeCredential{}, ErrInvalidInput
	}
	stored, owner, err := s.repository.reveal(ctx, credentialID, kind, true)
	if err != nil {
		return RuntimeCredential{}, err
	}
	plaintext, err := s.keyring.Open(secret.Sealed{Ciphertext: stored.Ciphertext, Nonce: stored.Nonce, KeyVersion: stored.KeyVersion}, aad(credentialID, stored.ID, kind, stored.KeyVersion))
	if err != nil {
		return RuntimeCredential{}, ErrSecretUnavailable
	}
	return RuntimeCredential{
		CredentialID:     owner.ID,
		ProviderID:       owner.ProviderID,
		ProviderSlug:     owner.ProviderSlug,
		ExternalStableID: owner.ExternalStableID,
		SourceKind:       owner.SourceKind,
		Region:           owner.Region,
		ProxyRef:         owner.ProxyRef,
		Metadata:         cloneMetadata(owner.Metadata),
		Secrets:          map[string][]byte{kind: plaintext},
	}, nil
}

// RevealAll decrypts all active secrets for runtime adapter registration.
func (s *Service) RevealAll(ctx context.Context, credentialID uuid.UUID) (RuntimeCredential, error) {
	if credentialID == uuid.Nil {
		return RuntimeCredential{}, ErrInvalidInput
	}
	stored, owner, err := s.repository.loadRuntime(ctx, credentialID)
	if err != nil {
		return RuntimeCredential{}, err
	}
	runtime := RuntimeCredential{
		CredentialID:     owner.ID,
		ProviderID:       owner.ProviderID,
		ProviderSlug:     owner.ProviderSlug,
		ExternalStableID: owner.ExternalStableID,
		SourceKind:       owner.SourceKind,
		Region:           owner.Region,
		ProxyRef:         owner.ProxyRef,
		Metadata:         cloneMetadata(owner.Metadata),
		Secrets:          make(map[string][]byte, len(stored)),
	}
	for _, item := range stored {
		plaintext, err := s.keyring.Open(secret.Sealed{Ciphertext: item.Ciphertext, Nonce: item.Nonce, KeyVersion: item.KeyVersion}, aad(credentialID, item.ID, item.Kind, item.KeyVersion))
		if err != nil {
			runtime.Close()
			return RuntimeCredential{}, ErrSecretUnavailable
		}
		runtime.Secrets[item.Kind] = plaintext
	}
	return runtime, nil
}

// ForEachActive decrypts active credentials one at a time for runtime reconciliation.
// The callback must not retain or log plaintext; values are cleared after it returns.
func (s *Service) ForEachActive(ctx context.Context, sink func(RuntimeCredential) error) error {
	if sink == nil {
		return ErrInvalidInput
	}
	credentialIDs, err := s.repository.activeCredentialIDs(ctx)
	if err != nil {
		return err
	}
	for _, credentialID := range credentialIDs {
		value, err := s.RevealAll(ctx, credentialID)
		if errors.Is(err, ErrCredentialInactive) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		err = sink(value)
		value.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Reencrypt creates a new encrypted history version under the current key.
func (s *Service) Reencrypt(ctx context.Context, actorID, credentialID uuid.UUID, kind, requestID string) (Credential, error) {
	if actorID == uuid.Nil || credentialID == uuid.Nil || !validSecretKind(kind) {
		return Credential{}, ErrInvalidInput
	}
	stored, _, err := s.repository.reveal(ctx, credentialID, kind, false)
	if err != nil {
		return Credential{}, err
	}

	plaintext, err := s.keyring.Open(secret.Sealed{Ciphertext: stored.Ciphertext, Nonce: stored.Nonce, KeyVersion: stored.KeyVersion}, aad(credentialID, stored.ID, kind, stored.KeyVersion))
	if err != nil {
		return Credential{}, ErrSecretUnavailable
	}
	defer clearBytes(plaintext)
	secretID, err := id.New()
	if err != nil {
		return Credential{}, fmt.Errorf("generate re-encrypted credential secret UUIDv7: %w", err)
	}
	sealed, err := s.keyring.Seal(plaintext, aad(credentialID, secretID, kind, s.keyring.CurrentVersion()))
	if err != nil {
		return Credential{}, err
	}
	return s.repository.reencrypt(ctx, actorID, credentialID, kind, stored.VersionNo, sealedSecret{ID: secretID, Kind: kind, Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, KeyVersion: sealed.KeyVersion}, requestID)
}

// RecordHealth atomically accepts only observations newer than the stored one.
func (s *Service) RecordHealth(ctx context.Context, credentialID uuid.UUID, input HealthInput) error {
	if credentialID == uuid.Nil {
		return ErrInvalidInput
	}
	if input.ObservedAt.IsZero() {
		input.ObservedAt = s.now().UTC()
	} else {
		input.ObservedAt = input.ObservedAt.UTC()
	}
	if input.ObservedAt.After(s.now().UTC().Add(5*time.Minute)) || strings.TrimSpace(input.ErrorClass) != input.ErrorClass || len(input.ErrorClass) > 80 {
		return ErrInvalidInput
	}
	metadata, err := normalizeMetadata(input.Metadata)
	if err != nil {
		return err
	}
	input.Metadata = metadata
	if input.Succeeded {
		input.ErrorClass = ""
		input.CooldownUntil = nil
	} else {
		input.ErrorClass = strings.TrimSpace(input.ErrorClass)
		if input.ErrorClass == "" {
			return ErrInvalidInput
		}
		if input.CooldownUntil != nil {
			value := input.CooldownUntil.UTC()
			if value.Before(input.ObservedAt) {
				return ErrInvalidInput
			}
			input.CooldownUntil = &value
		}
	}
	return s.repository.recordHealth(ctx, credentialID, input)
}

// CreatePool creates a provider-owned pool for future route targets.
func (s *Service) CreatePool(ctx context.Context, actorID uuid.UUID, input PoolInput, requestID string) (Pool, error) {
	if actorID == uuid.Nil || input.ProviderID == uuid.Nil {
		return Pool{}, ErrInvalidInput
	}
	validated, err := validatePoolInput(input)
	if err != nil {
		return Pool{}, err
	}
	return s.repository.createPool(ctx, actorID, validated, requestID)
}

// ListPools lists pools for one provider.
func (s *Service) ListPools(ctx context.Context, providerID uuid.UUID, cursor string, limit int) (PoolPage, error) {
	if providerID == uuid.Nil || len(cursor) > 120 {
		return PoolPage{}, ErrInvalidInput
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > maximumPageSize {
		return PoolPage{}, ErrInvalidInput
	}
	return s.repository.listPools(ctx, providerID, strings.TrimSpace(cursor), limit)
}

// AddMember enforces provider ownership through the database trigger.
func (s *Service) AddMember(ctx context.Context, actorID, poolID uuid.UUID, input MembershipInput, requestID string) error {
	if actorID == uuid.Nil || poolID == uuid.Nil || input.CredentialID == uuid.Nil || input.Priority < 0 || input.Weight < 1 || input.Weight > 1000 {
		return ErrInvalidInput
	}
	return s.repository.addMember(ctx, actorID, poolID, input, requestID)
}

// RemoveMember removes one pool membership and writes an audit event.
func (s *Service) RemoveMember(ctx context.Context, actorID, poolID, credentialID uuid.UUID, requestID string) error {
	if actorID == uuid.Nil || poolID == uuid.Nil || credentialID == uuid.Nil {
		return ErrInvalidInput
	}
	return s.repository.removeMember(ctx, actorID, poolID, credentialID, requestID)
}

func validateCreate(input CreateInput) (CreateInput, error) {
	input.SourceKind = strings.TrimSpace(input.SourceKind)
	if input.ProviderID == uuid.Nil || !validSourceKind(input.SourceKind) {
		return CreateInput{}, ErrInvalidInput
	}
	stableID, err := normalizeStableID(input.ExternalStableID)
	if err != nil {
		return CreateInput{}, err
	}
	region, err := normalizeRegion(input.Region)
	if err != nil {
		return CreateInput{}, err
	}
	proxyRef, err := normalizeProxyRef(input.ProxyRef)
	if err != nil {
		return CreateInput{}, err
	}
	metadata, err := normalizeMetadata(input.Metadata)
	if err != nil {
		return CreateInput{}, err
	}
	if len(input.Secrets) == 0 || len(input.Secrets) > 8 {
		return CreateInput{}, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(input.Secrets))
	for index := range input.Secrets {
		input.Secrets[index].Kind = strings.TrimSpace(input.Secrets[index].Kind)
		if err := validateSecretInput(input.Secrets[index], input.SourceKind); err != nil {
			return CreateInput{}, err
		}
		if _, exists := seen[input.Secrets[index].Kind]; exists {
			return CreateInput{}, ErrInvalidInput
		}
		seen[input.Secrets[index].Kind] = struct{}{}
	}
	if !hasRequiredSecret(input.SourceKind, seen) {
		return CreateInput{}, ErrInvalidInput
	}
	input.ExternalStableID = stableID
	input.Region = region
	input.ProxyRef = proxyRef
	input.Metadata = metadata
	return input, nil
}

func validateSecretInput(input SecretInput, source string) error {
	input.Kind = strings.TrimSpace(input.Kind)
	if !validSecretKind(input.Kind) || (source != "" && !sourceAllowsSecret(source, input.Kind)) || len(input.Value) == 0 || len(input.Value) > maximumSecretBytes || strings.ContainsAny(input.Value, "\x00\r\n") {
		return ErrInvalidInput
	}
	return nil
}

func hasRequiredSecret(source string, kinds map[string]struct{}) bool {
	var required string
	switch source {
	case SourceOAuth:
		required = SecretOAuthAccess
	case SourceAPIKey:
		required = SecretAPIKey
	case SourceServiceAccount:
		required = SecretServiceAcct
	case SourceCustom:
		required = SecretCustom
	default:
		return false
	}
	_, ok := kinds[required]
	return ok
}

func validateUpdate(input UpdateInput) (UpdateInput, error) {
	if input.Region != nil {
		value, err := normalizeRegion(*input.Region)
		if err != nil {
			return UpdateInput{}, err
		}
		input.Region = &value
	}
	if input.ProxyRef != nil {
		value, err := normalizeProxyRef(*input.ProxyRef)
		if err != nil {
			return UpdateInput{}, err
		}
		input.ProxyRef = &value
	}
	if input.Metadata != nil {
		value, err := normalizeMetadata(*input.Metadata)
		if err != nil {
			return UpdateInput{}, err
		}
		input.Metadata = &value
	}
	if input.Status != nil {
		value := strings.TrimSpace(*input.Status)
		if !validStatus(value) {
			return UpdateInput{}, ErrInvalidInput
		}
		input.Status = &value
	}
	return input, nil
}

func validatePoolInput(input PoolInput) (PoolInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.ProviderID == uuid.Nil || input.Name == "" || len(input.Name) > 120 || strings.ContainsAny(input.Name, "\r\n") {
		return PoolInput{}, ErrInvalidInput
	}
	metadata, err := normalizeMetadata(input.Metadata)
	if err != nil {
		return PoolInput{}, err
	}
	input.Metadata = metadata
	return input, nil
}

func emptyUpdate(input UpdateInput) bool {
	return input.Region == nil && input.ProxyRef == nil && input.Metadata == nil && input.Status == nil
}

func (r *Repository) sealInputs(credentialID uuid.UUID, inputs []SecretInput) ([]sealedSecret, error) {
	sealed := make([]sealedSecret, 0, len(inputs))
	for _, input := range inputs {
		secretID, err := id.New()
		if err != nil {
			return nil, fmt.Errorf("generate credential secret UUIDv7: %w", err)
		}
		item, err := sealSecret(r.keyring, credentialID, secretID, input.Kind, input.Value)
		if err != nil {
			return nil, err
		}
		sealed = append(sealed, item)
	}
	return sealed, nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
