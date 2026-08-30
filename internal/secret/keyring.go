// Package secret provides versioned application-layer AEAD encryption without
// exposing key material to repositories, logs, or persisted metadata.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
)

var keyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var (
	ErrInvalidKeyring = errors.New("invalid encryption keyring")
	ErrUnknownVersion = errors.New("unknown encryption key version")
	ErrDecrypt        = errors.New("decrypt secret")
)

// Sealed contains ciphertext metadata safe for PostgreSQL persistence.
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion string
}

// Keyring is immutable after construction. New writes use CurrentVersion while
// old versions remain available for online re-encryption.
type Keyring struct {
	current string
	keys    map[string]cipher.AEAD
}

// NewKeyring validates AES-256 keys and selects the active write version.
func NewKeyring(current string, keys map[string][]byte) (*Keyring, error) {
	if !keyVersionPattern.MatchString(current) || len(keys) == 0 || len(keys) > 16 {
		return nil, ErrInvalidKeyring
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for version, key := range keys {
		if !keyVersionPattern.MatchString(version) || len(key) != 32 {
			return nil, ErrInvalidKeyring
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create cipher for key version %s: %w", version, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create AEAD for key version %s: %w", version, err)
		}
		aeads[version] = aead
	}
	if _, exists := aeads[current]; !exists {
		return nil, ErrInvalidKeyring
	}
	return &Keyring{current: current, keys: aeads}, nil
}

// CurrentVersion is the key version used for new ciphertext.
func (k *Keyring) CurrentVersion() string {
	if k == nil {
		return ""
	}
	return k.current
}

// Seal encrypts plaintext with the active key. AAD must bind the owning record.
func (k *Keyring) Seal(plaintext, aad []byte) (Sealed, error) {
	if k == nil {
		return Sealed{}, ErrInvalidKeyring
	}
	aead := k.keys[k.current]
	if aead == nil {
		return Sealed{}, ErrUnknownVersion
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, fmt.Errorf("generate secret nonce: %w", err)
	}
	return Sealed{
		Ciphertext: aead.Seal(nil, nonce, plaintext, aad),
		Nonce:      nonce,
		KeyVersion: k.current,
	}, nil
}

// Open authenticates and decrypts one stored ciphertext version.
func (k *Keyring) Open(sealed Sealed, aad []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrInvalidKeyring
	}
	aead := k.keys[sealed.KeyVersion]
	if aead == nil {
		return nil, ErrUnknownVersion
	}
	if len(sealed.Nonce) != aead.NonceSize() || len(sealed.Ciphertext) <= aead.Overhead() {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
