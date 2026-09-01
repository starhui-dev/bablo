package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	stripewebhook "github.com/stripe/stripe-go/v86/webhook"
)

const (
	stripeProviderName               = "stripe"
	stripeSignatureHeader            = "Stripe-Signature"
	stripeOrderMetadataKey           = "bablo_order_no"
	stripeCheckoutSessionMetadataKey = "bablo_checkout_session_id"
)

// StripeProviderConfig contains only Stripe secrets and browser return URLs.
// Browser redirects never settle an order; only a verified Stripe event can do so.
type StripeProviderConfig struct {
	SecretKey     string
	WebhookSecret string
	SuccessURL    string
	CancelURL     string
	AccountID     string
	Tolerance     time.Duration
}

type stripeAPI interface {
	CreateCheckoutSession(context.Context, *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error)
	RetrieveCheckoutSession(context.Context, string, *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error)
	ExpireCheckoutSession(context.Context, string, *stripe.CheckoutSessionExpireParams) (*stripe.CheckoutSession, error)
	CreateRefund(context.Context, *stripe.RefundCreateParams) (*stripe.Refund, error)
	RetrieveRefund(context.Context, string, *stripe.RefundRetrieveParams) (*stripe.Refund, error)
}

type stripeObjectLookupAPI interface {
	RetrievePaymentIntent(context.Context, string, *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error)
	RetrieveCharge(context.Context, string, *stripe.ChargeRetrieveParams) (*stripe.Charge, error)
}

type stripeSDKAPI struct {
	client *stripe.Client
}

func (a stripeSDKAPI) CreateCheckoutSession(ctx context.Context, params *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	return a.client.V1CheckoutSessions.Create(ctx, params)
}

func (a stripeSDKAPI) RetrieveCheckoutSession(ctx context.Context, id string, params *stripe.CheckoutSessionRetrieveParams) (*stripe.CheckoutSession, error) {
	return a.client.V1CheckoutSessions.Retrieve(ctx, id, params)
}

func (a stripeSDKAPI) ExpireCheckoutSession(ctx context.Context, id string, params *stripe.CheckoutSessionExpireParams) (*stripe.CheckoutSession, error) {
	return a.client.V1CheckoutSessions.Expire(ctx, id, params)
}

func (a stripeSDKAPI) CreateRefund(ctx context.Context, params *stripe.RefundCreateParams) (*stripe.Refund, error) {
	return a.client.V1Refunds.Create(ctx, params)
}

func (a stripeSDKAPI) RetrieveRefund(ctx context.Context, id string, params *stripe.RefundRetrieveParams) (*stripe.Refund, error) {
	return a.client.V1Refunds.Retrieve(ctx, id, params)
}

func (a stripeSDKAPI) RetrievePaymentIntent(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
	return a.client.V1PaymentIntents.Retrieve(ctx, id, params)
}

func (a stripeSDKAPI) RetrieveCharge(ctx context.Context, id string, params *stripe.ChargeRetrieveParams) (*stripe.Charge, error) {
	return a.client.V1Charges.Retrieve(ctx, id, params)
}

// StripeProvider adapts Stripe Checkout and Refunds without exposing Stripe
// types outside internal/payment.
type StripeProvider struct {
	api           stripeAPI
	webhookSecret string
	successURL    string
	cancelURL     string
	accountID     string
	liveMode      bool
	tolerance     time.Duration
}

func NewStripeProvider(config StripeProviderConfig) (*StripeProvider, error) {
	secretKey := strings.TrimSpace(config.SecretKey)
	if !validStripeSecretKey(secretKey) {
		return nil, ErrInvalidInput
	}
	return newStripeProvider(config, stripeSDKAPI{client: stripe.NewClient(secretKey)})
}

