// Package data owns Bablo's PostgreSQL access primitives.
package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config controls the PostgreSQL connection pool. Zero values use conservative
// single-instance defaults; deployment-specific limits belong in configuration.
type Config struct {
	URL               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// Querier is the smallest database surface repositories need. Both pgxpool.Pool
// and pgx.Tx implement it, which keeps transaction ownership outside handlers.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store owns the pool and provides the repository transaction boundary.
type Store struct {
	pool *pgxpool.Pool
}

// Open parses the PostgreSQL URL, opens the pool, and performs a real ping.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, errors.New("database URL is required")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 8
	}
	if cfg.MinConns < 0 {
		return nil, errors.New("database min connections must not be negative")
	}
	if cfg.MinConns > cfg.MaxConns {
		return nil, errors.New("database min connections must not exceed max connections")
	}
	if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = 30 * time.Minute
	}
	if cfg.MaxConnIdleTime == 0 {
		cfg.MaxConnIdleTime = 5 * time.Minute
	}
	if cfg.HealthCheckPeriod == 0 {
		cfg.HealthCheckPeriod = time.Minute
	}
	if cfg.MaxConnLifetime < 0 || cfg.MaxConnIdleTime < 0 || cfg.HealthCheckPeriod < 0 {
		return nil, errors.New("database pool durations must not be negative")
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolConfig.ConnConfig.RuntimeParams["timezone"] = "UTC"
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "bablo"
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Queryer returns the pool for repository reads and non-transactional work.
// Callers must not use it from HTTP handlers directly; repositories own SQL.
func (s *Store) Queryer() Querier {
	if s == nil {
		return nil
	}
	return s.pool
}

// WithTx runs fn in a PostgreSQL transaction and commits only when fn succeeds.
// Rollback is attempted on every non-commit path; PostgreSQL owns transaction
// atomicity, while repositories own the statements inside the callback.
func (s *Store) WithTx(ctx context.Context, fn func(Querier) error) error {
	if fn == nil {
		return errors.New("transaction callback is required")
	}
	if s == nil || s.pool == nil {
		return errors.New("database store is not initialized")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Ping verifies that the pool can reach PostgreSQL.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("database store is not initialized")
	}
	return s.pool.Ping(ctx)
}

// Close releases all PostgreSQL connections. It is safe to call on a nil store.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}
