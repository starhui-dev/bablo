package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// AuthConfig contains Web Session security configuration. EncryptionKey is
// secret-bearing and must never be logged.
type AuthConfig struct {
	EncryptionKey   []byte
	KeyVersion      string
	SessionTTL      time.Duration
	AllowedOrigin   string
	CookieDomain    string
	CookieSecure    bool
	RequireAdminMFA bool
	Issuer          string
}

// LoadAuth reads Web Session authentication configuration.
func LoadAuth(environment string) (AuthConfig, error) {
	sessionTTL, err := durationEnv("BABLO_AUTH_SESSION_TTL", 12*time.Hour)
	if err != nil {
		return AuthConfig{}, err
	}
	if sessionTTL < 5*time.Minute || sessionTTL > 7*24*time.Hour {
		return AuthConfig{}, errors.New("BABLO_AUTH_SESSION_TTL must be between 5m and 168h")
	}
	requireAdminMFA, err := boolEnv("BABLO_AUTH_REQUIRE_ADMIN_MFA", true)
	if err != nil {
		return AuthConfig{}, err
	}
	production := strings.EqualFold(strings.TrimSpace(environment), "production")
	if production && !requireAdminMFA {
		return AuthConfig{}, errors.New("BABLO_AUTH_REQUIRE_ADMIN_MFA cannot be disabled in production")
	}

	origin, err := normalizeOrigin(os.Getenv("BABLO_WEB_ORIGIN"))
	if err != nil {
		return AuthConfig{}, err
	}
	if production && origin == "" {
		return AuthConfig{}, errors.New("BABLO_WEB_ORIGIN is required in production")
	}

	key, err := decodeAuthKey(os.Getenv("BABLO_AUTH_ENCRYPTION_KEY"))
	if err != nil {
		return AuthConfig{}, err
	}
	if production && len(key) == 0 {
		return AuthConfig{}, errors.New("BABLO_AUTH_ENCRYPTION_KEY is required in production")
	}

	cookieDomain := strings.TrimSpace(os.Getenv("BABLO_AUTH_COOKIE_DOMAIN"))
	if strings.ContainsAny(cookieDomain, " /\\") {
		return AuthConfig{}, errors.New("BABLO_AUTH_COOKIE_DOMAIN is invalid")
	}

	return AuthConfig{
		EncryptionKey:   key,
		KeyVersion:      envOr("BABLO_AUTH_KEY_VERSION", "v1"),
		SessionTTL:      sessionTTL,
		AllowedOrigin:   origin,
		CookieDomain:    cookieDomain,
		CookieSecure:    production,
		RequireAdminMFA: requireAdminMFA,
		Issuer:          envOr("BABLO_AUTH_TOTP_ISSUER", "Bablo"),
	}, nil
}

func decodeAuthKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("BABLO_AUTH_ENCRYPTION_KEY must be standard base64: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("BABLO_AUTH_ENCRYPTION_KEY must decode to exactly 32 bytes")
	}
	return key, nil
}

func normalizeOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("BABLO_WEB_ORIGIN must be an absolute http(s) origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("BABLO_WEB_ORIGIN must use http or https")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("BABLO_WEB_ORIGIN must not include a path")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", key, value)
	}
	return parsed, nil
}