func newStripeProvider(config StripeProviderConfig, api stripeAPI) (*StripeProvider, error) {
	config.SecretKey = strings.TrimSpace(config.SecretKey)
	config.WebhookSecret = strings.TrimSpace(config.WebhookSecret)
	config.SuccessURL = strings.TrimSpace(config.SuccessURL)
	config.CancelURL = strings.TrimSpace(config.CancelURL)
	config.AccountID = strings.TrimSpace(config.AccountID)
	if api == nil || !validStripeSecretKey(config.SecretKey) || !strings.HasPrefix(config.WebhookSecret, "whsec_") || len(config.WebhookSecret) < 16 || !validStripeReturnURL(config.SuccessURL) || !validStripeReturnURL(config.CancelURL) || !validStripeAccountID(config.AccountID) {
		return nil, ErrInvalidInput
	}
	if config.Tolerance == 0 {
		config.Tolerance = stripewebhook.DefaultTolerance
	}
	if config.Tolerance < time.Second || config.Tolerance > time.Hour {
		return nil, ErrInvalidInput
	}
	liveMode := strings.HasPrefix(config.SecretKey, "sk_live_") || strings.HasPrefix(config.SecretKey, "rk_live_")
	return &StripeProvider{
		api:           api,
		webhookSecret: config.WebhookSecret,
		successURL:    config.SuccessURL,
		cancelURL:     config.CancelURL,
		accountID:     config.AccountID,
		liveMode:      liveMode,
		tolerance:     config.Tolerance,
	}, nil
}

func (p *StripeProvider) Identity() ProviderIdentity {
	if p == nil {
		return ProviderIdentity{}
	}
	return ProviderIdentity{MerchantID: p.merchantIdentity(), LiveMode: p.liveMode}
}
func (p *StripeProvider) Name() string { return stripeProviderName }

func (p *StripeProvider) CreateOrder(ctx context.Context, input ProviderCreateInput) (Checkout, error) {
	orderNo := normalizeOrderNo(input.OrderNo)
	currency := normalizeCurrency(input.Currency)
	if p == nil || p.api == nil || orderNo == "" || input.AmountMinor <= 0 || currency == "" || input.ExpiresAt.IsZero() {
		return Checkout{}, ErrInvalidInput
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL:        stripe.String(p.successURL),
		CancelURL:         stripe.String(p.cancelURL),
		ClientReferenceID: stripe.String(orderNo),
		ExpiresAt:         stripe.Int64(input.ExpiresAt.UTC().Unix()),
		Metadata: map[string]string{
			stripeOrderMetadataKey: orderNo,
		},
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Metadata: map[string]string{stripeOrderMetadataKey: orderNo},
		},
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:   stripe.String(strings.ToLower(currency)),
				UnitAmount: stripe.Int64(input.AmountMinor),
				ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name:     stripe.String("Bablo wallet credit"),
					Metadata: map[string]string{stripeOrderMetadataKey: orderNo},
				},
			},
		}},
	}
	params.SetIdempotencyKey("bablo-checkout:" + orderNo)
	p.setStripeAccount(&params.Params)
	session, err := p.api.CreateCheckoutSession(ctx, params)
	if err != nil {
		return Checkout{}, mapStripeProviderError(err)
	}
	if !p.validCreatedStripeSession(session, orderNo, input.AmountMinor, currency) {
		return Checkout{}, ErrProviderUnavailable
	}
	expiresAt := time.Unix(session.ExpiresAt, 0).UTC()
	data := map[string]string{"mode": "redirect"}
	if session.URL != "" {
		data["url"] = session.URL
	}
	return Checkout{
		ProviderTradeNo: session.ID, MerchantID: p.merchantIdentity(),
		LiveMode: p.liveMode, Data: data, ExpiresAt: &expiresAt,
	}, nil
}

