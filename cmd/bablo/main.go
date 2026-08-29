package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/starhui-dev/bablo/internal/config"
	"github.com/starhui-dev/bablo/internal/httpapi"
)

var buildVersion = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("bablo_config_error", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	server := httpapi.New(cfg, logger, buildVersion)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("bablo_shutdown_error", "error", err)
			os.Exit(1)
		}
		logger.Info("bablo_http_stopped")
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("bablo_http_error", "error", err)
			os.Exit(1)
		}
	}
}
