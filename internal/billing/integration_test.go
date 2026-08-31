package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/usage"
	"github.com/starhui-dev/bablo/migrations"
)

type billingFixture struct {
	user   uuid.UUID
	apiKey uuid.UUID
	price  uuid.UUID
}

func openBillingTestStore(t *testing.T) *data.Store {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_billing_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 16})
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func seedBillingFixture(t *testing.T, store *data.Store) billingFixture {
	t.Helper()
	fixture := billingFixture{user: uuid.New(), apiKey: uuid.New(), price: uuid.New()}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO users (id, email_normalized, password_hash, password_params_version)
		VALUES ($1, $2, 'billing-test-hash', 'test')`,
		fixture.user, "billing-"+fixture.user.String()+"@example.test"); err != nil {
		t.Fatalf("insert billing user: %v", err)
	}
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO api_keys (id, user_id, name, key_prefix, secret_hash)
		VALUES ($1, $2, 'billing-test', $3, $4)`,
		fixture.apiKey, fixture.user, "bablo_sk_"+fixture.apiKey.String()[:12], []byte(fixture.apiKey.String())); err != nil {
		t.Fatalf("insert billing API key: %v", err)
	}
	var priceCount int
	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FROM price_versions`).Scan(&priceCount); err != nil {
		t.Fatalf("count billing price versions: %v", err)
	}
	scopes := []string{pricing.ScopeGlobal, pricing.ScopeModel, pricing.ScopeProviderModel}
	scope := scopes[priceCount%len(scopes)]
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO price_versions (
			id, scope, version_no, currency, effective_from, status, created_by
		)
		SELECT $1, $2, COALESCE(MAX(version_no), 0) + 1,
			'USD', now() - interval '1 minute', 'active', $3
		FROM price_versions
		WHERE scope = $2`, fixture.price, scope, fixture.user); err != nil {
		t.Fatalf("insert billing price version: %v", err)
	}
	return fixture
}

func billingServiceForTest(t *testing.T, store *data.Store) *Service {
	t.Helper()
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error: %v", err)
	}
	service, err := NewService(repository, Options{DefaultOutputTokens: 1})
	if err != nil {
		t.Fatalf("NewService() error: %v", err)
	}
	return service
}

func priceSnapshot(priceID uuid.UUID, inputPrice, outputPrice string) pricing.Snapshot {
	return pricing.Snapshot{
		VersionID: priceID,
		Currency:  "USD",
		Prices: map[string]string{
			pricing.DimensionInputToken:  inputPrice,
			pricing.DimensionOutputToken: outputPrice,
		},
	}
}

func reserveInput(fixture billingFixture, requestID string, snapshot pricing.Snapshot, inputTokens, outputTokens int64) ReserveInput {
	return ReserveInput{
		UserID:    fixture.user,
		APIKeyID:  fixture.apiKey,
		RequestID: requestID,
		Price:     snapshot,
		EstimatedUsage: usage.TokenUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		},
	}
}

func creditWallet(t *testing.T, service *Service, fixture billingFixture, amount int64, key string) LedgerEntry {
	t.Helper()
	entry, err := service.Credit(context.Background(), CreditInput{
		UserID:         fixture.user,
		Currency:       "USD",
		EntryType:      EntryRecharge,
		AmountMinor:    amount,
		ReferenceType:  "test_recharge",
		ReferenceID:    key,
		IdempotencyKey: key,
		Source:         "test",
	})
	if err != nil {
		t.Fatalf("Credit() error: %v", err)
	}
	return entry
}

