package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// PaymentConfig contains provider-neutral timing plus explicitly enabled
// payment adapters. The HMAC fixture is forbidden in production.
type PaymentConfig struct {
	OrderTTL             time.Duration
	WebhookTolerance     time.Duration
	ExpirationInterval   time.Duration
	FixtureEnabled       bool
	FixtureMerchant      string
	FixtureSecret        []byte
	StripeEnabled        bool
	StripeSecretKey      string
	StripeWebhookSecret  string
	StripeSuccessURL     string
	StripeCancelURL      string
	StripeAccountID      string
	VoucherEnabled       bool
	VoucherKeys          map[string][]byte
	VoucherCurrentVersion string
}

func LoadPayment(environment string) (PaymentConfig, error) {
	orderTTL, err := durationEnv("BABLO_PAYMENT_ORDER_TTL", time.Hour)
	if err != nil {
		return PaymentConfig{}, err
	}
	if orderTTL < time.Minute || orderTTL > 24*time.Hour {
		return PaymentConfig{}, errors.New("BABLO_PAYMENT_ORDER_TTL must be between 1m and 24h")
	}
	webhookTolerance, err := durationEnv("BABLO_PAYMENT_WEBHOOK_TOLERANCE", 5*time.Minute)
	if err != nil {
		return PaymentConfig{}, err
	}
	if webhookTolerance < time.Second || webhookTolerance > time.Hour {
		return PaymentConfig{}, errors.New("BABLO_PAYMENT_WEBHOOK_TOLERANCE must be between 1s and 1h")
	}
	expirationInterval, err := durationEnv("BABLO_PAYMENT_EXPIRATION_INTERVAL", time.Minute)
	if err != nil {
		return PaymentConfig{}, err
	}
	if expirationInterval < 10*time.Second || expirationInterval > time.Hour {
		return PaymentConfig{}, errors.New("BABLO_PAYMENT_EXPIRATION_INTERVAL must be between 10s and 1h")
	}
	fixtureEnabled, err := boolEnv("BABLO_PAYMENT_FIXTURE_ENABLED", false)
	if err != nil {
		return PaymentConfig{}, err
	}
	stripeEnabled, err := boolEnv("BABLO_PAYMENT_STRIPE_ENABLED", false)
	if err != nil {
		return PaymentConfig{}, err
	}
	voucherEnabled, err := boolEnv("BABLO_PAYMENT_VOUCHER_ENABLED", false)
	if err != nil {
		return PaymentConfig{}, err
	}
	allowTestProviders, err := boolEnv("BABLO_PAYMENT_ALLOW_TEST_PROVIDERS", false)
	if err != nil {
		return PaymentConfig{}, err
	}
	environment = strings.ToLower(strings.TrimSpace(environment))
	switch environment {
	case "development", "test", "staging", "production":
	default:
		return PaymentConfig{}, fmt.Errorf("BABLO_ENV: unsupported environment %q", environment)
	}
	production := environment == "production"
	config := PaymentConfig{
		OrderTTL: orderTTL, WebhookTolerance: webhookTolerance,
		ExpirationInterval: expirationInterval, FixtureEnabled: fixtureEnabled,
		StripeEnabled: stripeEnabled, VoucherEnabled: voucherEnabled,
	}
	if fixtureEnabled {
		if production || !allowTestProviders {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_FIXTURE_ENABLED requires an explicit non-production test-provider opt-in")
		}
		config.FixtureMerchant = strings.TrimSpace(os.Getenv("BABLO_PAYMENT_FIXTURE_MERCHANT_ID"))
		if config.FixtureMerchant == "" || len(config.FixtureMerchant) > 160 {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_FIXTURE_MERCHANT_ID is required when the payment fixture is enabled")
		}
		secret, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("BABLO_PAYMENT_FIXTURE_SECRET")))
		if err != nil {
			return PaymentConfig{}, fmt.Errorf("BABLO_PAYMENT_FIXTURE_SECRET must be standard base64: %w", err)
		}
		if len(secret) < 32 {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_FIXTURE_SECRET must decode to at least 32 bytes")
		}
		config.FixtureSecret = secret
	}
	if voucherEnabled {
		config.VoucherCurrentVersion = strings.TrimSpace(envOr("BABLO_PAYMENT_VOUCHER_KEY_VERSION", "v1"))
		if !validPaymentKeyVersion(config.VoucherCurrentVersion) {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_VOUCHER_KEY_VERSION is invalid")
		}
		config.VoucherKeys, err = loadPaymentVoucherKeys(config.VoucherCurrentVersion)
		if err != nil {
			return PaymentConfig{}, err
		}
		if len(config.VoucherKeys) == 0 {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEY is required when vouchers are enabled")
		}
	}
	if stripeEnabled {
		if orderTTL <= 30*time.Minute {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_ORDER_TTL must be greater than 30m when Stripe is enabled")
		}
		config.StripeSecretKey = strings.TrimSpace(os.Getenv("BABLO_PAYMENT_STRIPE_SECRET_KEY"))
		stripeLive := strings.HasPrefix(config.StripeSecretKey, "sk_live_") || strings.HasPrefix(config.StripeSecretKey, "rk_live_")
		if !validStripeConfigKey(config.StripeSecretKey) || unsafePaymentPlaceholder(config.StripeSecretKey) || (production && !stripeLive) || (!production && stripeLive) || (!stripeLive && !allowTestProviders) {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_STRIPE_SECRET_KEY is invalid for the selected environment")
		}
		config.StripeWebhookSecret = strings.TrimSpace(os.Getenv("BABLO_PAYMENT_STRIPE_WEBHOOK_SECRET"))
		if !strings.HasPrefix(config.StripeWebhookSecret, "whsec_") || len(config.StripeWebhookSecret) < 16 || unsafePaymentPlaceholder(config.StripeWebhookSecret) {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_STRIPE_WEBHOOK_SECRET is required when Stripe is enabled")
		}
		config.StripeAccountID = strings.TrimSpace(os.Getenv("BABLO_PAYMENT_STRIPE_ACCOUNT_ID"))
		if !validPaymentIdentity(config.StripeAccountID, "acct_") || unsafePaymentPlaceholder(config.StripeAccountID) {
			return PaymentConfig{}, errors.New("BABLO_PAYMENT_STRIPE_ACCOUNT_ID is required when Stripe is enabled")
		}
		config.StripeSuccessURL, err = paymentReturnURL("BABLO_PAYMENT_STRIPE_SUCCESS_URL", production)
		if err != nil {
			return PaymentConfig{}, err
		}
		config.StripeCancelURL, err = paymentReturnURL("BABLO_PAYMENT_STRIPE_CANCEL_URL", production)
		if err != nil {
			return PaymentConfig{}, err
		}
	}
	return config, nil
}

