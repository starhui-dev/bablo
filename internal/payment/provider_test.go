package payment

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestFixtureProviderVerifyWebhook(t *testing.T) {
	now := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	provider, err := NewFixtureProvider(FixtureProviderConfig{
		MerchantID: "merchant-test",
		Secret:     []byte("0123456789abcdef0123456789abcdef"),
		Tolerance:  5 * time.Minute,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewFixtureProvider() error = %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"event_id": "evt_1", "merchant_id": "merchant-test",
		"order_no": "bablo_pay_01999999999999999999999999",
		"trade_no": "trade_1", "status": EventPaid,
		"amount_minor": int64(1200), "currency": "USD", "occurred_at": now,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	headers, err := provider.SignWebhook(body, now)
	if err != nil {
		t.Fatalf("SignWebhook() error = %v", err)
	}
	verified, err := provider.VerifyWebhook(t.Context(), SignedWebhook{Headers: headers, Body: body, ReceivedAt: now})
	if err != nil {
		t.Fatalf("VerifyWebhook() error = %v", err)
	}
	if verified.ProviderEventID != "evt_1" || verified.AmountMinor != 1200 || verified.Currency != "USD" {
		t.Fatalf("VerifyWebhook() = %#v", verified)
	}

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] ^= 1
	if _, err := provider.VerifyWebhook(t.Context(), SignedWebhook{Headers: headers, Body: tampered, ReceivedAt: now}); !errors.Is(err, ErrWebhookInvalid) {
		t.Fatalf("tampered VerifyWebhook() error = %v, want ErrWebhookInvalid", err)
	}
	if _, err := provider.VerifyWebhook(t.Context(), SignedWebhook{Headers: headers, Body: body, ReceivedAt: now.Add(6 * time.Minute)}); !errors.Is(err, ErrWebhookInvalid) {
		t.Fatalf("stale VerifyWebhook() error = %v, want ErrWebhookInvalid", err)
	}

	trailing := append(append([]byte(nil), body...), []byte(` {}`)...)
	trailingHeaders, err := provider.SignWebhook(trailing, now)
	if err != nil {
		t.Fatalf("SignWebhook(trailing) error = %v", err)
	}
	if _, err := provider.VerifyWebhook(t.Context(), SignedWebhook{Headers: trailingHeaders, Body: trailing, ReceivedAt: now}); !errors.Is(err, ErrWebhookInvalid) {
		t.Fatalf("trailing VerifyWebhook() error = %v, want ErrWebhookInvalid", err)
	}
}

func TestFixtureProviderWebhookResponse(t *testing.T) {
	provider, err := NewFixtureProvider(FixtureProviderConfig{
		MerchantID: "merchant-test",
		Secret:     []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("NewFixtureProvider() error = %v", err)
	}
	if response := provider.WebhookResponse(nil); response.StatusCode != 200 {
		t.Fatalf("success response status = %d", response.StatusCode)
	}
	if response := provider.WebhookResponse(ErrWebhookInvalid); response.StatusCode != 401 {
		t.Fatalf("invalid response status = %d", response.StatusCode)
	}
}
