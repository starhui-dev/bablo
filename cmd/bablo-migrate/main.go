// Command bablo-migrate applies or rolls back Bablo PostgreSQL migrations.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("BABLO_DATABASE_URL")
	if databaseURL == "" {
		logger.Error("bablo_migration_config_error", "error", "BABLO_DATABASE_URL is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Getenv("BABLO_MIGRATION_ACTION") {
	case "", "up":
		err = data.Migrate(ctx, databaseURL, migrations.Files, logger)
	case "down":
		err = data.MigrateDown(ctx, databaseURL, migrations.Files, logger)
	default:
		err = errors.New("BABLO_MIGRATION_ACTION must be up or down")
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		logger.Error("bablo_migration_error", "error", err)
		os.Exit(1)
	}
	logger.Info("bablo_migrations_completed", "action", actionName())
}

func actionName() string {
	if action := os.Getenv("BABLO_MIGRATION_ACTION"); action != "" {
		return action
	}
	return "up"
}
