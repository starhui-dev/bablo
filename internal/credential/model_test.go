package credential

import (
	"errors"
	"testing"
)

func TestNormalizeProxyRefRejectsEmbeddedCredentialsAndQuery(t *testing.T) {
	if value, err := normalizeProxyRef("https://proxy.example.test:8443"); err != nil || value != "https://proxy.example.test:8443" {
		t.Fatalf("normalizeProxyRef(valid) = %q, %v", value, err)
	}
	for _, value := range []string{
		"https://user:password@proxy.example.test",
		"https://proxy.example.test?token=secret",
		"file:///tmp/socket",
		"proxy ref with spaces",
	} {
		if _, err := normalizeProxyRef(value); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("normalizeProxyRef(%q) error = %v, want ErrInvalidInput", value, err)
		}
	}
}

func TestNormalizeMetadataRejectsSensitiveAndAmbiguousKeys(t *testing.T) {
	if _, err := normalizeMetadata(map[string]string{"api_token": "value"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sensitive metadata error = %v", err)
	}
	if _, err := normalizeMetadata(map[string]string{"Team": "a", "team": "b"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("normalized duplicate metadata error = %v", err)
	}
	metadata, err := normalizeMetadata(map[string]string{" Account_Email ": " owner@example.test "})
	if err != nil || metadata["account_email"] != "owner@example.test" {
		t.Fatalf("normalizeMetadata() = %#v, %v", metadata, err)
	}
}
