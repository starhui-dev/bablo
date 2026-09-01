package payment

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	stripewebhook "github.com/stripe/stripe-go/v86/webhook"
)

type stripeAPIFake struct {
	createCheckout        func(context.Context, *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error)
	retrieve              func(context.Context, string, *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error)
	expire                func(context.Context, string, *stripe.CheckoutSessionExpireParams) (*stripe.CheckoutSession, error)
	createRefund          func(context.Context, *stripe.RefundCreateParams) (*stripe.Refund, error)
	retrieveRefund        func(context.Context, string, *stripe.RefundRetrieveParams) (*stripe.Refund, error)
	retrievePaymentIntent func(context.Context, string, *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error)
	retrieveCharge        func(context.Context, string, *stripe.ChargeRetrieveParams) (*stripe.Charge, error)
}

func (f *stripeAPIFake) CreateCheckoutSession(ctx context.Context, params *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	if f.createCheckout == nil {
		return nil, errors.New("unexpected checkout create")
	}
	return f.createCheckout(ctx, params)
}

func (f *stripeAPIFake) RetrieveCheckoutSession(ctx context.Context, id string, params *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error) {
	if f.retrieve == nil {
		return nil, errors.New("unexpected checkout retrieve")
	}
	return f.retrieve(ctx, id, params)
}

func (f *stripeAPIFake) ExpireCheckoutSession(ctx context.Context, id string, params *stripe.CheckoutSessionExpireParams) (*stripe.CheckoutSession, error) {
	if f.expire == nil {
		return nil, errors.New("unexpected checkout expire")
	}
	return f.expire(ctx, id, params)
}

func (f *stripeAPIFake) CreateRefund(ctx context.Context, params *stripe.RefundCreateParams) (*stripe.Refund, error) {
	if f.createRefund == nil {
		return nil, errors.New("unexpected refund create")
	}
	return f.createRefund(ctx, params)
}

func (f *stripeAPIFake) RetrieveRefund(ctx context.Context, id string, params *stripe.RefundRetrieveParams) (*stripe.Refund, error) {
	if f.retrieveRefund == nil {
		return nil, errors.New("unexpected refund retrieve")
	}
	return f.retrieveRefund(ctx, id, params)
}

func (f *stripeAPIFake) RetrievePaymentIntent(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
	if f.retrievePaymentIntent == nil {
		return nil, errors.New("unexpected payment intent retrieve")
	}
	return f.retrievePaymentIntent(ctx, id, params)
}

