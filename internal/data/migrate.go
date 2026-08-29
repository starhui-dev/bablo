package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migrate applies every pending SQL migration. Goose's PostgreSQL session-level
// advisory lock makes concurrent deploy invocations serialize without adding a
// Bablo-specific lock table.
func Migrate(ctx context.Context, databaseURL string, migrations fs.FS, logger *slog.Logger) error {
	provider, err := newMigrationProvider(databaseURL, migrations, logger)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back exactly the latest applied migration. It is kept
// explicit so ordinary application startup cannot accidentally mutate schema.
func MigrateDown(ctx context.Context, databaseURL string, migrations fs.FS, logger *slog.Logger) error {
	provider, err := newMigrationProvider(databaseURL, migrations, logger)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()

	if _, err := provider.Down(ctx); err != nil {
		return fmt.Errorf("rollback migration: %w", err)
	}
	return nil
}

func newMigrationProvider(databaseURL string, migrations fs.FS, logger *slog.Logger) (*goose.Provider, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database URL is required")
	}
	if migrations == nil {
		return nil, errors.New("migration filesystem is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open migration database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	sessionLocker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration lock: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations,
		goose.WithSessionLocker(sessionLocker),
		goose.WithSlog(logger),
	)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return provider, nil
}
