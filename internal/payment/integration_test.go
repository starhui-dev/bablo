package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/config"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/httpapi"
	"github.com/starhui-dev/bablo/internal/secret"
	"github.com/starhui-dev/bablo/migrations"
)

type paymentFixture struct {
	store       *data.Store
	service     *Service
	billing     *billing.Service
	provider    *FixtureProvider
	voucherKeys *secret.Keyring
	user        uuid.UUID
	other       uuid.UUID
	admin       uuid.UUID
	now         time.Time
}

type closeRecordingProvider struct {
	*FixtureProvider
	createCalls atomic.Int32
	closeCalls  atomic.Int32
	createErr   error
	closeErr    error
}

func (p *closeRecordingProvider) CreateOrder(ctx context.Context, input ProviderCreateInput) (Checkout, error) {
	p.createCalls.Add(1)
	if p.createErr != nil {
		return Checkout{}, p.createErr
	}
	return p.FixtureProvider.CreateOrder(ctx, input)
}

func (p *closeRecordingProvider) Close(ctx context.Context, input ProviderCloseInput) error {
	p.closeCalls.Add(1)
	if p.closeErr != nil {
		return p.closeErr
	}
	return p.FixtureProvider.Close(ctx, input)
}

type blockingProvider struct {
	*FixtureProvider
	createCalls   atomic.Int32
	refundCalls   atomic.Int32
	createStarted chan struct{}
	createRelease chan struct{}
	refundStarted chan struct{}
	refundRelease chan struct{}
}

func (p *blockingProvider) CreateOrder(ctx context.Context, input ProviderCreateInput) (Checkout, error) {
	p.createCalls.Add(1)
	if p.createStarted != nil {
		p.createStarted <- struct{}{}
		select {
		case <-p.createRelease:
		case <-ctx.Done():
			return Checkout{}, ctx.Err()
		}
	}
	return p.FixtureProvider.CreateOrder(ctx, input)
}

func (p *blockingProvider) Refund(ctx context.Context, input ProviderRefundInput) (ProviderRefundResult, error) {
	p.refundCalls.Add(1)
	if p.refundStarted != nil {
		p.refundStarted <- struct{}{}
		select {
		case <-p.refundRelease:
		case <-ctx.Done():
			return ProviderRefundResult{}, ctx.Err()
		}
	}
	return p.FixtureProvider.Refund(ctx, input)
}

type reconciliationProvider struct {
	*FixtureProvider
	paymentState ProviderState
	refundState  ProviderState
}

func (p *reconciliationProvider) ReconcilePayment(context.Context, ProviderPaymentStateInput) (ProviderState, error) {
	return p.paymentState, nil
}

func (p *reconciliationProvider) ReconcileRefund(context.Context, ProviderRefundStateInput) (ProviderState, error) {
	return p.refundState, nil
}

type rotatingReconciliationProvider struct {
	*FixtureProvider
	mu        sync.Mutex
	failOrder string
	states    map[string]ProviderState
	calls     []string
}

func (p *rotatingReconciliationProvider) ReconcilePayment(_ context.Context, input ProviderPaymentStateInput) (ProviderState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, input.OrderNo)
	if input.OrderNo == p.failOrder {
		return ProviderState{}, ErrProviderUnavailable
	}
	state, ok := p.states[input.OrderNo]
	if !ok {
		return ProviderState{}, ErrProviderUnavailable
	}
	return state, nil
}

