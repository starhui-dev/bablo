package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	secretMarker       = "bablo_sk_"
	secretEntropyBytes = 32
	secretEncodedBytes = 43
	displayPrefixBytes = 8
)

type secretMaterial struct {
	Plaintext string
	Prefix    string
	Hash      [sha256.Size]byte
}

type storedSecret struct {
	Prefix string
	Hash   [sha256.Size]byte
}

func (material secretMaterial) stored() storedSecret {
	return storedSecret{Prefix: material.Prefix, Hash: material.Hash}
}

func generateSecret() (secretMaterial, error) {
	entropy := make([]byte, secretEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return secretMaterial{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(entropy)
	plaintext := secretMarker + encoded
	return secretMaterial{
		Plaintext: plaintext,
		Prefix:    secretMarker + encoded[:displayPrefixBytes],
		Hash:      sha256.Sum256([]byte(plaintext)),
	}, nil
}

func hashSecret(secret string) ([sha256.Size]byte, error) {
	if len(secret) != len(secretMarker)+secretEncodedBytes || !strings.HasPrefix(secret, secretMarker) {
		return [sha256.Size]byte{}, ErrInvalidKey
	}
	encoded := strings.TrimPrefix(secret, secretMarker)
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != secretEntropyBytes {
		return [sha256.Size]byte{}, ErrInvalidKey
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return [sha256.Size]byte{}, ErrInvalidKey
	}
	return sha256.Sum256([]byte(secret)), nil
}

func validateStoredSecret(material storedSecret) error {
	if material.Prefix == "" || material.Hash == ([sha256.Size]byte{}) {
		return errors.New("API key stored secret material is incomplete")
	}
	return nil
}
