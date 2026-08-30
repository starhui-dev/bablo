package secret

import (
	"bytes"
	"errors"
	"testing"
)

func TestKeyringSealOpenAndAADBinding(t *testing.T) {
	keys := map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)}
	keyring, err := NewKeyring("v2", keys)
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	sealed, err := keyring.Seal([]byte("provider-secret"), []byte("credential-aad-v2"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if sealed.KeyVersion != "v2" || len(sealed.Nonce) != 12 || bytes.Contains(sealed.Ciphertext, []byte("provider-secret")) {
		t.Fatalf("sealed material leaked metadata or plaintext: version=%q nonce=%d ciphertext=%q", sealed.KeyVersion, len(sealed.Nonce), sealed.Ciphertext)
	}
	opened, err := keyring.Open(sealed, []byte("credential-aad-v2"))
	if err != nil || string(opened) != "provider-secret" {
		t.Fatalf("Open() = %q, %v", opened, err)
	}
	if _, err := keyring.Open(sealed, []byte("different-aad")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("AAD mismatch error = %v, want ErrDecrypt", err)
	}
}

func TestKeyringOpensOldVersionAndRejectsInvalidConfiguration(t *testing.T) {
	old, err := NewKeyring("v1", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatalf("old NewKeyring() error = %v", err)
	}
	sealed, err := old.Seal([]byte("old-secret"), []byte("aad"))
	if err != nil {
		t.Fatalf("old Seal() error = %v", err)
	}
	rotated, err := NewKeyring("v2", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatalf("rotated NewKeyring() error = %v", err)
	}
	opened, err := rotated.Open(sealed, []byte("aad"))
	if err != nil || string(opened) != "old-secret" {
		t.Fatalf("rotated Open() = %q, %v", opened, err)
	}
	if _, err := NewKeyring("v3", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}); !errors.Is(err, ErrInvalidKeyring) {
		t.Fatalf("missing current key error = %v, want ErrInvalidKeyring", err)
	}
}
