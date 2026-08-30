package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadCredentialProductionRequiresCurrentKey(t *testing.T) {
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEY", "")
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEYS", "")
	t.Setenv("BABLO_CREDENTIAL_KEY_VERSION", "v1")
	if _, err := LoadCredential("production"); err == nil || !strings.Contains(err.Error(), "required in production") {
		t.Fatalf("LoadCredential(production) error = %v, want required key", err)
	}
	cfg, err := LoadCredential("development")
	if err != nil || cfg.CurrentVersion != "v1" || cfg.Keys != nil {
		t.Fatalf("LoadCredential(development) = %+v, %v", cfg, err)
	}
}

func TestLoadCredentialSupportsVersionedRotationKeys(t *testing.T) {
	current := bytes.Repeat([]byte{2}, 32)
	old := bytes.Repeat([]byte{1}, 32)
	t.Setenv("BABLO_CREDENTIAL_KEY_VERSION", "v2")
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(current))
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEYS", "v1="+base64.StdEncoding.EncodeToString(old))
	cfg, err := LoadCredential("production")
	if err != nil {
		t.Fatalf("LoadCredential() error = %v", err)
	}
	if cfg.CurrentVersion != "v2" || !bytes.Equal(cfg.Keys["v2"], current) || !bytes.Equal(cfg.Keys["v1"], old) {
		t.Fatalf("LoadCredential() = version %q keys %#v", cfg.CurrentVersion, cfg.Keys)
	}
}

func TestLoadCredentialRejectsAmbiguousOrInvalidKeys(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	t.Setenv("BABLO_CREDENTIAL_KEY_VERSION", "v1")
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEY", key)
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEYS", "v1="+key)
	if _, err := LoadCredential("production"); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate version error = %v", err)
	}
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEY", "")
	t.Setenv("BABLO_CREDENTIAL_ENCRYPTION_KEYS", "v2="+key)
	if _, err := LoadCredential("production"); err == nil || !strings.Contains(err.Error(), "current credential key") {
		t.Fatalf("missing current version error = %v", err)
	}
	t.Setenv("BABLO_CREDENTIAL_KEY_VERSION", "bad version")
	if _, err := LoadCredential("production"); err == nil || !strings.Contains(err.Error(), "KEY_VERSION") {
		t.Fatalf("invalid version error = %v", err)
	}
}