func insertBillingUsage(t *testing.T, store *data.Store, fixture billingFixture, reservation Reservation, amount int64, estimated bool) usage.Event {
	t.Helper()
	eventID := uuid.New()
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO usage_events (
			id, settlement_key, request_id, user_id, api_key_id, wallet_id,
			requested_model, started_at, finished_at, price_version_id,
			input_tokens, output_tokens, amount_minor, currency, estimated,
			provenance, terminal_status, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'billing-test-model', $7, $8, $9,
			1, 1, $10, 'USD', $11, 'adapter', 'succeeded', $8)`,
		eventID,
		"settlement:"+eventID.String(),
		reservation.RequestID,
		fixture.user,
		fixture.apiKey,
		reservation.WalletID,
		now.Add(-time.Second),
		now,
		fixture.price,
		amount,
		estimated,
	); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
	amountCopy := amount
	return usage.Event{
		ID:             eventID,
		RequestID:      reservation.RequestID,
		UserID:         uuidPointer(fixture.user),
		APIKeyID:       uuidPointer(fixture.apiKey),
		WalletID:       uuidPointer(reservation.WalletID),
		PriceVersionID: uuidPointer(fixture.price),
		AmountMinor:    &amountCopy,
		Currency:       "USD",
		Estimated:      estimated,
	}
}

func TestReserveRejectsUnpublishedPriceVersion(t *testing.T) {
	store := openBillingTestStore(t)
	fixture := seedBillingFixture(t, store)
	service := billingServiceForTest(t, store)
	creditWallet(t, service, fixture, 100, "credit-unpublished")
	draftID := uuid.New()
	if _, err := store.Queryer().Exec(context.Background(), `
		INSERT INTO price_versions (
			id, scope, version_no, currency, effective_from, status, created_by
		)
		VALUES ($1, 'global', 999, 'USD', now() - interval '1 minute', 'draft', $2)`, draftID, fixture.user); err != nil {
		t.Fatalf("insert draft price version: %v", err)
	}
	_, err := service.Reserve(context.Background(), reserveInput(
		fixture,
		"billing-unpublished",
		priceSnapshot(draftID, "0", "0.10"),
		0,
		1,
	))
	if !errors.Is(err, ErrPriceMissing) {
		t.Fatalf("Reserve() error = %v, want ErrPriceMissing", err)
	}
}

func TestPriceVersionSwitchKeepsExistingReservationSnapshot(t *testing.T) {
	store := openBillingTestStore(t)
	fixture := seedBillingFixture(t, store)
	service := billingServiceForTest(t, store)
	ctx := context.Background()
	creditWallet(t, service, fixture, 100, "credit-price-switch")

	oldReservation, err := service.Reserve(ctx, reserveInput(
		fixture,
		"billing-price-before-cutover",
		priceSnapshot(fixture.price, "0", "0.10"),
		0,
		1,
	))
	if err != nil {
		t.Fatalf("old Reserve() error: %v", err)
	}
	cutover := time.Now().UTC().Add(-time.Second)
	if _, err := store.Queryer().Exec(ctx, `
		UPDATE price_versions
		SET status = 'retired', effective_to = $2
		WHERE id = $1`, fixture.price, cutover); err != nil {
		t.Fatalf("retire old price version: %v", err)
	}
	newPriceID := uuid.New()
	if _, err := store.Queryer().Exec(ctx, `
		INSERT INTO price_versions (
			id, scope, version_no, currency, effective_from, status, created_by
		)
		VALUES ($1, 'global', 2, 'USD', $2, 'active', $3)`, newPriceID, cutover, fixture.user); err != nil {
		t.Fatalf("insert replacement price version: %v", err)
	}
	newReservation, err := service.Reserve(ctx, reserveInput(
		fixture,
		"billing-price-after-cutover",
		priceSnapshot(newPriceID, "0", "0.20"),
		0,
		1,
	))
	if err != nil {
		t.Fatalf("new Reserve() error: %v", err)
	}
	if oldReservation.PriceVersionID != fixture.price || oldReservation.AmountMinor != 10 || newReservation.PriceVersionID != newPriceID || newReservation.AmountMinor != 20 {
		t.Fatalf("price switch reservations = old %+v / new %+v", oldReservation, newReservation)
	}

	oldEvent := insertBillingUsage(t, store, fixture, oldReservation, 10, false)
	if settlement, err := service.Settle(ctx, SettleInput{ReservationID: oldReservation.ID, Event: oldEvent}); err != nil || settlement.Status != SettlementSettled {
		t.Fatalf("old snapshot Settle() = %+v, error=%v", settlement, err)
	}
	wallet, err := service.GetWallet(ctx, fixture.user, "USD")
	if err != nil {
		t.Fatalf("GetWallet() error: %v", err)
	}
	if wallet.AvailableBalanceMinor != 70 || wallet.ReservedBalanceMinor != 20 {
		t.Fatalf("wallet after price switch = %+v", wallet)
	}
}

func TestReserveSettleIsIdempotentAndLedgerRebuildsWallet(t *testing.T) {
	store := openBillingTestStore(t)
	fixture := seedBillingFixture(t, store)
	service := billingServiceForTest(t, store)
	ctx := context.Background()
	credit := creditWallet(t, service, fixture, 500, "credit-standard")

	input := reserveInput(fixture, "billing-standard", priceSnapshot(fixture.price, "0.25", "0.25"), 1, 1)
	reservation, err := service.Reserve(ctx, input)
	if err != nil {
		t.Fatalf("Reserve() error: %v", err)
	}
	duplicate, err := service.Reserve(ctx, input)
	if err != nil {
		t.Fatalf("duplicate Reserve() error: %v", err)
	}
	if duplicate.ID != reservation.ID || reservation.AmountMinor != 50 {
		t.Fatalf("idempotent reservation = %+v / %+v", reservation, duplicate)
	}

	event := insertBillingUsage(t, store, fixture, reservation, 30, false)
	settlement, err := service.Settle(ctx, SettleInput{ReservationID: reservation.ID, Event: event})
	if err != nil {
		t.Fatalf("Settle() error: %v", err)
	}
	duplicateSettlement, err := service.Settle(ctx, SettleInput{ReservationID: reservation.ID, Event: event})
	if err != nil {
		t.Fatalf("duplicate Settle() error: %v", err)
	}
	if duplicateSettlement.ID != settlement.ID || settlement.Status != SettlementSettled {
		t.Fatalf("idempotent settlement = %+v / %+v", settlement, duplicateSettlement)
	}
	wallet, err := service.GetWallet(ctx, fixture.user, "USD")
	if err != nil {
		t.Fatalf("GetWallet() error: %v", err)
	}
	if wallet.ID != credit.WalletID || wallet.AvailableBalanceMinor != 470 || wallet.ReservedBalanceMinor != 0 {
		t.Fatalf("wallet = %+v, want available=470 reserved=0", wallet)
	}
	available, reserved, err := service.RebuildBalance(ctx, wallet.ID)
	if err != nil {
		t.Fatalf("RebuildBalance() error: %v", err)
	}
	if available != wallet.AvailableBalanceMinor || reserved != wallet.ReservedBalanceMinor {
		t.Fatalf("rebuilt balances = %d/%d, wallet = %d/%d", available, reserved, wallet.AvailableBalanceMinor, wallet.ReservedBalanceMinor)
	}
	if count := billingCount(t, store, `SELECT count(*) FROM wallet_ledger WHERE wallet_id = $1`, wallet.ID); count != 4 {
		t.Fatalf("ledger count = %d, want recharge+reservation+charge+release", count)
	}
	if count := billingCount(t, store, `SELECT count(*) FROM billing_settlements WHERE reservation_id = $1`, reservation.ID); count != 1 {
		t.Fatalf("settlement count = %d, want 1", count)
	}
	if _, err := store.Queryer().Exec(ctx, `UPDATE wallet_ledger SET amount_minor = amount_minor + 1 WHERE id = $1`, credit.ID); postgresCode(err) != "55000" {
		t.Fatalf("ledger update code = %q, error = %v", postgresCode(err), err)
	}
	if _, err := store.Queryer().Exec(ctx, `DELETE FROM wallet_ledger WHERE id = $1`, credit.ID); postgresCode(err) != "55000" {
		t.Fatalf("ledger delete code = %q, error = %v", postgresCode(err), err)
	}
}

func TestSettlementPendingRetriesAfterFunding(t *testing.T) {
	store := openBillingTestStore(t)
	fixture := seedBillingFixture(t, store)
	service := billingServiceForTest(t, store)
	ctx := context.Background()
	creditWallet(t, service, fixture, 50, "credit-pending")
	reservation, err := service.Reserve(ctx, reserveInput(fixture, "billing-pending", priceSnapshot(fixture.price, "0", "0.50"), 0, 1))
	if err != nil {
		t.Fatalf("Reserve() error: %v", err)
	}
	event := insertBillingUsage(t, store, fixture, reservation, 100, false)
	pending, err := service.Settle(ctx, SettleInput{ReservationID: reservation.ID, Event: event})
	if !errors.Is(err, ErrSettlementPending) || pending.Status != SettlementPending {
		t.Fatalf("pending Settle() = %+v, error=%v", pending, err)
	}
	wallet, err := service.GetWallet(ctx, fixture.user, "USD")
	if err != nil {
		t.Fatalf("GetWallet() error: %v", err)
	}
	if wallet.AvailableBalanceMinor != 0 || wallet.ReservedBalanceMinor != 50 {
		t.Fatalf("pending wallet = %+v", wallet)
	}
	if count := billingCount(t, store, `SELECT count(*) FROM wallet_ledger WHERE wallet_id = $1 AND entry_type = 'usage_charge'`, wallet.ID); count != 0 {
		t.Fatalf("pending usage charge count = %d, want 0", count)
	}
	creditWallet(t, service, fixture, 60, "credit-retry")
	settled, err := service.Settle(ctx, SettleInput{ReservationID: reservation.ID, Event: event})
	if err != nil || settled.Status != SettlementSettled {
		t.Fatalf("retried Settle() = %+v, error=%v", settled, err)
	}
	wallet, err = service.GetWallet(ctx, fixture.user, "USD")
	if err != nil {
		t.Fatalf("GetWallet() after retry error: %v", err)
	}
	if wallet.AvailableBalanceMinor != 10 || wallet.ReservedBalanceMinor != 0 {
		t.Fatalf("settled wallet = %+v, want available=10 reserved=0", wallet)
	}
	available, reserved, err := service.RebuildBalance(ctx, wallet.ID)
	if err != nil || available != 10 || reserved != 0 {
		t.Fatalf("rebuilt pending wallet = %d/%d error=%v", available, reserved, err)
	}
}

func TestReserveEnforcesBudgetAndConcurrentWalletFunds(t *testing.T) {
	store := openBillingTestStore(t)
	fixture := seedBillingFixture(t, store)
	service := billingServiceForTest(t, store)
	ctx := context.Background()
	creditWallet(t, service, fixture, 200, "credit-budget")
	limit := int64(50)
	first := reserveInput(fixture, "billing-budget-first", priceSnapshot(fixture.price, "0", "0.30"), 0, 1)
	first.DailyBudgetMinor = &limit
	if _, err := service.Reserve(ctx, first); err != nil {
		t.Fatalf("first budget Reserve() error: %v", err)
	}
	second := reserveInput(fixture, "billing-budget-second", first.Price, 0, 1)
	second.DailyBudgetMinor = &limit
	if _, err := service.Reserve(ctx, second); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second budget Reserve() error = %v, want ErrBudgetExceeded", err)
	}

	other := seedBillingFixture(t, store)
	const (
		requestCount = 128
		walletFunds  = 100
	)
	creditWallet(t, service, other, walletFunds, "credit-concurrent")
	snapshot := priceSnapshot(other.price, "0", "0.01")
	inputs := make([]ReserveInput, requestCount)
	for index := range inputs {
		inputs[index] = reserveInput(other, "billing-concurrent-"+uuid.NewString(), snapshot, 0, 1)
	}
	var wait sync.WaitGroup
	wait.Add(len(inputs))
	errorsByRequest := make([]error, len(inputs))
	for index := range inputs {
		index := index
		go func() {
			defer wait.Done()
			_, errorsByRequest[index] = service.Reserve(context.Background(), inputs[index])
		}()
	}
	wait.Wait()
	successes := 0
	insufficient := 0
	for _, err := range errorsByRequest {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInsufficientFunds):
			insufficient++
		default:
			t.Fatalf("concurrent Reserve() unexpected error: %v", err)
		}
	}
	if successes != walletFunds || insufficient != requestCount-walletFunds {
		t.Fatalf("concurrent outcomes success=%d insufficient=%d", successes, insufficient)
	}
	wallet, err := service.GetWallet(ctx, other.user, "USD")
	if err != nil {
		t.Fatalf("GetWallet() error: %v", err)
	}
	if wallet.AvailableBalanceMinor != 0 || wallet.ReservedBalanceMinor != walletFunds {
		t.Fatalf("concurrent wallet = %+v", wallet)
	}
}

func TestCreditAndAdminAdjustmentAreIdempotentAndAudited(t *testing.T) {
	store := openBillingTestStore(t)
	fixture := seedBillingFixture(t, store)
	service := billingServiceForTest(t, store)
	ctx := context.Background()
	first := creditWallet(t, service, fixture, 20, "credit-idempotent")
	duplicate := creditWallet(t, service, fixture, 20, "credit-idempotent")
	if first.ID != duplicate.ID {
		t.Fatalf("idempotent credits differ: %+v / %+v", first, duplicate)
	}
	if _, err := service.Credit(ctx, CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: EntryRecharge,
		AmountMinor: 21, ReferenceType: "test_recharge", ReferenceID: "credit-idempotent",
		IdempotencyKey: "credit-idempotent", Source: "test",
	}); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("conflicting Credit() error = %v, want ErrSettlementConflict", err)
	}
	operator := fixture.user
	adjustment, err := service.Credit(ctx, CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: EntryAdminAdjustment,
		AmountMinor: -5, ReferenceType: "admin_case", ReferenceID: "case-1",
		IdempotencyKey: "admin-adjustment-1", OperatorUserID: &operator, Source: "admin",
	})
	if err != nil {
		t.Fatalf("admin Credit() error: %v", err)
	}
	if adjustment.AvailableBalanceAfterMinor != 15 {
		t.Fatalf("adjustment = %+v, want available=15", adjustment)
	}
	if count := billingCount(t, store, `SELECT count(*) FROM audit_logs WHERE action = 'wallet.admin_adjustment' AND target_id = $1`, adjustment.WalletID.String()); count != 1 {
		t.Fatalf("admin adjustment audit count = %d, want 1", count)
	}
	refundInput := CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: EntryRefund,
		AmountMinor: 7, ReferenceType: "payment_refund", ReferenceID: "refund-1",
		IdempotencyKey: "refund-1", Source: "payment",
	}
	refund, err := service.Credit(ctx, refundInput)
	if err != nil {
		t.Fatalf("refund Credit() error: %v", err)
	}
	duplicateRefund, err := service.Credit(ctx, refundInput)
	if err != nil || duplicateRefund.ID != refund.ID {
		t.Fatalf("idempotent refund = %+v / %+v, error=%v", refund, duplicateRefund, err)
	}
	if _, err := service.Credit(ctx, CreditInput{
		UserID: fixture.user, Currency: "USD", EntryType: EntryAdjustment,
		AmountMinor: -23, ReferenceType: "correction", ReferenceID: "negative-balance",
		IdempotencyKey: "negative-balance", Source: "test",
	}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("negative-balance adjustment error = %v, want ErrInsufficientFunds", err)
	}
	wallet, err := service.GetWallet(ctx, fixture.user, "USD")
	if err != nil {
		t.Fatalf("GetWallet() error: %v", err)
	}
	if wallet.AvailableBalanceMinor != 22 || wallet.ReservedBalanceMinor != 0 {
		t.Fatalf("wallet after refund/failed debit = %+v", wallet)
	}
	if count := billingCount(t, store, `SELECT count(*) FROM wallet_ledger WHERE wallet_id = $1`, wallet.ID); count != 3 {
		t.Fatalf("ledger count after refund/failed debit = %d, want 3", count)
	}
}

func billingCount(t *testing.T, store *data.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.Queryer().QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("query billing count: %v", err)
	}
	return count
}

func postgresCode(err error) string {
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		return databaseError.Code
	}
	return ""
}
