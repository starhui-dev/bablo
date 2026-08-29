package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// SecretBox encrypts TOTP secrets before they enter PostgreSQL.
type SecretBox struct {
	aead       cipher.AEAD
	keyVersion string
}

// NewSecretBox constructs AES-256-GCM secret storage.
func NewSecretBox(key []byte, keyVersion string) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("auth encryption key must be 32 bytes")
	}
	if keyVersion == "" {
		return nil, errors.New("auth encryption key version is required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create auth cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create auth AEAD: %w", err)
	}
	return &SecretBox{aead: aead, keyVersion: keyVersion}, nil
}

func (b *SecretBox) Seal(factorID, userID uuid.UUID, plaintext []byte) (ciphertext, nonce []byte, keyVersion string, err error) {
	if b == nil || b.aead == nil {
		return nil, nil, "", ErrMFAUnavailable
	}
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, "", fmt.Errorf("generate MFA nonce: %w", err)
	}
	ciphertext = b.aead.Seal(nil, nonce, plaintext, mfaAAD(factorID, userID, b.keyVersion))
	return ciphertext, nonce, b.keyVersion, nil
}

func (b *SecretBox) Open(factorID, userID uuid.UUID, ciphertext, nonce []byte, keyVersion string) ([]byte, error) {
	if b == nil || b.aead == nil {
		return nil, ErrMFAUnavailable
	}
	if keyVersion != b.keyVersion {
		return nil, errors.New("unsupported MFA key version")
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, mfaAAD(factorID, userID, keyVersion))
	if err != nil {
		return nil, errors.New("decrypt MFA secret")
	}
	return plaintext, nil
}

func mfaAAD(factorID, userID uuid.UUID, keyVersion string) []byte {
	return []byte("bablo:mfa:" + factorID.String() + ":" + userID.String() + ":" + keyVersion)
}
