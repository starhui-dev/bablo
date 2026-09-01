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

func TestLoadProductionRequiresRedis(t *testing.T) {
	t.Setenv("BABLO_ENV", "production")
	t.Setenv("BABLO_REDIS_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BABLO_REDIS_URL") {
		t.Fatalf("Load() error = %v, want missing production Redis", err)
	}
	t.Setenv("BABLO_REDIS_URL", "redis://redis:6379/0")
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() with Redis error = %v", err)
	}
	if config.RedisURL != "redis://redis:6379/0" {
		t.Fatalf("Load() RedisURL = %q", config.RedisURL)
	}
}

func TestTrustedProxyCIDRsRequireValidExplicitPrefixes(t *testing.T) {
	t.Setenv("BABLO_TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 192.0.2.7/24,10.0.0.0/8")
	prefixes, err := prefixListEnv("BABLO_TRUSTED_PROXY_CIDRS")
	if err != nil {
		t.Fatalf("prefixListEnv() error = %v", err)
	}
	if len(prefixes) != 2 || prefixes[0].String() != "10.0.0.0/8" || prefixes[1].String() != "192.0.2.0/24" {
		t.Fatalf("trusted proxy prefixes = %#v", prefixes)
	}
	t.Setenv("BABLO_TRUSTED_PROXY_CIDRS", "not-a-prefix")
	if _, err := prefixListEnv("BABLO_TRUSTED_PROXY_CIDRS"); err == nil {
		t.Fatal("invalid trusted proxy prefix was accepted")
	}
}