func (p *rotatingReconciliationProvider) ReconcileRefund(context.Context, ProviderRefundStateInput) (ProviderState, error) {
	return ProviderState{}, ErrProviderUnavailable
}
func openPaymentTestStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_payment_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create test schema: %v", err)
	}
	pool.Close()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	databaseURL := parsed.String()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		cleanupPool, err := pgxpool.New(cleanupCtx, baseURL)
		if err != nil {
			t.Errorf("open cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	if err := data.Migrate(ctx, databaseURL, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 24})
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func newPaymentFixture(t *testing.T) paymentFixture {
	t.Helper()
	store := openPaymentTestStore(t)
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	fixture := paymentFixture{
		store: store, user: uuid.New(), other: uuid.New(), admin: uuid.New(), now: now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for index, userID := range []uuid.UUID{fixture.user, fixture.other, fixture.admin} {
		if _, err := store.Queryer().Exec(ctx, `
			INSERT INTO users (id, email_normalized, password_hash, password_params_version)
			VALUES ($1, $2, 'payment-test-hash', 'test')`,
			userID, "payment-"+string(rune('a'+index))+"-"+userID.String()+"@example.test"); err != nil {
			t.Fatalf("insert payment user: %v", err)
		}
	}
	billingRepository, err := billing.NewRepository(store)
	if err != nil {
		t.Fatalf("billing.NewRepository() error = %v", err)
	}
	fixture.billing, err = billing.NewService(billingRepository, billing.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("billing.NewService() error = %v", err)
	}
	fixture.voucherKeys, err = secret.NewKeyring("v1", map[string][]byte{
		"v1": []byte("abcdef0123456789abcdef0123456789"),
	})
	if err != nil {
		t.Fatalf("secret.NewKeyring() error = %v", err)
	}
	fixture.provider, err = NewFixtureProvider(FixtureProviderConfig{
		MerchantID: "merchant-test", Secret: []byte("0123456789abcdef0123456789abcdef"),
		Tolerance: 5 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewFixtureProvider() error = %v", err)
	}
	registry, err := NewRegistry(fixture.provider)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	fixture.service, err = NewService(repository, fixture.billing, registry, Options{
		OrderTTL: 15 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return fixture
}

func (f paymentFixture) createOrder(t *testing.T, userID uuid.UUID, amount int64, key string) Order {
	t.Helper()
	result, err := f.service.CreateOrder(t.Context(), CreateOrderInput{
		UserID: userID, AmountMinor: amount, Currency: "USD",
		PaymentProvider: f.provider.Name(), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if result.Order.Status != StatusPending || result.Order.ProviderTradeNo == "" || result.Checkout.Data["url"] == "" {
		t.Fatalf("CreateOrder() = %#v", result)
	}
	return result.Order
}

func (f paymentFixture) webhook(t *testing.T, order Order, eventID, status string, amount int64) (WebhookResult, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event_id": eventID, "merchant_id": "merchant-test", "order_no": order.OrderNo,
		"trade_no": order.ProviderTradeNo, "status": status, "amount_minor": amount,
		"currency": order.Currency, "occurred_at": f.now,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	headers, err := f.provider.SignWebhook(body, f.now)
	if err != nil {
		t.Fatalf("SignWebhook() error = %v", err)
	}
	return f.service.HandleWebhook(t.Context(), f.provider.Name(), headers, body, "req-"+eventID, f.now)
}

func (f paymentFixture) recoveryWebhook(t *testing.T, order Order, eventID, status string, amount int64, refundNo, paymentIntentNo, chargeNo, disputeNo string) (WebhookResult, error) {
	t.Helper()
	tradeNo := order.ProviderTradeNo
	if chargeNo != "" {
		tradeNo = chargeNo
	} else if paymentIntentNo != "" {
		tradeNo = paymentIntentNo
	}
	body, err := json.Marshal(map[string]any{
		"event_id": eventID, "merchant_id": "merchant-test", "order_no": order.OrderNo,
		"trade_no": tradeNo, "refund_no": refundNo,
		"payment_intent_no": paymentIntentNo, "charge_no": chargeNo, "dispute_no": disputeNo,
		"status": status, "amount_minor": amount, "currency": order.Currency, "occurred_at": f.now,
	})
	if err != nil {
		t.Fatalf("json.Marshal() recovery webhook error = %v", err)
	}
	headers, err := f.provider.SignWebhook(body, f.now)
	if err != nil {
		t.Fatalf("SignWebhook() recovery error = %v", err)
	}
	return f.service.HandleWebhook(t.Context(), f.provider.Name(), headers, body, "req-"+eventID, f.now)
}

func TestPaymentWebhookCreditsExactlyOnce(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 1200, "create-order-idempotency")
	result, err := fixture.webhook(t, order, "evt-paid-once", EventPaid, order.AmountMinor)
	if err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	if result.Order == nil || result.Order.Status != StatusPaid || result.Replayed {
		t.Fatalf("first webhook result = %#v", result)
	}
	replayed, err := fixture.webhook(t, order, "evt-paid-once", EventPaid, order.AmountMinor)
	if err != nil {
		t.Fatalf("replayed HandleWebhook() error = %v", err)
	}
	if !replayed.Replayed || replayed.Order == nil || replayed.Order.Status != StatusPaid {
		t.Fatalf("replayed webhook result = %#v", replayed)
	}
	assertWallet(t, fixture, fixture.user, 1200, 0)
	assertLedgerCount(t, fixture, order.ID, "payment_order", 1)

	_, err = fixture.webhook(t, order, "evt-paid-once", EventFailed, order.AmountMinor)
	if !errors.Is(err, ErrWebhookReplay) {
		t.Fatalf("conflicting replay error = %v, want ErrWebhookReplay", err)
	}
	assertWallet(t, fixture, fixture.user, 1200, 0)
	assertLedgerCount(t, fixture, order.ID, "payment_order", 1)
}

func TestConcurrentPaymentWebhookCreditsExactlyOnce(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 900, "concurrent-order-idempotency")
	body, err := json.Marshal(map[string]any{
		"event_id": "evt-concurrent", "merchant_id": "merchant-test", "order_no": order.OrderNo,
		"trade_no": order.ProviderTradeNo, "status": EventPaid, "amount_minor": order.AmountMinor,
		"currency": order.Currency, "occurred_at": fixture.now,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	headers, err := fixture.provider.SignWebhook(body, fixture.now)
	if err != nil {
		t.Fatalf("SignWebhook() error = %v", err)
	}
	const workers = 12
	var group sync.WaitGroup
	errorsCh := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, callErr := fixture.service.HandleWebhook(context.Background(), fixture.provider.Name(), headers, body, "req-concurrent-"+strconvItoa(index), fixture.now)
			errorsCh <- callErr
		}(index)
	}
	group.Wait()
	close(errorsCh)
	for callErr := range errorsCh {
		if callErr != nil {
			t.Errorf("concurrent HandleWebhook() error = %v", callErr)
		}
	}
	assertWallet(t, fixture, fixture.user, 900, 0)
	assertLedgerCount(t, fixture, order.ID, "payment_order", 1)
}

func TestWebhookMismatchIsDurableAndInvalidSignatureIsNotPersisted(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 500, "mismatch-order-idempotency")
	first, err := fixture.webhook(t, order, "evt-mismatch", EventPaid, order.AmountMinor+1)
	if !errors.Is(err, ErrWebhookMismatch) || !first.Rejected {
		t.Fatalf("mismatched webhook result = %#v, error = %v", first, err)
	}
	second, err := fixture.webhook(t, order, "evt-mismatch", EventPaid, order.AmountMinor+1)
	if !errors.Is(err, ErrWebhookMismatch) || !second.Replayed {
		t.Fatalf("mismatched replay result = %#v, error = %v", second, err)
	}
	var processingStatus, errorClass string
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT status, last_error_class FROM payment_event_processing WHERE payment_event_id = $1`, first.Event.ID).Scan(&processingStatus, &errorClass); err != nil {
		t.Fatalf("query mismatched processing: %v", err)
	}
	if processingStatus != ProcessingRejected || errorClass != "order_mismatch" {
		t.Fatalf("processing = (%q, %q)", processingStatus, errorClass)
	}
	assertLedgerCount(t, fixture, order.ID, "payment_order", 0)

	for index := 0; index < 3; index++ {
		invalidBody := []byte(`{"event_id":"attacker-` + strconvItoa(index) + `"}`)
		invalidHeaders, err := fixture.provider.SignWebhook(invalidBody, fixture.now)
		if err != nil {
			t.Fatalf("SignWebhook() error = %v", err)
		}
		invalidHeaders[fixtureSignatureHeader][0] = strings.Repeat("0", 64)
		result, err := fixture.service.HandleWebhook(t.Context(), fixture.provider.Name(), invalidHeaders, invalidBody, "req-invalid-"+strconvItoa(index), fixture.now)
		if !errors.Is(err, ErrWebhookInvalid) || result.Event.ID != uuid.Nil || result.Rejected {
			t.Fatalf("invalid webhook result = %#v, error = %v", result, err)
		}
	}
	var rejectedCount int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT count(*) FROM payment_events WHERE payment_provider = $1 AND signature_verified = false`, fixture.provider.Name()).Scan(&rejectedCount); err != nil {
		t.Fatalf("count rejected webhooks: %v", err)
	}
	if rejectedCount != 0 {
		t.Fatalf("rejected webhook count = %d, want 0", rejectedCount)
	}
}

func TestWebhookRejectsConflictingOrderAndProviderReferences(t *testing.T) {
	fixture := newPaymentFixture(t)
	first := fixture.createOrder(t, fixture.user, 500, "reference-order-first")
	second := fixture.createOrder(t, fixture.user, 500, "reference-order-second")
	body, err := json.Marshal(map[string]any{
		"event_id": "evt-conflicting-references", "merchant_id": "merchant-test",
		"order_no": second.OrderNo, "trade_no": first.ProviderTradeNo,
		"status": EventPaid, "amount_minor": second.AmountMinor,
		"currency": second.Currency, "occurred_at": fixture.now,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	headers, err := fixture.provider.SignWebhook(body, fixture.now)
	if err != nil {
		t.Fatalf("SignWebhook() error = %v", err)
	}
	result, err := fixture.service.HandleWebhook(t.Context(), fixture.provider.Name(), headers, body, "req-conflicting-references", fixture.now)
	if !errors.Is(err, ErrWebhookMismatch) || !result.Rejected || result.Order == nil || result.Order.ID != second.ID {
		t.Fatalf("conflicting webhook result = %#v, error = %v", result, err)
	}
	assertLedgerCount(t, fixture, first.ID, "payment_order", 0)
	assertLedgerCount(t, fixture, second.ID, "payment_order", 0)
}

func TestRefundRequiresHoldAndVerifiedEvent(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 1000, "refund-order-idempotency")
	paid, err := fixture.webhook(t, order, "evt-refund-paid", EventPaid, order.AmountMinor)
	if err != nil {
		t.Fatalf("paid webhook error = %v", err)
	}
	order = *paid.Order
	refunding, err := fixture.service.Refund(t.Context(), RefundInput{
		OperatorUserID: fixture.admin, OrderNo: order.OrderNo, RequestID: "req-refund",
	})
	if err != nil {
		t.Fatalf("Refund() error = %v", err)
	}
	if refunding.Status != StatusRefundPending || refunding.ProviderRefundNo == "" {
		t.Fatalf("Refund() = %#v", refunding)
	}
	assertWallet(t, fixture, fixture.user, 0, 1000)

	refunded, err := fixture.webhook(t, refunding, "evt-refunded", EventRefunded, refunding.AmountMinor)
	if err != nil {
		t.Fatalf("refund webhook error = %v", err)
	}
	if refunded.Order == nil || refunded.Order.Status != StatusRefunded {
		t.Fatalf("refund webhook result = %#v", refunded)
	}
	assertWallet(t, fixture, fixture.user, 0, 0)
	assertLedgerCount(t, fixture, order.ID, "payment_order", 3)
	if _, err := fixture.webhook(t, refunding, "evt-refunded", EventRefunded, refunding.AmountMinor); err != nil {
		t.Fatalf("refund replay error = %v", err)
	}
	assertLedgerCount(t, fixture, order.ID, "payment_order", 3)
}

func TestExternalRefundCreatesRecoverableLiability(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 1000, "external-refund-order")
	paid, err := fixture.webhook(t, order, "evt-external-refund-paid", EventPaid, order.AmountMinor)
	if err != nil || paid.Order == nil {
		t.Fatalf("paid webhook = (%#v, %v)", paid, err)
	}
	order = *paid.Order
	if _, err := fixture.billing.Credit(t.Context(), billing.CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: billing.EntryAdminAdjustment,
		AmountMinor: -700, ReferenceType: "admin_case", ReferenceID: "external-refund-spend",
		IdempotencyKey: "external-refund-spend", OperatorUserID: &fixture.admin, Source: "test",
	}); err != nil {
		t.Fatalf("debit wallet: %v", err)
	}

	partial, err := fixture.recoveryWebhook(t, order, "evt-external-refund-1", EventRefunded, 600,
		"re_external_1", "pi_external_1", "ch_external_1", "")
	if err != nil || partial.Order == nil {
		t.Fatalf("partial external refund = (%#v, %v)", partial, err)
	}
	if partial.Order.Status != StatusPaid || partial.Order.ExternalRefundedAmountMinor != 600 {
		t.Fatalf("partial external refund order = %#v", partial.Order)
	}
	assertWallet(t, fixture, fixture.user, 0, 0)
	assertWalletHold(t, fixture, fixture.user, true)
	var principal, recovered int64
	var liabilityStatus string
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT principal_amount_minor, recovered_amount_minor, status
		FROM wallet_liabilities
		WHERE reference_type = 'payment_refund' AND reference_id = $1`,
		fixture.provider.Name()+":re_external_1").Scan(&principal, &recovered, &liabilityStatus); err != nil {
		t.Fatalf("query refund liability: %v", err)
	}
	if principal != 600 || recovered != 300 || liabilityStatus != "open" {
		t.Fatalf("refund liability = (%d, %d, %s)", principal, recovered, liabilityStatus)
	}
	if _, err := fixture.billing.Credit(t.Context(), billing.CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: billing.EntryRecharge,
		AmountMinor: 300, ReferenceType: "test_recharge", ReferenceID: "liability-recovery",
		IdempotencyKey: "liability-recovery-credit", Source: "test",
	}); err != nil {
		t.Fatalf("credit liability recovery: %v", err)
	}
	assertWallet(t, fixture, fixture.user, 0, 0)
	assertWalletHold(t, fixture, fixture.user, false)

	completed, err := fixture.recoveryWebhook(t, *partial.Order, "evt-external-refund-2", EventRefunded, 400,
		"re_external_2", "pi_external_1", "ch_external_1", "")
	if err != nil || completed.Order == nil {
		t.Fatalf("final external refund = (%#v, %v)", completed, err)
	}
	if completed.Order.Status != StatusRefunded || completed.Order.ExternalRefundedAmountMinor != 1000 {
		t.Fatalf("final external refund order = %#v", completed.Order)
	}
	assertWalletHold(t, fixture, fixture.user, true)
	var refundCount int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT count(*) FROM payment_external_refunds WHERE payment_order_id = $1`, order.ID).Scan(&refundCount); err != nil {
		t.Fatalf("count external refunds: %v", err)
	}
	if refundCount != 2 {
		t.Fatalf("external refund count = %d, want 2", refundCount)
	}
	replayed, err := fixture.recoveryWebhook(t, *partial.Order, "evt-external-refund-2", EventRefunded, 400,
		"re_external_2", "pi_external_1", "ch_external_1", "")
	if err != nil || !replayed.Replayed {
		t.Fatalf("external refund replay = (%#v, %v)", replayed, err)
	}
}

func TestDisputeFreezesWalletAndWonEventRestoresRecoveredFunds(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 1000, "dispute-order")
	paid, err := fixture.webhook(t, order, "evt-dispute-paid", EventPaid, order.AmountMinor)
	if err != nil || paid.Order == nil {
		t.Fatalf("paid webhook = (%#v, %v)", paid, err)
	}
	order = *paid.Order
	if _, err := fixture.billing.Credit(t.Context(), billing.CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: billing.EntryAdminAdjustment,
		AmountMinor: -800, ReferenceType: "admin_case", ReferenceID: "dispute-spend",
		IdempotencyKey: "dispute-spend", OperatorUserID: &fixture.admin, Source: "test",
	}); err != nil {
		t.Fatalf("debit wallet: %v", err)
	}
	opened, err := fixture.recoveryWebhook(t, order, "evt-dispute-opened", EventDisputeOpened, 500,
		"", "pi_dispute_1", "ch_dispute_1", "dp_dispute_1")
	if err != nil || opened.Order == nil || opened.Order.Status != StatusPaid {
		t.Fatalf("opened dispute = (%#v, %v)", opened, err)
	}
	assertWallet(t, fixture, fixture.user, 0, 0)
	assertWalletHold(t, fixture, fixture.user, true)

	won, err := fixture.recoveryWebhook(t, *opened.Order, "evt-dispute-won", EventDisputeWon, 500,
		"", "pi_dispute_1", "ch_dispute_1", "dp_dispute_1")
	if err != nil || won.Order == nil {
		t.Fatalf("won dispute = (%#v, %v)", won, err)
	}
	assertWallet(t, fixture, fixture.user, 200, 0)
	assertWalletHold(t, fixture, fixture.user, false)
	var disputeStatus, liabilityStatus string
	var recovered int64
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT dispute.status, liability.status, liability.recovered_amount_minor
		FROM payment_disputes dispute
		JOIN wallet_liabilities liability ON liability.id = dispute.wallet_liability_id
		WHERE dispute.payment_provider = $1 AND dispute.provider_dispute_no = $2`,
		fixture.provider.Name(), "dp_dispute_1").Scan(&disputeStatus, &liabilityStatus, &recovered); err != nil {
		t.Fatalf("query dispute: %v", err)
	}
	if disputeStatus != "won" || liabilityStatus != "reversed" || recovered != 200 {
		t.Fatalf("dispute = (%s, %s, %d)", disputeStatus, liabilityStatus, recovered)
	}
	var liabilityLedgerCount int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT count(*) FROM wallet_ledger ledger
		JOIN wallet_liabilities liability ON liability.id::text = ledger.reference_id
		WHERE liability.reference_type = 'payment_dispute'
		  AND liability.reference_id = $1 AND ledger.entry_type = 'payment_liability'`,
		fixture.provider.Name()+":dp_dispute_1").Scan(&liabilityLedgerCount); err != nil {
		t.Fatalf("count dispute ledger entries: %v", err)
	}
	if liabilityLedgerCount != 2 {
		t.Fatalf("dispute liability ledger count = %d, want 2", liabilityLedgerCount)
	}
}