func validStripeConfigKey(value string) bool {
	if len(value) < 16 {
		return false
	}
	return strings.HasPrefix(value, "sk_test_") || strings.HasPrefix(value, "sk_live_") || strings.HasPrefix(value, "rk_test_") || strings.HasPrefix(value, "rk_live_")
}

func loadPaymentVoucherKeys(current string) (map[string][]byte, error) {
	keys := make(map[string][]byte)
	if raw := strings.TrimSpace(os.Getenv("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEY")); raw != "" {
		key, err := decodePaymentVoucherKey(raw)
		if err != nil {
			return nil, err
		}
		keys[current] = key
	}
	if raw := strings.TrimSpace(os.Getenv("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEYS")); raw != "" {
		for _, item := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
			if len(parts) != 2 || !validPaymentKeyVersion(strings.TrimSpace(parts[0])) {
				return nil, errors.New("BABLO_PAYMENT_VOUCHER_ENCRYPTION_KEYS must use version=base64 entries")
			}
			version := strings.TrimSpace(parts[0])
			if _, exists := keys[version]; exists {
				return nil, fmt.Errorf("payment voucher key version %q is configured more than once", version)
			}
			key, err := decodePaymentVoucherKey(parts[1])
			if err != nil {
				return nil, err
			}
			keys[version] = key
		}
	}
	if len(keys) > 16 {
		return nil, errors.New("at most 16 payment voucher key versions may be configured")
	}
	if len(keys) > 0 {
		if _, ok := keys[current]; !ok {
			return nil, fmt.Errorf("current payment voucher key version %q is not configured", current)
		}
	}
	return keys, nil
}

func decodePaymentVoucherKey(value string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("payment voucher encryption key must be standard base64: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("payment voucher encryption key must decode to exactly 32 bytes")
	}
	return key, nil
}

func validPaymentKeyVersion(value string) bool {
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

func validPaymentIdentity(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) < len(prefix)+5 || len(value) > 160 {
		return false
	}
	for _, item := range value {
		if (item >= 'a' && item <= 'z') || (item >= 'A' && item <= 'Z') || (item >= '0' && item <= '9') || item == '_' || item == '-' {
			continue
		}
		return false
	}
	return true
}

func unsafePaymentPlaceholder(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{"change-me", "change_me", "changeme", "replace-me", "replace_me", "placeholder", ".example"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func paymentReturnURL(key string, production bool) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || len(value) > 2048 {
		return "", fmt.Errorf("%s must be an absolute http(s) URL", key)
	}
	if production && (parsed.Scheme != "https" || strings.EqualFold(parsed.Hostname(), "localhost") || unsafePaymentPlaceholder(value)) {
		return "", fmt.Errorf("%s must use a non-placeholder https URL in production", key)
	}
	return value, nil
}
