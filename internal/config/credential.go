package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// CredentialConfig contains versioned application-layer keys for upstream
// Credential secrets. Key material must never be logged or persisted.
type CredentialConfig struct {
	Keys           map[string][]byte
	CurrentVersion string
}

// LoadCredential reads Credential encryption configuration. In development a
// missing key disables the Credential HTTP surface; production requires one.
func LoadCredential(environment string) (CredentialConfig, error) {
	current := strings.TrimSpace(envOr("BABLO_CREDENTIAL_KEY_VERSION", "v1"))
	if !validCredentialKeyVersion(current) {
		return CredentialConfig{}, errors.New("BABLO_CREDENTIAL_KEY_VERSION is invalid")
	}
	keys := make(map[string][]byte)
	if raw := strings.TrimSpace(os.Getenv("BABLO_CREDENTIAL_ENCRYPTION_KEY")); raw != "" {
		key, err := decodeCredentialKey(raw)
		if err != nil {
			return CredentialConfig{}, err
		}
		keys[current] = key
	}
	if raw := strings.TrimSpace(os.Getenv("BABLO_CREDENTIAL_ENCRYPTION_KEYS")); raw != "" {
		for _, item := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
			version := ""
			if len(parts) == 2 {
				version = strings.TrimSpace(parts[0])
			}
			if !validCredentialKeyVersion(version) {
				return CredentialConfig{}, errors.New("BABLO_CREDENTIAL_ENCRYPTION_KEYS must use version=base64 entries")
			}
			if _, exists := keys[version]; exists {
				return CredentialConfig{}, fmt.Errorf("credential encryption key version %q is configured more than once", version)
			}
			key, err := decodeCredentialKey(parts[1])
			if err != nil {
				return CredentialConfig{}, err
			}
			keys[version] = key
		}
	}
	if len(keys) == 0 {
		if strings.EqualFold(strings.TrimSpace(environment), "production") {
			return CredentialConfig{}, errors.New("BABLO_CREDENTIAL_ENCRYPTION_KEY is required in production")
		}
		return CredentialConfig{CurrentVersion: current, Keys: nil}, nil
	}
	if _, ok := keys[current]; !ok {
		return CredentialConfig{}, fmt.Errorf("current credential key version %q is not configured", current)
	}
	return CredentialConfig{CurrentVersion: current, Keys: keys}, nil
}

func validCredentialKeyVersion(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for index, item := range []byte(value) {
		if (item >= 'a' && item <= 'z') || (item >= 'A' && item <= 'Z') || (item >= '0' && item <= '9') || (index > 0 && (item == '.' || item == '_' || item == '-')) {
			continue
		}
		return false
	}
	return true
}

func decodeCredentialKey(value string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("credential encryption key must be standard base64: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("credential encryption key must decode to exactly 32 bytes")
	}
	return key, nil
}