func TestRefundFailureWebhookReleasesHeldFunds(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 1000, "refund-failure-order-idempotency")
	paid, err := fixture.webhook(t, order, "evt-refund-failure-paid", EventPaid, order.AmountMinor)
	if err != nil || paid.Order == nil || paid.Order.Status != StatusPaid {
		t.Fatalf("paid webhook = (%#v, %v)", paid, err)
	}
	refunding, err := fixture.service.Refund(t.Context(), RefundInput{
		OperatorUserID: fixture.admin, OrderNo: order.OrderNo, RequestID: "req-refund-failure",
	})
	if err != nil {
		t.Fatalf("Refund() error = %v", err)
	}
	assertWallet(t, fixture, fixture.user, 0, 1000)
	failed, err := fixture.webhook(t, refunding, "evt-refund-failed", EventRefundFailed, order.AmountMinor)
	if err != nil {
		t.Fatalf("refund failed webhook error = %v", err)
	}
	if failed.Order == nil || failed.Order.Status != StatusPaid || failed.Order.FailureClass != "provider_refund_failed" || failed.Order.ProviderRefundNo == "" {
		t.Fatalf("refund failed webhook = %#v", failed)
	}
	assertWallet(t, fixture, fixture.user, 1000, 0)
	assertLedgerCount(t, fixture, order.ID, "payment_order", 3)
	if _, err := fixture.webhook(t, refunding, "evt-refund-failed", EventRefundFailed, order.AmountMinor); err != nil {
		t.Fatalf("refund failure replay error = %v", err)
	}
	assertLedgerCount(t, fixture, order.ID, "payment_order", 3)
	if _, err := fixture.service.Refund(t.Context(), RefundInput{
		OperatorUserID: fixture.admin, OrderNo: order.OrderNo, RequestID: "req-refund-failure-retry",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retry failed Refund() error = %v", err)
	}
	assertWallet(t, fixture, fixture.user, 1000, 0)
}

