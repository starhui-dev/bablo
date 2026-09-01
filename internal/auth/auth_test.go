package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
)

func TestPasswordHashVerifyAndRehashPolicy(t *testing.T) {
	password := "correct horse battery staple"
	encoded, version, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash = %q, want current Argon2id PHC profile", encoded)
	}
	if version != CurrentPasswordParamsVersion {
		t.Fatalf("version = %q, want %q", version, CurrentPasswordParamsVersion)
	}
	valid, err := VerifyPassword(password, encoded)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword(correct) = %v, %v", valid, err)
	}
	valid, err = VerifyPassword("wrong password value", encoded)
	if err != nil || valid {
		t.Fatalf("VerifyPassword(wrong) = %v, %v", valid, err)
	}
	if PasswordNeedsRehash(encoded, version) {
		t.Fatal("current hash unexpectedly needs rehash")
	}
	if !PasswordNeedsRehash(encoded, "argon2id-old") {
		t.Fatal("old parameter version must need rehash")
	}
	malformed := strings.Replace(encoded, "m=19456,t=2,p=1", "m=19456,t=2,p=1,x=1", 1)
	if _, err := VerifyPassword(password, malformed); err == nil {
		t.Fatal("VerifyPassword() accepted trailing Argon2id parameters")
	}
}

func TestPasswordPolicyRejectsShortAndOversizedValues(t *testing.T) {
	for _, password := range []string{"too-short", strings.Repeat("x", maximumPasswordBytes+1)} {
		if _, _, err := HashPassword(password); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("HashPassword(%d bytes) error = %v, want ErrInvalidInput", len(password), err)
		}
	}
}

func TestOpaqueTokenHashAndSecretBoxBinding(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, opaqueTokenBytes))
	token, digest, err := newOpaqueTokenFrom(random)
	if err != nil {
		t.Fatalf("newOpaqueTokenFrom() error = %v", err)
	}
	if len(token) != 43 || digest != hashToken(token) {
		t.Fatalf("token length/hash mismatch: %d", len(token))
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	box, err := NewSecretBox(key, "v1")
	if err != nil {
		t.Fatalf("NewSecretBox() error = %v", err)
	}
	factorID := uuid.Must(uuid.NewV7())
	userID := uuid.Must(uuid.NewV7())
	ciphertext, nonce, version, err := box.Seal(factorID, userID, []byte("JBSWY3DPEHPK3PXP"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	plaintext, err := box.Open(factorID, userID, ciphertext, nonce, version)
	if err != nil || string(plaintext) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Open() = %q, %v", plaintext, err)
	}
	if _, err := box.Open(factorID, uuid.Must(uuid.NewV7()), ciphertext, nonce, version); err == nil {
		t.Fatal("Open() accepted ciphertext for another user")
	}
}

func TestLoginLimiterExpiresWithoutPermanentLock(t *testing.T) {
	limiter := NewMemoryAttemptLimiter(2, 10, time.Minute, 2)
	now := time.Unix(1_700_000_000, 0)
	if !limiter.Allow(t.Context(), "a@example.test", "192.0.2.1", now) || !limiter.Allow(t.Context(), "a@example.test", "192.0.2.1", now) {
		t.Fatal("initial attempts must be allowed")
	}
	if limiter.Allow(t.Context(), "a@example.test", "192.0.2.1", now) {
		t.Fatal("attempt above fixed-window limit must be denied")
	}
	if !limiter.Allow(t.Context(), "a@example.test", "192.0.2.1", now.Add(time.Minute)) {
		t.Fatal("expired window must allow a new attempt")
	}
}

func TestLoginLimiterBoundsAccountAndAddressIndependently(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	accountLimiter := NewMemoryAttemptLimiter(2, 10, time.Minute, 100)
	if !accountLimiter.Allow(t.Context(), "a@example.test", "192.0.2.1", now) ||
		!accountLimiter.Allow(t.Context(), "a@example.test", "192.0.2.2", now) {
		t.Fatal("initial account attempts must be allowed")
	}
	if accountLimiter.Allow(t.Context(), "a@example.test", "192.0.2.3", now) {
		t.Fatal("rotating addresses must not bypass the account limit")
	}

	addressLimiter := NewMemoryAttemptLimiter(10, 3, time.Minute, 100)
	for _, email := range []string{"a@example.test", "b@example.test", "c@example.test"} {
		if !addressLimiter.Allow(t.Context(), email, "192.0.2.9", now) {
			t.Fatalf("address attempt for %s was denied early", email)
		}
	}
	if addressLimiter.Allow(t.Context(), "d@example.test", "192.0.2.9", now) {
		t.Fatal("rotating accounts must not bypass the address limit")
	}
}

func TestTrustedClientAddressRejectsUntrustedForwardingHeaders(t *testing.T) {
	request := httptest.NewRequest("POST", "http://bablo.test/api/v1/auth/login", nil)
	request.RemoteAddr = "198.51.100.20:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	if address := trustedClientAddress(request, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}); address != "198.51.100.20" {
		t.Fatalf("untrusted proxy address = %q", address)
	}

	request.RemoteAddr = "10.0.0.2:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if address := trustedClientAddress(request, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}); address != "203.0.113.9" {
		t.Fatalf("trusted proxy chain address = %q", address)
	}

	request.Header.Set("X-Forwarded-For", "not-an-ip")
	if address := trustedClientAddress(request, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}); address != "10.0.0.2" {
		t.Fatalf("malformed proxy chain address = %q", address)
	}
}

