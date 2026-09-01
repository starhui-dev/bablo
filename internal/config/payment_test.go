package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadPaymentDefaultsToNoProvider(t *testing.T) {
	for _, key := range []string{
		"BABLO_PAYMENT_ORDER_TTL", "BABLO_PAYMENT_WEBHOOK_TOLERANCE",
		"BABLO_PAYMENT_EXPIRATION_INTERVAL", "BABLO_PAYMENT_FIXTURE_ENABLED",
		"BABLO_PAYMENT_ALLOW_TEST_PROVIDERS",
		"BABLO_PAYMENT_FIXTURE_MERCHANT_ID", "BABLO_PAYMENT_FIXTURE_SECRET",
		"BABLO_PAYMENT_STRIPE_ENABLED", "BABLO_PAYMENT_STRIPE_SECRET_KEY",
		"BABLO_PAYMENT_STRIPE_WEBHOOK_SECRET", "BABLO_PAYMENT_STRIPE_SUCCESS_URL",
		"BABLO_PAYMENT_STRIPE_CANCEL_URL", "BABLO_PAYMENT_STRIPE_ACCOUNT_ID",
		"BABLO_PAYMENT_VOUCHER_ENABLED", "BABLO_PAYMENT_VOUCHER_KEY_VERSION",
		"BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEY", "BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEYS",
	} {
		t.Setenv(key, "")
	}
	config, err := LoadPayment("development")
	if err != nil {
		t.Fatalf("LoadPayment() error = %v", err)
	}
	if config.FixtureEnabled || config.StripeEnabled || config.OrderTTL != time.Hour || config.WebhookTolerance != 5*time.Minute || config.ExpirationInterval != time.Minute {
		t.Fatalf("LoadPayment() = %#v", config)
	}
}

func TestLoadPaymentFixtureIsDevelopmentOnly(t *testing.T) {
	t.Setenv("BABLO_PAYMENT_FIXTURE_ENABLED", "true")
	t.Setenv("BABLO_PAYMENT_FIXTURE_MERCHANT_ID", "merchant-test")
	t.Setenv("BABLO_PAYMENT_FIXTURE_SECRET", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	if _, err := LoadPayment("development"); err == nil {
		t.Fatal("LoadPayment(development) accepted fixture without test-provider opt-in")
	}
	t.Setenv("BABLO_PAYMENT_ALLOW_TEST_PROVIDERS", "true")
	if _, err := LoadPayment("production"); err == nil {
		t.Fatal("LoadPayment(production) error = nil, want fixture rejection")
	}
	config, err := LoadPayment("development")
	if err != nil {
		t.Fatalf("LoadPayment(development) error = %v", err)
	}
	if !config.FixtureEnabled || config.FixtureMerchant != "merchant-test" || len(config.FixtureSecret) != 32 {
		t.Fatalf("LoadPayment(development) = %#v", config)
	}
}

func TestLoadPaymentStripeRequiresLiveProductionConfiguration(t *testing.T) {
	for _, key := range []string{
		"BABLO_PAYMENT_ORDER_TTL", "BABLO_PAYMENT_FIXTURE_ENABLED",
		"BABLO_PAYMENT_STRIPE_ENABLED", "BABLO_PAYMENT_STRIPE_SECRET_KEY",
		"BABLO_PAYMENT_STRIPE_WEBHOOK_SECRET", "BABLO_PAYMENT_STRIPE_SUCCESS_URL",
		"BABLO_PAYMENT_STRIPE_CANCEL_URL", "BABLO_PAYMENT_STRIPE_ACCOUNT_ID",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("BABLO_PAYMENT_STRIPE_ENABLED", "true")
	t.Setenv("BABLO_PAYMENT_STRIPE_SECRET_KEY", "sk_test_payment_configuration")
	t.Setenv("BABLO_PAYMENT_STRIPE_WEBHOOK_SECRET", "whsec_payment_configuration")
	t.Setenv("BABLO_PAYMENT_STRIPE_ACCOUNT_ID", "acct_payment_configuration")
	t.Setenv("BABLO_PAYMENT_STRIPE_SUCCESS_URL", "http://localhost:5173/billing/success")
	t.Setenv("BABLO_PAYMENT_STRIPE_CANCEL_URL", "http://localhost:5173/billing")
	if _, err := LoadPayment("development"); err == nil {
		t.Fatal("LoadPayment(development) accepted test Stripe without opt-in")
	}
	t.Setenv("BABLO_PAYMENT_ALLOW_TEST_PROVIDERS", "true")
	config, err := LoadPayment("development")
	if err != nil {
		t.Fatalf("LoadPayment(development) error = %v", err)
	}
	if !config.StripeEnabled || config.StripeSecretKey == "" || config.StripeSuccessURL == "" {
		t.Fatalf("LoadPayment(development) = %#v", config)
	}
	if _, err := LoadPayment("production"); err == nil {
		t.Fatal("LoadPayment(production) accepted test Stripe credentials")
	}
	t.Setenv("BABLO_PAYMENT_STRIPE_SECRET_KEY", "sk_live_payment_configuration")
	if _, err := LoadPayment("production"); err == nil {
		t.Fatal("LoadPayment(production) accepted non-HTTPS return URLs")
	}
	t.Setenv("BABLO_PAYMENT_STRIPE_SUCCESS_URL", "https://console.bablo.ai/billing/success")
	t.Setenv("BABLO_PAYMENT_STRIPE_CANCEL_URL", "https://console.bablo.ai/billing")
	if _, err := LoadPayment("production"); err != nil {
		t.Fatalf("LoadPayment(production) error = %v", err)
	}
}
func TestLoadPaymentRejectsUnknownEnvironment(t *testing.T) {
	if _, err := LoadPayment("prod"); err == nil {
		t.Fatal("LoadPayment(prod) accepted unsupported environment")
	}
}

func TestLoadPaymentVoucherRequiresVersionedKey(t *testing.T) {
	for _, key := range []string{
		"BABLO_PAYMENT_VOUCHER_ENABLED", "BABLO_PAYMENT_VOUCHER_KEY_VERSION",
		"BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEY", "BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEYS",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("BABLO_PAYMENT_VOUCHER_ENABLED", "true")
	if _, err := LoadPayment("development"); err == nil {
		t.Fatal("LoadPayment() accepted enabled vouchers without encryption key")
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Setenv("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	config, err := LoadPayment("production")
	if err != nil {
		t.Fatalf("LoadPayment(production) error = %v", err)
	}
	if !config.VoucherEnabled || config.VoucherCurrentVersion != "v1" || len(config.VoucherKeys["v1"]) != 32 {
		t.Fatalf("LoadPayment(production) = %#v", config)
	}
	t.Setenv("BABLO_PAYMENT_VOUCHER_KEY_VERSION", "v2")
	t.Setenv("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEY", "")
	t.Setenv("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEYS", "v1="+base64.StdEncoding.EncodeToString(key))
	if _, err := LoadPayment("production"); err == nil {
		t.Fatal("LoadPayment() accepted missing current voucher key version")
	}
}
