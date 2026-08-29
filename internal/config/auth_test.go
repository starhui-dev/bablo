package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadAuthProductionRequiresOriginKeyAndAdminMFA(t *testing.T) {
	t.Setenv("BABLO_WEB_ORIGIN", "")
	t.Setenv("BABLO_AUTH_ENCRYPTION_KEY", "")
	if _, err := LoadAuth("production"); err == nil || !strings.Contains(err.Error(), "BABLO_WEB_ORIGIN") {
		t.Fatalf("LoadAuth(production) error = %v, want missing origin", err)
	}

	t.Setenv("BABLO_WEB_ORIGIN", "https://console.example/")
	if _, err := LoadAuth("production"); err == nil || !strings.Contains(err.Error(), "BABLO_AUTH_ENCRYPTION_KEY") {
		t.Fatalf("LoadAuth(production) error = %v, want missing encryption key", err)
	}

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("BABLO_AUTH_ENCRYPTION_KEY", key)
	t.Setenv("BABLO_AUTH_REQUIRE_ADMIN_MFA", "false")
	if _, err := LoadAuth("production"); err == nil || !strings.Contains(err.Error(), "cannot be disabled") {
		t.Fatalf("LoadAuth(production) error = %v, want admin MFA gate", err)
	}
}

func TestLoadAuthNormalizesSafeSettings(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("BABLO_WEB_ORIGIN", "https://console.example/")
	t.Setenv("BABLO_AUTH_ENCRYPTION_KEY", key)
	t.Setenv("BABLO_AUTH_SESSION_TTL", "6h")
	t.Setenv("BABLO_AUTH_REQUIRE_ADMIN_MFA", "true")
	cfg, err := LoadAuth("production")
	if err != nil {
		t.Fatalf("LoadAuth() error = %v", err)
	}
	if cfg.AllowedOrigin != "https://console.example" || cfg.SessionTTL != 6*time.Hour || !cfg.CookieSecure || len(cfg.EncryptionKey) != 32 {
		t.Fatalf("LoadAuth() origin=%q ttl=%v secure=%v key_bytes=%d", cfg.AllowedOrigin, cfg.SessionTTL, cfg.CookieSecure, len(cfg.EncryptionKey))
	}
}