func TestRedisAttemptLimiterBoundsAccountAndAddress(t *testing.T) {
	rawURL := os.Getenv("BABLO_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("BABLO_TEST_REDIS_URL is not set")
	}
	now := time.Now().UTC()
	namespace := "test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	limiter, err := NewRedisAttemptLimiter(context.Background(), rawURL, namespace, 2, 3, time.Minute)
	if err != nil {
		t.Fatalf("NewRedisAttemptLimiter() error = %v", err)
	}
	t.Cleanup(func() { _ = limiter.Close() })
	if !limiter.Allow(t.Context(), "a@example.test", "192.0.2.1", now) ||
		!limiter.Allow(t.Context(), "a@example.test", "192.0.2.2", now) {
		t.Fatal("initial Redis account attempts must be allowed")
	}
	if limiter.Allow(t.Context(), "a@example.test", "192.0.2.3", now) {
		t.Fatal("Redis account limit was bypassed with address rotation")
	}

	addressLimiter, err := NewRedisAttemptLimiter(context.Background(), rawURL, namespace+"-ip", 10, 3, time.Minute)
	if err != nil {
		t.Fatalf("NewRedisAttemptLimiter(address) error = %v", err)
	}
	t.Cleanup(func() { _ = addressLimiter.Close() })
	for _, email := range []string{"a@example.test", "b@example.test", "c@example.test"} {
		if !addressLimiter.Allow(t.Context(), email, "192.0.2.9", now) {
			t.Fatalf("Redis address attempt for %s was denied early", email)
		}
	}
	if addressLimiter.Allow(t.Context(), "d@example.test", "192.0.2.9", now) {
		t.Fatal("Redis address limit was bypassed with account rotation")
	}
}

func TestTOTPValidationRejectsReplay(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Bablo", AccountName: "user@example.test"})
	if err != nil {
		t.Fatalf("totp.Generate() error = %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	code, err := totp.GenerateCode(key.Secret(), now)
	if err != nil {
		t.Fatalf("totp.GenerateCode() error = %v", err)
	}
	counter, err := validateTOTP(key.Secret(), code, now, nil)
	if err != nil {
		t.Fatalf("validateTOTP() error = %v", err)
	}
	if _, err := validateTOTP(key.Secret(), code, now, &counter); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("replayed validateTOTP() error = %v, want ErrMFAInvalid", err)
	}
}
