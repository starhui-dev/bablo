package payment

import (
	"context"
	"log/slog"
	"time"
)

const paymentMaintenanceBatchSize = 100

// RunExpirationWorker first resumes leased provider operations, then closes
// due orders. Provider calls use stable Bablo-derived idempotency keys, so
// concurrent instances and crash retries remain safe.
func (s *Service) RunExpirationWorker(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if s == nil || interval <= 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	run := func() {
		if ctx.Err() != nil {
			return
		}
		recovered, err := s.RecoverProviderOperations(ctx, paymentMaintenanceBatchSize)
		if err != nil {
			logger.Error("payment_provider_recovery_error", "error", err)
		}
		if recovered > 0 {
			logger.Info("payment_provider_operations_recovered", "count", recovered)
		}
		reconciled, err := s.ReconcileOrders(ctx, paymentMaintenanceBatchSize)
		if err != nil {
			logger.Error("payment_reconciliation_error", "error", err)
		}
		if reconciled > 0 {
			logger.Info("payment_orders_reconciled", "count", reconciled)
		}
		count, err := s.ExpireDue(ctx, paymentMaintenanceBatchSize)
		if err != nil {
			logger.Error("payment_expiration_error", "error", err)
		}
		if count > 0 {
			logger.Info("payment_orders_expired", "count", count)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