func (p *StripeProvider) Refund(ctx context.Context, input ProviderRefundInput) (ProviderRefundResult, error) {
	orderNo := normalizeOrderNo(input.OrderNo)
	checkoutSessionID := normalizeTradeNo(input.ProviderTradeNo)
	currency := normalizeCurrency(input.Currency)
	if p == nil || p.api == nil || orderNo == "" || checkoutSessionID == "" || input.AmountMinor <= 0 || currency == "" {
		return ProviderRefundResult{}, ErrInvalidInput
	}
	sessionParams := &stripe.CheckoutSessionRetrieveParams{}
	p.setStripeAccount(&sessionParams.Params)
	session, err := p.api.RetrieveCheckoutSession(ctx, checkoutSessionID, sessionParams)
	if err != nil {
		return ProviderRefundResult{}, mapStripeProviderError(err)
	}
	if !p.validStripeSession(session, orderNo, input.AmountMinor, currency) || session.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid || session.PaymentIntent == nil || normalizeTradeNo(session.PaymentIntent.ID) == "" {
		return ProviderRefundResult{}, ErrProviderRejected
	}
	params := &stripe.RefundCreateParams{
		Amount:        stripe.Int64(input.AmountMinor),
		PaymentIntent: stripe.String(session.PaymentIntent.ID),
		Reason:        stripe.String(string(stripe.RefundReasonRequestedByCustomer)),
		Metadata: map[string]string{
			stripeOrderMetadataKey:           orderNo,
			stripeCheckoutSessionMetadataKey: checkoutSessionID,
		},
	}
	params.SetIdempotencyKey("bablo-refund:" + orderNo)
	p.setStripeAccount(&params.Params)
	refund, err := p.api.CreateRefund(ctx, params)
	if err != nil {
		return ProviderRefundResult{}, mapStripeProviderError(err)
	}
	if !p.validStripeRefund(refund, orderNo, checkoutSessionID, input.AmountMinor, currency) {
		return ProviderRefundResult{}, ErrProviderUnavailable
	}
	return ProviderRefundResult{ProviderRefundNo: refund.ID}, nil
}

func (p *StripeProvider) Close(ctx context.Context, input ProviderCloseInput) error {
	orderNo := normalizeOrderNo(input.OrderNo)
	checkoutSessionID := normalizeTradeNo(input.ProviderTradeNo)
	if p == nil || p.api == nil || orderNo == "" {
		return ErrInvalidInput
	}
	if checkoutSessionID == "" {
		return ErrProviderUnavailable
	}
	params := &stripe.CheckoutSessionRetrieveParams{}
	p.setStripeAccount(&params.Params)
	session, err := p.api.RetrieveCheckoutSession(ctx, checkoutSessionID, params)
	if err != nil {
		return mapStripeProviderError(err)
	}
	if session == nil || session.ID != checkoutSessionID || stripeSessionOrderNo(session) != orderNo || session.Livemode != p.liveMode {
		return ErrProviderRejected
	}
	switch session.Status {
	case stripe.CheckoutSessionStatusExpired:
		return nil
	case stripe.CheckoutSessionStatusComplete:
		return ErrOperationPending
	case stripe.CheckoutSessionStatusOpen:
	default:
		return ErrProviderRejected
	}
	expireParams := &stripe.CheckoutSessionExpireParams{}
	p.setStripeAccount(&expireParams.Params)
	expired, err := p.api.ExpireCheckoutSession(ctx, checkoutSessionID, expireParams)
	if err == nil {
		if expired != nil && expired.ID == checkoutSessionID && expired.Status == stripe.CheckoutSessionStatusExpired && expired.Livemode == p.liveMode {
			return nil
		}
		return ErrProviderUnavailable
	}
	reloaded, reloadErr := p.api.RetrieveCheckoutSession(ctx, checkoutSessionID, params)
	if reloadErr == nil && reloaded != nil && reloaded.Status == stripe.CheckoutSessionStatusExpired && reloaded.Livemode == p.liveMode {
		return nil
	}
	return mapStripeProviderError(err)
}

func (p *StripeProvider) ReconcilePayment(ctx context.Context, input ProviderPaymentStateInput) (ProviderState, error) {
	orderNo := normalizeOrderNo(input.OrderNo)
	tradeNo := normalizeTradeNo(input.ProviderTradeNo)
	currency := normalizeCurrency(input.Currency)
	if p == nil || p.api == nil || orderNo == "" || tradeNo == "" || input.AmountMinor <= 0 || currency == "" || input.ObservedAt.IsZero() {
		return ProviderState{}, ErrInvalidInput
	}
	params := &stripe.CheckoutSessionRetrieveParams{}
	p.setStripeAccount(&params.Params)
	session, err := p.api.RetrieveCheckoutSession(ctx, tradeNo, params)
	if err != nil {
		return ProviderState{}, mapStripeProviderError(err)
	}
	if !p.validStripeSession(session, orderNo, input.AmountMinor, currency) {
		return ProviderState{}, ErrProviderRejected
	}
	eventType := EventPending
	occurredAt := input.ObservedAt.UTC()
	switch {
	case session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid:
		eventType = EventPaid
		if session.Created > 0 {
			occurredAt = time.Unix(session.Created, 0).UTC()
		}
	case session.Status == stripe.CheckoutSessionStatusExpired:
		eventType = EventExpired
		if session.ExpiresAt > 0 {
			occurredAt = time.Unix(session.ExpiresAt, 0).UTC()
		}
	case session.Status == stripe.CheckoutSessionStatusComplete:
		eventType = EventPending
	case session.Status == stripe.CheckoutSessionStatusOpen:
		eventType = EventPending
	default:
		return ProviderState{}, ErrProviderRejected
	}
	return ProviderState{
		MerchantID: p.merchantIdentity(), LiveMode: p.liveMode,
		ProviderTradeNo: tradeNo, EventType: eventType,
		AmountMinor: input.AmountMinor, Currency: currency,
		OccurredAt: occurredAt,
	}, nil
}