func (f *stripeAPIFake) RetrieveCharge(ctx context.Context, id string, params *stripe.ChargeRetrieveParams) (*stripe.Charge, error) {
	if f.retrieveCharge == nil {
		return nil, errors.New("unexpected charge retrieve")
	}
	return f.retrieveCharge(ctx, id, params)
}
func TestStripeCreateOrderUsesStableCheckoutContract(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var captured *stripe.CheckoutSessionCreateParams
	api := &stripeAPIFake{createCheckout: func(_ context.Context, params *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
		captured = params
		return &stripe.CheckoutSession{
			ID: "cs_test_bablo", ClientReferenceID: "bablo_pay_checkout_test",
			Metadata:    map[string]string{stripeOrderMetadataKey: "bablo_pay_checkout_test"},
			AmountTotal: 1250, Currency: stripe.CurrencyUSD, ExpiresAt: expiresAt.Unix(),
			Status: stripe.CheckoutSessionStatusOpen, URL: "https://checkout.stripe.test/session",
		}, nil
	}}
	provider := newStripeProviderForTest(t, api)
	checkout, err := provider.CreateOrder(t.Context(), ProviderCreateInput{
		OrderNo: "bablo_pay_checkout_test", AmountMinor: 1250, Currency: "USD", ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if checkout.ProviderTradeNo != "cs_test_bablo" || checkout.Data["url"] == "" || checkout.ExpiresAt == nil || !checkout.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("CreateOrder() = %#v", checkout)
	}
	if captured == nil || captured.IdempotencyKey == nil || *captured.IdempotencyKey != "bablo-checkout:bablo_pay_checkout_test" {
		t.Fatalf("checkout idempotency = %#v", captured)
	}
	if captured.ClientReferenceID == nil || *captured.ClientReferenceID != "bablo_pay_checkout_test" || len(captured.LineItems) != 1 || captured.LineItems[0].PriceData == nil || captured.LineItems[0].PriceData.UnitAmount == nil || *captured.LineItems[0].PriceData.UnitAmount != 1250 {
		t.Fatalf("checkout params = %#v", captured)
	}
	if captured.LineItems[0].PriceData.Currency == nil || *captured.LineItems[0].PriceData.Currency != "usd" || captured.PaymentIntentData.Metadata[stripeOrderMetadataKey] != "bablo_pay_checkout_test" {
		t.Fatalf("checkout currency/metadata = %#v", captured)
	}
	if captured.StripeAccount == nil || *captured.StripeAccount != "acct_bablo_test" || checkout.MerchantID != "acct_bablo_test" || checkout.LiveMode {
		t.Fatalf("checkout account binding = (%#v, %#v)", captured.StripeAccount, checkout)
	}
}

func TestStripeRefundUsesPaidSessionAndStableIdempotency(t *testing.T) {
	var captured *stripe.RefundCreateParams
	api := &stripeAPIFake{
		retrieve: func(_ context.Context, id string, params *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error) {
			if id != "cs_test_bablo" {
				t.Fatalf("retrieve session id = %q", id)
			}
			if params.StripeAccount == nil || *params.StripeAccount != "acct_bablo_test" {
				t.Fatalf("retrieve Stripe account = %#v", params.StripeAccount)
			}
			return &stripe.CheckoutSession{
				ID: id, ClientReferenceID: "bablo_pay_refund_test",
				Metadata:    map[string]string{stripeOrderMetadataKey: "bablo_pay_refund_test"},
				AmountTotal: 900, Currency: stripe.CurrencyUSD,
				PaymentStatus: stripe.CheckoutSessionPaymentStatusPaid,
				PaymentIntent: &stripe.PaymentIntent{ID: "pi_test_bablo"},
			}, nil
		},
		createRefund: func(_ context.Context, params *stripe.RefundCreateParams) (*stripe.Refund, error) {
			captured = params
			return &stripe.Refund{
				ID: "re_test_bablo", Amount: 900, Currency: stripe.CurrencyUSD,
				Status: stripe.RefundStatusPending,
				Metadata: map[string]string{
					stripeOrderMetadataKey:           "bablo_pay_refund_test",
					stripeCheckoutSessionMetadataKey: "cs_test_bablo",
				},
			}, nil
		},
	}
	provider := newStripeProviderForTest(t, api)
	result, err := provider.Refund(t.Context(), ProviderRefundInput{
		OrderNo: "bablo_pay_refund_test", ProviderTradeNo: "cs_test_bablo",
		AmountMinor: 900, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("Refund() error = %v", err)
	}
	if result.ProviderRefundNo != "re_test_bablo" {
		t.Fatalf("Refund() = %#v", result)
	}
	if captured == nil || captured.IdempotencyKey == nil || *captured.IdempotencyKey != "bablo-refund:bablo_pay_refund_test" || captured.PaymentIntent == nil || *captured.PaymentIntent != "pi_test_bablo" || captured.Amount == nil || *captured.Amount != 900 {
		t.Fatalf("refund params = %#v", captured)
	}
	if captured.Metadata[stripeCheckoutSessionMetadataKey] != "cs_test_bablo" || captured.StripeAccount == nil || *captured.StripeAccount != "acct_bablo_test" {
		t.Fatalf("refund metadata/account = %#v", captured)
	}
}

func TestStripeWebhookVerifiesPaidAndRefundFailedEvents(t *testing.T) {
	provider := newStripeProviderForTest(t, &stripeAPIFake{})
	now := time.Now().UTC().Truncate(time.Second)
	paidBody := []byte(`{"id":"evt_checkout_paid","object":"event","api_version":"` + stripe.APIVersion + `","created":` + strconvItoa(int(now.Unix())) + `,"type":"checkout.session.completed","data":{"object":{"id":"cs_test_paid","object":"checkout.session","client_reference_id":"bablo_pay_webhook_test","metadata":{"bablo_order_no":"bablo_pay_webhook_test"},"amount_total":700,"currency":"usd","payment_status":"paid","status":"complete"}}}`)
	paid := verifyStripeTestWebhook(t, provider, paidBody, now)
	if paid.ProviderEventID != "evt_checkout_paid" || paid.OrderNo != "bablo_pay_webhook_test" || paid.ProviderTradeNo != "cs_test_paid" || paid.EventType != EventPaid || paid.AmountMinor != 700 || paid.Currency != "USD" || paid.MerchantID != "acct_bablo_test" || paid.LiveMode {
		t.Fatalf("paid event = %#v", paid)
	}

	failedBody := []byte(`{"id":"evt_refund_failed","object":"event","api_version":"` + stripe.APIVersion + `","created":` + strconvItoa(int(now.Unix())) + `,"type":"refund.failed","data":{"object":{"id":"re_test_failed","object":"refund","amount":700,"currency":"usd","status":"failed","metadata":{"bablo_order_no":"bablo_pay_webhook_test","bablo_checkout_session_id":"cs_test_paid"}}}}`)
	failed := verifyStripeTestWebhook(t, provider, failedBody, now)
	if failed.EventType != EventRefundFailed || failed.ProviderRefundNo != "re_test_failed" || failed.ProviderTradeNo != "cs_test_paid" {
		t.Fatalf("refund failed event = %#v", failed)
	}
}

func TestStripeWebhookResolvesExternalRefundAndDisputeOrderMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	const orderNo = "bablo_pay_recovery_test"
	api := &stripeAPIFake{
		retrievePaymentIntent: func(_ context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
			if id != "pi_recovery_test" || params.StripeAccount == nil || *params.StripeAccount != "acct_bablo_test" {
				t.Fatalf("payment intent lookup = (%q, %#v)", id, params.StripeAccount)
			}
			return &stripe.PaymentIntent{ID: id, Livemode: false, Metadata: map[string]string{stripeOrderMetadataKey: orderNo}}, nil
		},
		retrieveCharge: func(_ context.Context, id string, params *stripe.ChargeRetrieveParams) (*stripe.Charge, error) {
			if id != "ch_recovery_test" || params.StripeAccount == nil || *params.StripeAccount != "acct_bablo_test" {
				t.Fatalf("charge lookup = (%q, %#v)", id, params.StripeAccount)
			}
			return &stripe.Charge{ID: id, Livemode: false, Metadata: map[string]string{stripeOrderMetadataKey: orderNo}}, nil
		},
	}
	provider := newStripeProviderForTest(t, api)
	refundBody := []byte(`{"id":"evt_external_refund","object":"event","api_version":"` + stripe.APIVersion + `","created":` + strconvItoa(int(now.Unix())) + `,"type":"refund.created","data":{"object":{"id":"re_recovery_test","object":"refund","amount":300,"currency":"usd","status":"succeeded","payment_intent":"pi_recovery_test","charge":"ch_recovery_test","metadata":{}}}}`)
	refund := verifyStripeTestWebhook(t, provider, refundBody, now)
	if refund.OrderNo != orderNo || refund.EventType != EventRefunded || refund.ProviderRefundNo != "re_recovery_test" ||
		refund.ProviderPaymentIntentNo != "pi_recovery_test" || refund.ProviderChargeNo != "ch_recovery_test" {
		t.Fatalf("external refund event = %#v", refund)
	}

	disputeBody := []byte(`{"id":"evt_dispute_opened","object":"event","api_version":"` + stripe.APIVersion + `","created":` + strconvItoa(int(now.Unix())) + `,"type":"charge.dispute.created","data":{"object":{"id":"dp_recovery_test","object":"dispute","amount":500,"currency":"usd","livemode":false,"status":"needs_response","payment_intent":"pi_recovery_test","charge":"ch_recovery_test","metadata":{}}}}`)
	dispute := verifyStripeTestWebhook(t, provider, disputeBody, now)
	if dispute.OrderNo != orderNo || dispute.EventType != EventDisputeOpened || dispute.ProviderDisputeNo != "dp_recovery_test" ||
		dispute.ProviderPaymentIntentNo != "pi_recovery_test" || dispute.ProviderChargeNo != "ch_recovery_test" {
		t.Fatalf("dispute event = %#v", dispute)
	}
}

func TestStripeWebhookRejectsWrongAccountOrMode(t *testing.T) {
	provider := newStripeProviderForTest(t, &stripeAPIFake{})
	now := time.Now().UTC().Truncate(time.Second)
	for name, identity := range map[string]string{
		"account": `"account":"acct_other",`,
		"mode":    `"livemode":true,`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"id":"evt_wrong_` + name + `","object":"event",` + identity + `"api_version":"` + stripe.APIVersion + `","created":` + strconvItoa(int(now.Unix())) + `,"type":"checkout.session.completed","data":{"object":{"id":"cs_test_paid","object":"checkout.session","client_reference_id":"bablo_pay_webhook_test","metadata":{"bablo_order_no":"bablo_pay_webhook_test"},"amount_total":700,"currency":"usd","payment_status":"paid","status":"complete"}}}`)
			signed := stripewebhook.GenerateTestSignedPayload(&stripewebhook.UnsignedPayload{Payload: body, Secret: provider.webhookSecret, Timestamp: now})
			_, err := provider.VerifyWebhook(t.Context(), SignedWebhook{
				Headers: map[string][]string{stripeSignatureHeader: {signed.Header}}, Body: body, ReceivedAt: now,
			})
			if !errors.Is(err, ErrWebhookInvalid) {
				t.Fatalf("VerifyWebhook() error = %v", err)
			}
		})
	}
}

func TestStripeWebhookRejectsBadSignatureAndAcknowledgesDurableRejection(t *testing.T) {
	provider := newStripeProviderForTest(t, &stripeAPIFake{})
	_, err := provider.VerifyWebhook(t.Context(), SignedWebhook{
		Headers: map[string][]string{stripeSignatureHeader: {"t=1,v1=bad"}}, Body: []byte(`{"id":"evt_bad"}`),
	})
	if !errors.Is(err, ErrWebhookInvalid) {
		t.Fatalf("VerifyWebhook() error = %v", err)
	}
	if response := provider.WebhookResponse(ErrWebhookMismatch); response.StatusCode != http.StatusOK {
		t.Fatalf("durable mismatch response = %#v", response)
	}
	if response := provider.WebhookResponse(errors.New("database unavailable")); response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("transient failure response = %#v", response)
	}
}

func TestStripeCloseLeavesCompletedSessionForWebhook(t *testing.T) {
	api := &stripeAPIFake{retrieve: func(_ context.Context, _ string, _ *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error) {
		return &stripe.CheckoutSession{
			ID: "cs_test_complete", ClientReferenceID: "bablo_pay_complete_test",
			Metadata: map[string]string{stripeOrderMetadataKey: "bablo_pay_complete_test"},
			Status:   stripe.CheckoutSessionStatusComplete,
		}, nil
	}}
	provider := newStripeProviderForTest(t, api)
	if err := provider.Close(t.Context(), ProviderCloseInput{OrderNo: "bablo_pay_complete_test", ProviderTradeNo: "cs_test_complete"}); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("Close() error = %v", err)
	}
}

func newStripeProviderForTest(t *testing.T, api stripeAPI) *StripeProvider {
	t.Helper()
	provider, err := newStripeProvider(StripeProviderConfig{
		SecretKey: "sk_test_bablo_payment", WebhookSecret: "whsec_bablo_payment",
		SuccessURL: "https://console.example/billing/success", CancelURL: "https://console.example/billing",
		AccountID: "acct_bablo_test", Tolerance: 5 * time.Minute,
	}, api)
	if err != nil {
		t.Fatalf("newStripeProvider() error = %v", err)
	}
	return provider
}

func verifyStripeTestWebhook(t *testing.T, provider *StripeProvider, body []byte, at time.Time) VerifiedEvent {
	t.Helper()
	signed := stripewebhook.GenerateTestSignedPayload(&stripewebhook.UnsignedPayload{
		Payload: body, Secret: provider.webhookSecret, Timestamp: at,
	})
	event, err := provider.VerifyWebhook(t.Context(), SignedWebhook{
		Headers: map[string][]string{stripeSignatureHeader: {signed.Header}}, Body: body, ReceivedAt: at,
	})
	if err != nil {
		t.Fatalf("VerifyWebhook() error = %v", err)
	}
	return event
}