func TestCreateOrderRetriesAmbiguousProviderFailure(t *testing.T) {
	fixture := newPaymentFixture(t)
	clock := fixture.now
	recording := &closeRecordingProvider{FixtureProvider: fixture.provider, createErr: context.DeadlineExceeded}
	registry, err := NewRegistry(recording)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(fixture.store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	fixture.service, err = NewService(repository, fixture.billing, registry, Options{
		OrderTTL: 15 * time.Minute, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	input := CreateOrderInput{
		UserID: fixture.user, AmountMinor: 250, Currency: "USD",
		PaymentProvider: recording.Name(), IdempotencyKey: "ambiguous-create-idempotency",
	}
	first, err := fixture.service.CreateOrder(t.Context(), input)
	if !errors.Is(err, ErrProviderUnavailable) || first.Order.Status != StatusCreated {
		t.Fatalf("first CreateOrder() = (%#v, %v)", first, err)
	}
	recording.createErr = nil
	if _, err := fixture.service.CreateOrder(t.Context(), input); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("early retry CreateOrder() error = %v", err)
	}
	if recording.createCalls.Load() != 1 {
		t.Fatalf("early retry provider calls = %d", recording.createCalls.Load())
	}
	clock = clock.Add(providerOperationRetryBase)
	second, err := fixture.service.CreateOrder(t.Context(), input)
	if err != nil {
		t.Fatalf("retry CreateOrder() error = %v", err)
	}
	if second.Order.ID != first.Order.ID || second.Order.Status != StatusPending || recording.createCalls.Load() != 2 {
		t.Fatalf("retry CreateOrder() = %#v, calls = %d", second, recording.createCalls.Load())
	}
}

func TestProviderCreateAndRefundOperationsAreSingleFlight(t *testing.T) {
	fixture := newPaymentFixture(t)
	blocking := &blockingProvider{
		FixtureProvider: fixture.provider,
		createStarted:   make(chan struct{}, 1),
		createRelease:   make(chan struct{}),
	}
	registry, err := NewRegistry(blocking)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(fixture.store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, fixture.billing, registry, Options{OrderTTL: 15 * time.Minute, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	fixture.service = service
	createInput := CreateOrderInput{
		UserID: fixture.user, AmountMinor: 600, Currency: "USD",
		PaymentProvider: blocking.Name(), IdempotencyKey: "single-flight-create",
	}
	type createResult struct {
		value CreateOrderResult
		err   error
	}
	createDone := make(chan createResult, 1)
	go func() {
		value, createErr := service.CreateOrder(context.Background(), createInput)
		createDone <- createResult{value: value, err: createErr}
	}()
	select {
	case <-blocking.createStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("provider create did not start")
	}
	if _, err := service.CreateOrder(t.Context(), createInput); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("concurrent CreateOrder() error = %v", err)
	}
	if calls := blocking.createCalls.Load(); calls != 1 {
		t.Fatalf("provider create calls = %d", calls)
	}
	close(blocking.createRelease)
	created := <-createDone
	if created.err != nil || created.value.Order.Status != StatusPending {
		t.Fatalf("first CreateOrder() = (%#v, %v)", created.value, created.err)
	}
	if _, err := fixture.webhook(t, created.value.Order, "evt-single-flight-paid", EventPaid, 600); err != nil {
		t.Fatalf("payment webhook error = %v", err)
	}

	blocking.refundStarted = make(chan struct{}, 1)
	blocking.refundRelease = make(chan struct{})
	type refundResult struct {
		value Order
		err   error
	}
	refundDone := make(chan refundResult, 1)
	refundInput := RefundInput{OperatorUserID: fixture.admin, OrderNo: created.value.Order.OrderNo, RequestID: "req-single-flight-refund"}
	go func() {
		value, refundErr := service.Refund(context.Background(), refundInput)
		refundDone <- refundResult{value: value, err: refundErr}
	}()
	select {
	case <-blocking.refundStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("provider refund did not start")
	}
	refundInput.RequestID = "req-single-flight-refund-retry"
	if _, err := service.Refund(t.Context(), refundInput); !errors.Is(err, ErrRefundPending) {
		t.Fatalf("concurrent Refund() error = %v", err)
	}
	if calls := blocking.refundCalls.Load(); calls != 1 {
		t.Fatalf("provider refund calls = %d", calls)
	}
	close(blocking.refundRelease)
	refunded := <-refundDone
	if refunded.err != nil || refunded.value.Status != StatusRefundPending || refunded.value.ProviderRefundNo == "" {
		t.Fatalf("first Refund() = (%#v, %v)", refunded.value, refunded.err)
	}
}

func TestRecoverProviderOperationsAfterExpiredLease(t *testing.T) {
	fixture := newPaymentFixture(t)
	clock := fixture.now
	recording := &closeRecordingProvider{FixtureProvider: fixture.provider}
	registry, err := NewRegistry(recording)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(fixture.store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, fixture.billing, registry, Options{OrderTTL: 15 * time.Minute, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	input := CreateOrderInput{
		UserID: fixture.user, AmountMinor: 350, Currency: "USD",
		PaymentProvider: recording.Name(), IdempotencyKey: "expired-lease-recovery",
	}
	persistedInput := input
	persistedInput.IdempotencyKey = orderIdempotencyKey(input.UserID, input.IdempotencyKey)
	order, err := repository.CreateOrder(t.Context(), persistedInput, fixture.provider.Identity(), clock.Add(15*time.Minute), clock)
	if err != nil {
		t.Fatalf("CreateOrder repository error = %v", err)
	}
	identity := fixture.provider.Identity()
	if _, err := repository.ClaimProviderOperation(t.Context(), order.ID, OperationCreate, identity, providerOperationPayload(OperationCreate, order, identity), clock); err != nil {
		t.Fatalf("ClaimProviderOperation() error = %v", err)
	}
	clock = clock.Add(providerOperationLease + time.Second)
	recovered, err := service.RecoverProviderOperations(t.Context(), 10)
	if err != nil || recovered != 1 || recording.createCalls.Load() != 1 {
		t.Fatalf("RecoverProviderOperations() = (%d, %v), calls = %d", recovered, err, recording.createCalls.Load())
	}
	stored, err := service.GetOrder(t.Context(), fixture.user, order.OrderNo)
	if err != nil || stored.Status != StatusPending || stored.ProviderTradeNo == "" {
		t.Fatalf("recovered order = (%#v, %v)", stored, err)
	}
	if recovered, err := service.RecoverProviderOperations(t.Context(), 10); err != nil || recovered != 0 {
		t.Fatalf("second RecoverProviderOperations() = (%d, %v)", recovered, err)
	}
}

func TestVerifiedWebhookRecoversCreateCrashBeforeCommit(t *testing.T) {
	fixture := newPaymentFixture(t)
	repository := fixture.service.repository
	input := CreateOrderInput{
		UserID: fixture.user, AmountMinor: 450, Currency: "USD",
		PaymentProvider: fixture.provider.Name(), IdempotencyKey: "webhook-create-recovery",
	}
	persistedInput := input
	persistedInput.IdempotencyKey = orderIdempotencyKey(input.UserID, input.IdempotencyKey)
	order, err := repository.CreateOrder(t.Context(), persistedInput, fixture.provider.Identity(), fixture.now.Add(15*time.Minute), fixture.now)
	if err != nil {
		t.Fatalf("CreateOrder repository error = %v", err)
	}
	identity := fixture.provider.Identity()
	claim, err := repository.ClaimProviderOperation(t.Context(), order.ID, OperationCreate, identity, providerOperationPayload(OperationCreate, order, identity), fixture.now)
	if err != nil || !claim.Claimed {
		t.Fatalf("ClaimProviderOperation() = (%#v, %v)", claim, err)
	}
	webhookOrder := order
	webhookOrder.ProviderTradeNo = "fixture_trade_" + order.OrderNo
	recovered, err := fixture.webhook(t, webhookOrder, "evt-create-crash-recovery", EventPaid, order.AmountMinor)
	if err != nil {
		t.Fatalf("recovery webhook error = %v", err)
	}
	if recovered.Order == nil || recovered.Order.Status != StatusPaid || recovered.Order.MerchantID != "merchant-test" || recovered.Order.ProviderLiveMode == nil || *recovered.Order.ProviderLiveMode {
		t.Fatalf("recovered order = %#v", recovered.Order)
	}
	var operationStatus, providerReference string
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT status, provider_reference FROM payment_provider_operations
		WHERE payment_order_id = $1 AND operation_type = 'create'`, order.ID).Scan(&operationStatus, &providerReference); err != nil {
		t.Fatalf("query recovered provider operation: %v", err)
	}
	if operationStatus != OperationSucceeded || providerReference != webhookOrder.ProviderTradeNo {
		t.Fatalf("recovered operation = (%q, %q)", operationStatus, providerReference)
	}
	replayed, err := fixture.service.CreateOrder(t.Context(), input)
	if err != nil || replayed.Order.Status != StatusPaid || replayed.Order.ID != order.ID {
		t.Fatalf("replayed recovered CreateOrder() = (%#v, %v)", replayed, err)
	}
	assertWallet(t, fixture, fixture.user, order.AmountMinor, 0)
}

func TestProviderAPIReconciliationSettlesPaymentAndRefund(t *testing.T) {
	fixture := newPaymentFixture(t)
	order := fixture.createOrder(t, fixture.user, 800, "provider-reconciliation")
	reconciling := &reconciliationProvider{
		FixtureProvider: fixture.provider,
		paymentState: ProviderState{
			MerchantID: "merchant-test", ProviderTradeNo: order.ProviderTradeNo,
			EventType: EventPaid, AmountMinor: order.AmountMinor,
			Currency: order.Currency, OccurredAt: fixture.now,
		},
	}
	registry, err := NewRegistry(reconciling)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(fixture.store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, fixture.billing, registry, Options{OrderTTL: 15 * time.Minute, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	fixture.service = service
	if count, err := service.ReconcileOrders(t.Context(), 10); err != nil || count != 1 {
		t.Fatalf("payment ReconcileOrders() = (%d, %v)", count, err)
	}
	assertWallet(t, fixture, fixture.user, 800, 0)
	refunding, err := service.Refund(t.Context(), RefundInput{OperatorUserID: fixture.admin, OrderNo: order.OrderNo, RequestID: "req-provider-reconcile-refund"})
	if err != nil {
		t.Fatalf("Refund() error = %v", err)
	}
	reconciling.refundState = ProviderState{
		MerchantID: "merchant-test", ProviderTradeNo: refunding.ProviderTradeNo,
		ProviderRefundNo: refunding.ProviderRefundNo, EventType: EventRefunded,
		AmountMinor: refunding.AmountMinor, Currency: refunding.Currency,
		OccurredAt: fixture.now,
	}
	if count, err := service.ReconcileOrders(t.Context(), 10); err != nil || count != 1 {
		t.Fatalf("refund ReconcileOrders() = (%d, %v)", count, err)
	}
	stored, err := service.GetOrder(t.Context(), fixture.user, order.OrderNo)
	if err != nil || stored.Status != StatusRefunded {
		t.Fatalf("reconciled refund order = (%#v, %v)", stored, err)
	}
	assertWallet(t, fixture, fixture.user, 0, 0)
	var providerEvents int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT count(*) FROM payment_events
		WHERE order_id = $1 AND verification_source = 'provider_api' AND signature_verified = false`, order.ID).Scan(&providerEvents); err != nil {
		t.Fatalf("query provider reconciliation events: %v", err)
	}
	if providerEvents != 2 {
		t.Fatalf("provider reconciliation event count = %d", providerEvents)
	}
}

func TestReconciliationFailureDoesNotBlockLaterOrders(t *testing.T) {
	fixture := newPaymentFixture(t)
	clock := fixture.now
	provider := &rotatingReconciliationProvider{FixtureProvider: fixture.provider, states: make(map[string]ProviderState)}
	registry, err := NewRegistry(provider)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(fixture.store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	service, err := NewService(repository, fixture.billing, registry, Options{OrderTTL: 15 * time.Minute, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	fixture.service = service
	first := fixture.createOrder(t, fixture.user, 300, "reconciliation-head-first")
	clock = clock.Add(time.Millisecond)
	second := fixture.createOrder(t, fixture.user, 400, "reconciliation-head-second")
	provider.failOrder = first.OrderNo
	provider.states[second.OrderNo] = ProviderState{
		MerchantID: "merchant-test", ProviderTradeNo: second.ProviderTradeNo,
		EventType: EventPaid, AmountMinor: second.AmountMinor, Currency: second.Currency,
		OccurredAt: clock,
	}
	clock = clock.Add(time.Minute)
	if count, err := service.ReconcileOrders(t.Context(), 1); err == nil || count != 0 {
		t.Fatalf("first ReconcileOrders() = (%d, %v), want isolated failure", count, err)
	}
	clock = clock.Add(time.Millisecond)
	if count, err := service.ReconcileOrders(t.Context(), 1); err != nil || count != 1 {
		t.Fatalf("second ReconcileOrders() = (%d, %v), want later order", count, err)
	}
	storedFirst, err := service.GetOrder(t.Context(), fixture.user, first.OrderNo)
	if err != nil || storedFirst.Status != StatusPending {
		t.Fatalf("first order after rotation = (%#v, %v)", storedFirst, err)
	}
	storedSecond, err := service.GetOrder(t.Context(), fixture.user, second.OrderNo)
	if err != nil || storedSecond.Status != StatusPaid {
		t.Fatalf("second order after rotation = (%#v, %v)", storedSecond, err)
	}
	provider.mu.Lock()
	calls := append([]string(nil), provider.calls...)
	provider.mu.Unlock()
	if len(calls) != 2 || calls[0] != first.OrderNo || calls[1] != second.OrderNo {
		t.Fatalf("reconciliation calls = %v", calls)
	}
}

func TestExpireDueClosesProviderBeforeLocalTransition(t *testing.T) {
	fixture := newPaymentFixture(t)
	clock := fixture.now
	recording := &closeRecordingProvider{FixtureProvider: fixture.provider}
	registry, err := NewRegistry(recording)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	repository, err := NewRepository(fixture.store, fixture.billing, fixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	fixture.service, err = NewService(repository, fixture.billing, registry, Options{
		OrderTTL: 15 * time.Minute, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	order := fixture.createOrder(t, fixture.user, 300, "expire-order-idempotency")
	clock = clock.Add(16 * time.Minute)
	expired, err := fixture.service.ExpireDue(t.Context(), 10)
	if err != nil {
		t.Fatalf("ExpireDue() error = %v", err)
	}
	if expired != 1 || recording.closeCalls.Load() != 1 {
		t.Fatalf("ExpireDue() = %d, close calls = %d", expired, recording.closeCalls.Load())
	}
	stored, err := fixture.service.GetOrder(t.Context(), fixture.user, order.OrderNo)
	if err != nil {
		t.Fatalf("GetOrder() error = %v", err)
	}
	if stored.Status != StatusExpired {
		t.Fatalf("expired order status = %q", stored.Status)
	}

	failedFixture := newPaymentFixture(t)
	failedClock := failedFixture.now
	closeFailure := errors.New("temporary provider close failure")
	failing := &closeRecordingProvider{FixtureProvider: failedFixture.provider, closeErr: closeFailure}
	failingRegistry, err := NewRegistry(failing)
	if err != nil {
		t.Fatalf("NewRegistry(failing) error = %v", err)
	}
	failingRepository, err := NewRepository(failedFixture.store, failedFixture.billing, failedFixture.voucherKeys)
	if err != nil {
		t.Fatalf("NewRepository(failing) error = %v", err)
	}
	failedFixture.service, err = NewService(failingRepository, failedFixture.billing, failingRegistry, Options{
		OrderTTL: 15 * time.Minute, Now: func() time.Time { return failedClock },
	})
	if err != nil {
		t.Fatalf("NewService(failing) error = %v", err)
	}
	failedOrder := failedFixture.createOrder(t, failedFixture.user, 300, "expire-failure-idempotency")
	failedClock = failedClock.Add(16 * time.Minute)
	if count, err := failedFixture.service.ExpireDue(t.Context(), 10); count != 0 || !errors.Is(err, closeFailure) {
		t.Fatalf("failing ExpireDue() = (%d, %v)", count, err)
	}
	stored, err = failedFixture.service.GetOrder(t.Context(), failedFixture.user, failedOrder.OrderNo)
	if err != nil {
		t.Fatalf("GetOrder(failed) error = %v", err)
	}
	if stored.Status != StatusPending {
		t.Fatalf("failed-close order status = %q", stored.Status)
	}
}

func TestPaymentHTTPSessionRBACAndWebhookFlow(t *testing.T) {
	fixture := newPaymentFixture(t)
	authRepository, err := auth.NewRepository(fixture.store)
	if err != nil {
		t.Fatalf("auth.NewRepository() error = %v", err)
	}
	authService, err := auth.NewService(authRepository, auth.ServiceConfig{
		SessionTTL: 12 * time.Hour, Issuer: "Bablo Test", RequireAdminMFA: false,
	})
	if err != nil {
		t.Fatalf("auth.NewService() error = %v", err)
	}
	user, err := authService.CreateLocalUser(t.Context(), "payment-http-user@example.test", "payment user password value", false, "create-http-user")
	if err != nil {
		t.Fatalf("CreateLocalUser(user) error = %v", err)
	}
	admin, err := authService.CreateLocalUser(t.Context(), "payment-http-admin@example.test", "payment admin password value", true, "create-http-admin")
	if err != nil {
		t.Fatalf("CreateLocalUser(admin) error = %v", err)
	}
	userSession, err := authService.Login(t.Context(), user.Email, "payment user password value", "", auth.LoginMetadata{RequestID: "login-http-user"})
	if err != nil {
		t.Fatalf("Login(user) error = %v", err)
	}
	adminSession, err := authService.Login(t.Context(), admin.Email, "payment admin password value", "", auth.LoginMetadata{RequestID: "login-http-admin"})
	if err != nil {
		t.Fatalf("Login(admin) error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authHandler, err := auth.NewHandler(authService, logger, auth.HandlerConfig{
		AllowedOrigin: "https://console.example", SessionTTL: 12 * time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.NewHandler() error = %v", err)
	}
	paymentHandler, err := NewHandler(fixture.service, logger)
	if err != nil {
		t.Fatalf("payment.NewHandler() error = %v", err)
	}
	server := httpapi.New(config.Config{}, logger, "test",
		httpapi.WithPaymentUserHandler(authHandler.Protect(http.HandlerFunc(paymentHandler.ServeUserHTTP))),
		httpapi.WithPaymentAdminHandler(authHandler.ProtectRole(http.HandlerFunc(paymentHandler.ServeAdminHTTP), "admin")),
		httpapi.WithPaymentWebhookHandler(http.HandlerFunc(paymentHandler.ServeWebhookHTTP)),
	)

	missingIdempotency := paymentControlRequest(t, server.Handler(), http.MethodPost, "/api/v1/me/payment-orders", map[string]any{
		"amount_minor": 500, "currency": "USD", "payment_provider": fixture.provider.Name(),
	}, userSession, "")
	if missingIdempotency.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency status = %d, body = %s", missingIdempotency.Code, missingIdempotency.Body.String())
	}

	createdResponse := paymentControlRequest(t, server.Handler(), http.MethodPost, "/api/v1/me/payment-orders", map[string]any{
		"amount_minor": 500, "currency": "USD", "payment_provider": fixture.provider.Name(),
	}, userSession, "http-order-idempotency")
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create order status = %d, body = %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created struct {
		Order orderView `json:"order"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create order: %v", err)
	}
	if created.Order.OrderNo == "" || created.Order.ProviderTradeNo == "" || !strings.HasPrefix(created.Order.OrderNo, "bablo_pay_") {
		t.Fatalf("create order response = %#v", created.Order)
	}

	forbidden := paymentControlRequest(t, server.Handler(), http.MethodPost, "/api/v1/admin/wallet-credits", map[string]any{
		"user_id": user.ID.String(), "amount_minor": 400, "currency": "USD",
	}, userSession, "http-admin-recharge")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("non-admin recharge status = %d, body = %s", forbidden.Code, forbidden.Body.String())
	}
	recharged := paymentControlRequest(t, server.Handler(), http.MethodPost, "/api/v1/admin/wallet-credits", map[string]any{
		"user_id": user.ID.String(), "amount_minor": 400, "currency": "USD",
	}, adminSession, "http-admin-recharge")
	if recharged.Code != http.StatusOK {
		t.Fatalf("admin recharge status = %d, body = %s", recharged.Code, recharged.Body.String())
	}

	body, err := json.Marshal(map[string]any{
		"event_id": "evt-http-paid", "merchant_id": "merchant-test", "order_no": created.Order.OrderNo,
		"trade_no": created.Order.ProviderTradeNo, "status": EventPaid, "amount_minor": int64(500),
		"currency": "USD", "occurred_at": fixture.now,
	})
	if err != nil {
		t.Fatalf("json.Marshal(webhook) error = %v", err)
	}
	headers, err := fixture.provider.SignWebhook(body, fixture.now)
	if err != nil {
		t.Fatalf("SignWebhook() error = %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/"+fixture.provider.Name(), bytes.NewReader(body))
		for key, values := range headers {
			for _, value := range values {
				request.Header.Add(key, value)
			}
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
			t.Fatalf("webhook attempt %d response = %d %s", attempt, recorder.Code, recorder.Body.String())
		}
	}
	assertWallet(t, fixture, user.ID, 900, 0)
	orderID, err := uuid.Parse(created.Order.ID)
	if err != nil {
		t.Fatalf("parse order ID: %v", err)
	}
	assertLedgerCount(t, fixture, orderID, "payment_order", 1)

	queried := paymentControlRequest(t, server.Handler(), http.MethodGet, "/api/v1/me/payment-orders/"+created.Order.OrderNo, nil, userSession, "")
	if queried.Code != http.StatusOK || !strings.Contains(queried.Body.String(), `"status":"paid"`) {
		t.Fatalf("query order response = %d %s", queried.Code, queried.Body.String())
	}
	replayedCreate := paymentControlRequest(t, server.Handler(), http.MethodPost, "/api/v1/me/payment-orders", map[string]any{
		"amount_minor": 500, "currency": "USD", "payment_provider": fixture.provider.Name(),
	}, userSession, "http-order-idempotency")
	if replayedCreate.Code != http.StatusCreated || !strings.Contains(replayedCreate.Body.String(), created.Order.OrderNo) {
		t.Fatalf("replayed create response = %d %s", replayedCreate.Code, replayedCreate.Body.String())
	}
}

func paymentControlRequest(t *testing.T, handler http.Handler, method, path string, payload any, session auth.SessionBundle, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("json.Marshal(request) error = %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.AddCookie(&http.Cookie{Name: "bablo_session", Value: session.SessionToken})
	request.AddCookie(&http.Cookie{Name: "bablo_csrf", Value: session.CSRFToken})
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", "https://console.example")
		request.Header.Set("X-CSRF-Token", session.CSRFToken)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestVoucherAndManualRechargeFinancialIdempotency(t *testing.T) {
	fixture := newPaymentFixture(t)
	created, err := fixture.service.CreateVoucher(t.Context(), CreateVoucherInput{
		OperatorUserID: fixture.admin, AmountMinor: 700, Currency: "USD",
		RequestID: "req-voucher-create", IdempotencyKey: "voucher-create-idempotency",
	})
	if err != nil {
		t.Fatalf("CreateVoucher() error = %v", err)
	}
	if created.Code == "" || strings.Contains(created.Voucher.CodePrefix, created.Code) {
		t.Fatalf("CreatedVoucher() = %#v", created)
	}
	replayed, err := fixture.service.CreateVoucher(t.Context(), CreateVoucherInput{
		OperatorUserID: fixture.admin, AmountMinor: 700, Currency: "USD",
		RequestID: "req-voucher-create-retry", IdempotencyKey: "voucher-create-idempotency",
	})
	if err != nil {
		t.Fatalf("replayed CreateVoucher() error = %v", err)
	}
	if replayed.Voucher.ID != created.Voucher.ID || replayed.Code != created.Code {
		t.Fatalf("replayed CreateVoucher() = %#v, want original code", replayed)
	}
	var ciphertext []byte
	if err := fixture.store.Queryer().QueryRow(t.Context(), `SELECT code_ciphertext FROM payment_vouchers WHERE id = $1`, created.Voucher.ID).Scan(&ciphertext); err != nil {
		t.Fatalf("query voucher ciphertext: %v", err)
	}
	if len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(created.Code)) {
		t.Fatalf("voucher ciphertext does not safely wrap code")
	}

	users := []uuid.UUID{fixture.user, fixture.other}
	var group sync.WaitGroup
	results := make(chan struct {
		user uuid.UUID
		err  error
	}, len(users))
	for _, userID := range users {
		group.Add(1)
		go func(userID uuid.UUID) {
			defer group.Done()
			_, redeemErr := fixture.service.RedeemVoucher(context.Background(), RedeemVoucherInput{
				UserID: userID, Code: created.Code, RequestID: "req-redeem-" + userID.String(),
			})
			results <- struct {
				user uuid.UUID
				err  error
			}{user: userID, err: redeemErr}
		}(userID)
	}
	group.Wait()
	close(results)
	var winner uuid.UUID
	for result := range results {
		switch {
		case result.err == nil:
			if winner != uuid.Nil {
				t.Fatalf("more than one voucher redemption succeeded")
			}
			winner = result.user
		case errors.Is(result.err, ErrVoucherUnavailable):
		default:
			t.Fatalf("RedeemVoucher() error = %v", result.err)
		}
	}
	if winner == uuid.Nil {
		t.Fatal("no voucher redemption succeeded")
	}
	assertWallet(t, fixture, winner, 700, 0)
	assertLedgerCount(t, fixture, created.Voucher.ID, "payment_voucher", 1)
	var codeHashLength int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `SELECT octet_length(code_hash) FROM payment_vouchers WHERE id = $1`, created.Voucher.ID).Scan(&codeHashLength); err != nil {
		t.Fatalf("query voucher hash: %v", err)
	}
	if codeHashLength != 32 {
		t.Fatalf("voucher hash length = %d", codeHashLength)
	}
	var ciphertextCleared bool
	if err := fixture.store.Queryer().QueryRow(t.Context(), `SELECT code_ciphertext IS NULL FROM payment_vouchers WHERE id = $1`, created.Voucher.ID).Scan(&ciphertextCleared); err != nil {
		t.Fatalf("query terminal voucher ciphertext: %v", err)
	}
	if !ciphertextCleared {
		t.Fatal("redeemed voucher retained replay ciphertext")
	}

	input := ManualRechargeInput{
		OperatorUserID: fixture.admin, UserID: fixture.other, AmountMinor: 400,
		Currency: "USD", RequestID: "req-manual-recharge", IdempotencyKey: "manual-recharge-idempotency",
	}
	first, err := fixture.service.ManualRecharge(t.Context(), input)
	if err != nil {
		t.Fatalf("ManualRecharge() error = %v", err)
	}
	var voucherOutboxType string
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT aggregate_type FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'payment.voucher.redeemed'`, created.Voucher.ID).Scan(&voucherOutboxType); err != nil {
		t.Fatalf("query voucher outbox: %v", err)
	}
	if voucherOutboxType != "payment_voucher" {
		t.Fatalf("voucher outbox aggregate_type = %q", voucherOutboxType)
	}
	input.RequestID = "req-manual-recharge-retry"
	second, err := fixture.service.ManualRecharge(t.Context(), input)
	if err != nil {
		t.Fatalf("replayed ManualRecharge() error = %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replayed recharge ledger IDs = %s and %s", first.ID, second.ID)
	}
	input.AmountMinor++
	if _, err := fixture.service.ManualRecharge(t.Context(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting ManualRecharge() error = %v, want ErrConflict", err)
	}
	input.AmountMinor = 400
	input.Currency = "EUR"
	if _, err := fixture.service.ManualRecharge(t.Context(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-currency ManualRecharge() error = %v, want ErrConflict", err)
	}
	var euroWallets int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `SELECT count(*) FROM wallets WHERE user_id = $1 AND currency = 'EUR'`, fixture.other).Scan(&euroWallets); err != nil {
		t.Fatalf("count EUR wallets: %v", err)
	}
	if euroWallets != 0 {
		t.Fatalf("cross-currency replay created %d EUR wallets", euroWallets)
	}
}

func assertWallet(t *testing.T, fixture paymentFixture, userID uuid.UUID, available, reserved int64) {
	t.Helper()
	var actualAvailable, actualReserved int64
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT available_balance_minor, reserved_balance_minor
		FROM wallets WHERE user_id = $1 AND currency = 'USD'`, userID).Scan(&actualAvailable, &actualReserved); err != nil {
		t.Fatalf("query wallet: %v", err)
	}
	if actualAvailable != available || actualReserved != reserved {
		t.Fatalf("wallet = (%d, %d), want (%d, %d)", actualAvailable, actualReserved, available, reserved)
	}
}

func assertWalletHold(t *testing.T, fixture paymentFixture, userID uuid.UUID, expected bool) {
	t.Helper()
	var actual bool
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT financial_hold FROM wallets WHERE user_id = $1 AND currency = 'USD'`, userID).Scan(&actual); err != nil {
		t.Fatalf("query wallet hold: %v", err)
	}
	if actual != expected {
		t.Fatalf("wallet financial_hold = %t, want %t", actual, expected)
	}
}

func assertLedgerCount(t *testing.T, fixture paymentFixture, referenceID uuid.UUID, referenceType string, expected int) {
	t.Helper()
	var count int
	if err := fixture.store.Queryer().QueryRow(t.Context(), `
		SELECT count(*) FROM wallet_ledger
		WHERE reference_type = $1 AND reference_id = $2`, referenceType, referenceID.String()).Scan(&count); err != nil {
		t.Fatalf("count wallet ledger: %v", err)
	}
	if count != expected {
		t.Fatalf("wallet ledger count = %d, want %d", count, expected)
	}
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