func (p *StripeProvider) ReconcileRefund(ctx context.Context, input ProviderRefundStateInput) (ProviderState, error) {
	orderNo := normalizeOrderNo(input.OrderNo)
	tradeNo := normalizeTradeNo(input.ProviderTradeNo)
	refundNo := normalizeTradeNo(input.ProviderRefundNo)
	currency := normalizeCurrency(input.Currency)
	if p == nil || p.api == nil || orderNo == "" || tradeNo == "" || refundNo == "" || input.AmountMinor <= 0 || currency == "" || input.ObservedAt.IsZero() {
		return ProviderState{}, ErrInvalidInput
	}
	params := &stripe.RefundRetrieveParams{}
	p.setStripeAccount(&params.Params)
	refund, err := p.api.RetrieveRefund(ctx, refundNo, params)
	if err != nil {
		return ProviderState{}, mapStripeProviderError(err)
	}
	if !p.validStripeRefund(refund, orderNo, tradeNo, input.AmountMinor, currency) {
		return ProviderState{}, ErrProviderRejected
	}
	eventType := EventPending
	switch refund.Status {
	case stripe.RefundStatusSucceeded:
		eventType = EventRefunded
	case stripe.RefundStatusFailed, stripe.RefundStatusCanceled:
		eventType = EventRefundFailed
	case stripe.RefundStatusPending, stripe.RefundStatusRequiresAction:
		eventType = EventPending
	default:
		return ProviderState{}, ErrProviderRejected
	}
	occurredAt := input.ObservedAt.UTC()
	if refund.Created > 0 {
		occurredAt = time.Unix(refund.Created, 0).UTC()
	}
	return ProviderState{
		MerchantID: p.merchantIdentity(), LiveMode: p.liveMode,
		ProviderTradeNo: tradeNo, ProviderRefundNo: refundNo,
		EventType: eventType, AmountMinor: input.AmountMinor,
		Currency: currency, OccurredAt: occurredAt,
	}, nil
}

func (p *StripeProvider) VerifyWebhook(ctx context.Context, request SignedWebhook) (VerifiedEvent, error) {
	if p == nil || len(request.Body) == 0 {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	signature := firstHeader(request.Headers, stripeSignatureHeader)
	event, err := stripewebhook.ConstructEventWithOptions(request.Body, signature, p.webhookSecret, stripewebhook.ConstructEventOptions{Tolerance: p.tolerance})
	if err != nil {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	if normalizeEventID(event.ID) == "" || event.Created <= 0 || event.Data == nil || len(event.Data.Raw) == 0 || event.Livemode != p.liveMode {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	if account := normalizeTradeNo(event.Account); account != "" && account != p.accountID {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted,
		stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded,
		stripe.EventTypeCheckoutSessionAsyncPaymentFailed,
		stripe.EventTypeCheckoutSessionExpired:
		var session stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &session); err != nil || session.Livemode != p.liveMode {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		return verifiedStripeCheckoutEvent(event, session, p.accountID)
	case stripe.EventTypeRefundCreated, stripe.EventTypeRefundUpdated, stripe.EventTypeRefundFailed:
		var refund stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &refund); err != nil {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		verified, err := verifiedStripeRefundEvent(event, refund, p.accountID)
		if err != nil {
			return VerifiedEvent{}, err
		}
		return p.enrichStripeOrderReference(ctx, verified)
	case stripe.EventTypeChargeDisputeCreated, stripe.EventTypeChargeDisputeFundsWithdrawn,
		stripe.EventTypeChargeDisputeFundsReinstated, stripe.EventTypeChargeDisputeClosed:
		var dispute stripe.Dispute
		if err := json.Unmarshal(event.Data.Raw, &dispute); err != nil || dispute.Livemode != p.liveMode {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		verified, err := verifiedStripeDisputeEvent(event, dispute, p.accountID)
		if err != nil {
			return VerifiedEvent{}, err
		}
		return p.enrichStripeOrderReference(ctx, verified)
	default:
		return VerifiedEvent{}, ErrWebhookIgnored
	}
}
func (p *StripeProvider) enrichStripeOrderReference(ctx context.Context, event VerifiedEvent) (VerifiedEvent, error) {
	if event.OrderNo != "" {
		return event, nil
	}
	lookup, ok := p.api.(stripeObjectLookupAPI)
	if !ok {
		return VerifiedEvent{}, ErrWebhookIgnored
	}
	orderNo := ""
	if event.ProviderPaymentIntentNo != "" {
		params := &stripe.PaymentIntentRetrieveParams{}
		p.setStripeAccount(&params.Params)
		intent, err := lookup.RetrievePaymentIntent(ctx, event.ProviderPaymentIntentNo, params)
		if err != nil {
			return VerifiedEvent{}, mapStripeProviderError(err)
		}
		if intent == nil || intent.Livemode != p.liveMode {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		orderNo = normalizeOrderNo(intent.Metadata[stripeOrderMetadataKey])
	}
	if event.ProviderChargeNo != "" {
		params := &stripe.ChargeRetrieveParams{}
		p.setStripeAccount(&params.Params)
		charge, err := lookup.RetrieveCharge(ctx, event.ProviderChargeNo, params)
		if err != nil {
			return VerifiedEvent{}, mapStripeProviderError(err)
		}
		if charge == nil || charge.Livemode != p.liveMode {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		chargeOrderNo := normalizeOrderNo(charge.Metadata[stripeOrderMetadataKey])
		if orderNo != "" && chargeOrderNo != "" && orderNo != chargeOrderNo {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		if orderNo == "" {
			orderNo = chargeOrderNo
		}
	}
	if orderNo == "" {
		return VerifiedEvent{}, ErrWebhookIgnored
	}
	event.OrderNo = orderNo
	return event, nil
}

func (p *StripeProvider) WebhookResponse(err error) WebhookResponse {
	switch {
	case err == nil || errors.Is(err, ErrWebhookIgnored):
		return WebhookResponse{StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"received":true}`)}
	case errors.Is(err, ErrWebhookInvalid), errors.Is(err, ErrWebhookReplay):
		return WebhookResponse{StatusCode: http.StatusBadRequest, ContentType: "application/json", Body: []byte(`{"received":false}`)}
	case errors.Is(err, ErrWebhookMismatch), errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrConflict), errors.Is(err, ErrInsufficientFunds):
		return WebhookResponse{StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"received":true,"accepted":false}`)}
	default:
		return WebhookResponse{StatusCode: http.StatusInternalServerError, ContentType: "application/json", Body: []byte(`{"received":false}`)}
	}
}

func verifiedStripeCheckoutEvent(event stripe.Event, session stripe.CheckoutSession, merchantID string) (VerifiedEvent, error) {
	orderNo := stripeSessionOrderNo(&session)
	currency := normalizeCurrency(string(session.Currency))
	if orderNo == "" || normalizeTradeNo(session.ID) == "" || session.AmountTotal <= 0 || currency == "" {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	eventType := ""
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		switch session.PaymentStatus {
		case stripe.CheckoutSessionPaymentStatusPaid:
			eventType = EventPaid
		case stripe.CheckoutSessionPaymentStatusUnpaid:
			eventType = EventPending
		default:
			return VerifiedEvent{}, ErrWebhookInvalid
		}
	case stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded:
		if session.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		eventType = EventPaid
	case stripe.EventTypeCheckoutSessionAsyncPaymentFailed:
		eventType = EventFailed
	case stripe.EventTypeCheckoutSessionExpired:
		eventType = EventExpired
	default:
		return VerifiedEvent{}, ErrWebhookIgnored
	}
	paymentIntentID := ""
	if session.PaymentIntent != nil {
		paymentIntentID = normalizeTradeNo(session.PaymentIntent.ID)
	}
	return VerifiedEvent{
		ProviderEventID: event.ID, MerchantID: merchantID, LiveMode: event.Livemode,
		OrderNo: orderNo, ProviderTradeNo: session.ID,
		ProviderPaymentIntentNo: paymentIntentID,
		EventType:               eventType, AmountMinor: session.AmountTotal, Currency: currency,
		OccurredAt: time.Unix(event.Created, 0).UTC(),
	}, nil
}

func verifiedStripeRefundEvent(event stripe.Event, refund stripe.Refund, merchantID string) (VerifiedEvent, error) {
	orderNo := normalizeOrderNo(refund.Metadata[stripeOrderMetadataKey])
	checkoutSessionID := normalizeTradeNo(refund.Metadata[stripeCheckoutSessionMetadataKey])
	refundID := normalizeTradeNo(refund.ID)
	paymentIntentID := ""
	if refund.PaymentIntent != nil {
		paymentIntentID = normalizeTradeNo(refund.PaymentIntent.ID)
	}
	chargeID := ""
	if refund.Charge != nil {
		chargeID = normalizeTradeNo(refund.Charge.ID)
	}
	providerTradeNo := checkoutSessionID
	if providerTradeNo == "" {
		providerTradeNo = paymentIntentID
	}
	if providerTradeNo == "" {
		providerTradeNo = chargeID
	}
	currency := normalizeCurrency(string(refund.Currency))
	if refundID == "" || providerTradeNo == "" || (orderNo == "" && paymentIntentID == "" && chargeID == "") || refund.Amount <= 0 || currency == "" {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	eventType := ""
	if event.Type == stripe.EventTypeRefundFailed {
		if refund.Status != stripe.RefundStatusFailed && refund.Status != stripe.RefundStatusCanceled {
			return VerifiedEvent{}, ErrWebhookInvalid
		}
		eventType = EventRefundFailed
	} else {
		switch refund.Status {
		case stripe.RefundStatusSucceeded:
			eventType = EventRefunded
		case stripe.RefundStatusFailed, stripe.RefundStatusCanceled:
			eventType = EventRefundFailed
		case stripe.RefundStatusPending, stripe.RefundStatusRequiresAction:
			eventType = EventPending
		default:
			return VerifiedEvent{}, ErrWebhookInvalid
		}
	}
	return VerifiedEvent{
		ProviderEventID: event.ID, MerchantID: merchantID, LiveMode: event.Livemode,
		OrderNo: orderNo, ProviderTradeNo: providerTradeNo, ProviderRefundNo: refundID,
		ProviderPaymentIntentNo: paymentIntentID, ProviderChargeNo: chargeID,
		EventType: eventType, AmountMinor: refund.Amount, Currency: currency,
		OccurredAt: time.Unix(event.Created, 0).UTC(),
	}, nil
}

func verifiedStripeDisputeEvent(event stripe.Event, dispute stripe.Dispute, merchantID string) (VerifiedEvent, error) {
	disputeID := normalizeTradeNo(dispute.ID)
	chargeID := ""
	orderNo := normalizeOrderNo(dispute.Metadata[stripeOrderMetadataKey])
	if dispute.Charge != nil {
		chargeID = normalizeTradeNo(dispute.Charge.ID)
		if orderNo == "" {
			orderNo = normalizeOrderNo(dispute.Charge.Metadata[stripeOrderMetadataKey])
		}
	}
	paymentIntentID := ""
	if dispute.PaymentIntent != nil {
		paymentIntentID = normalizeTradeNo(dispute.PaymentIntent.ID)
		if orderNo == "" {
			orderNo = normalizeOrderNo(dispute.PaymentIntent.Metadata[stripeOrderMetadataKey])
		}
	}
	currency := normalizeCurrency(string(dispute.Currency))
	if disputeID == "" || chargeID == "" || dispute.Amount <= 0 || currency == "" {
		return VerifiedEvent{}, ErrWebhookInvalid
	}
	eventType := ""
	switch event.Type {
	case stripe.EventTypeChargeDisputeCreated, stripe.EventTypeChargeDisputeFundsWithdrawn:
		eventType = EventDisputeOpened
	case stripe.EventTypeChargeDisputeFundsReinstated:
		eventType = EventDisputeWon
	case stripe.EventTypeChargeDisputeClosed:
		switch dispute.Status {
		case stripe.DisputeStatusWon, stripe.DisputeStatusPrevented, stripe.DisputeStatusWarningClosed:
			eventType = EventDisputeWon
		case stripe.DisputeStatusLost:
			eventType = EventDisputeLost
		default:
			return VerifiedEvent{}, ErrWebhookIgnored
		}
	default:
		return VerifiedEvent{}, ErrWebhookIgnored
	}
	return VerifiedEvent{
		ProviderEventID: event.ID, MerchantID: merchantID, LiveMode: event.Livemode,
		OrderNo: orderNo, ProviderTradeNo: chargeID,
		ProviderPaymentIntentNo: paymentIntentID, ProviderChargeNo: chargeID,
		ProviderDisputeNo: disputeID, EventType: eventType,
		AmountMinor: dispute.Amount, Currency: currency,
		OccurredAt: time.Unix(event.Created, 0).UTC(),
	}, nil
}

func stripeSessionOrderNo(session *stripe.CheckoutSession) string {
	if session == nil {
		return ""
	}
	clientReference := normalizeOrderNo(session.ClientReferenceID)
	metadataOrder := normalizeOrderNo(session.Metadata[stripeOrderMetadataKey])
	if clientReference != "" && metadataOrder != "" && clientReference != metadataOrder {
		return ""
	}
	if clientReference != "" {
		return clientReference
	}
	return metadataOrder
}

func stripeEventMerchant(event stripe.Event) string {
	if account := normalizeTradeNo(event.Account); account != "" {
		return account
	}
	return stripeProviderName
}

func (p *StripeProvider) validCreatedStripeSession(session *stripe.CheckoutSession, orderNo string, amountMinor int64, currency string) bool {
	if !p.validStripeSession(session, orderNo, amountMinor, currency) || session.ExpiresAt <= 0 {
		return false
	}
	switch session.Status {
	case stripe.CheckoutSessionStatusOpen:
		return session.URL != ""
	case stripe.CheckoutSessionStatusComplete, stripe.CheckoutSessionStatusExpired:
		return true
	default:
		return false
	}
}

func validStripeAccountID(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "acct_") && len(value) >= 10 && len(value) <= 160 && !strings.ContainsAny(value, "\r\n\x00")
}

func (p *StripeProvider) setStripeAccount(params *stripe.Params) {
	params.SetStripeAccount(p.accountID)
}

func (p *StripeProvider) merchantIdentity() string {
	if p == nil {
		return ""
	}
	return p.accountID
}

func (p *StripeProvider) validStripeSession(session *stripe.CheckoutSession, orderNo string, amountMinor int64, currency string) bool {
	return p != nil && session != nil && normalizeTradeNo(session.ID) != "" &&
		stripeSessionOrderNo(session) == orderNo && session.AmountTotal == amountMinor &&
		normalizeCurrency(string(session.Currency)) == currency && session.Livemode == p.liveMode
}

func (p *StripeProvider) validStripeRefund(refund *stripe.Refund, orderNo, checkoutSessionID string, amountMinor int64, currency string) bool {
	return p != nil && refund != nil && normalizeTradeNo(refund.ID) != "" &&
		normalizeOrderNo(refund.Metadata[stripeOrderMetadataKey]) == orderNo &&
		normalizeTradeNo(refund.Metadata[stripeCheckoutSessionMetadataKey]) == checkoutSessionID &&
		refund.Amount == amountMinor && normalizeCurrency(string(refund.Currency)) == currency
}

func validStripeSecretKey(value string) bool {
	return len(value) >= 16 && (strings.HasPrefix(value, "sk_") || strings.HasPrefix(value, "rk_"))
}

func validStripeReturnURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil
}

func mapStripeProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrProviderUnavailable
	}
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		switch stripeErr.HTTPStatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
			return ErrProviderRejected
		default:
			return ErrProviderUnavailable
		}
	}
	return ErrProviderUnavailable
}
