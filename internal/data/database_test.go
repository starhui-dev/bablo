package data

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/starhui-dev/bablo/migrations"
)

func TestOpenRejectsInvalidPoolBounds(t *testing.T) {
	_, err := Open(context.Background(), Config{
		URL:      "postgres://invalid.example/bablo",
		MaxConns: 1,
		MinConns: 2,
	})
	if err == nil || err.Error() != "database min connections must not exceed max connections" {
		t.Fatalf("Open() error = %v", err)
	}
}

func TestOpenRejectsMissingURL(t *testing.T) {
	_, err := Open(context.Background(), Config{})
	if err == nil || err.Error() != "database URL is required" {
		t.Fatalf("Open() error = %v", err)
	}
}

func TestWithTxRequiresInitializedStore(t *testing.T) {
	var store *Store
	err := store.WithTx(context.Background(), func(Querier) error { return nil })
	if err == nil || err.Error() != "database store is not initialized" {
		t.Fatalf("WithTx() error = %v", err)
	}
}

func TestWithTxRequiresCallback(t *testing.T) {
	store := &Store{}
	err := store.WithTx(context.Background(), nil)
	if err == nil || err.Error() != "transaction callback is required" {
		t.Fatalf("WithTx() error = %v", err)
	}
}

func TestWithTxRollbackAndCommit(t *testing.T) {
	url := testDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := Migrate(ctx, url, migrations.Files, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	defer cancel()

	store, err := Open(ctx, Config{URL: url, MaxConns: 2})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	roleID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	rollbackErr := errors.New("force rollback")
	err = store.WithTx(ctx, func(q Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO roles (id, name) VALUES ($1, $2)`, roleID, "tx-rollback-"+roleID.String())
		if err != nil {
			return err
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("WithTx() rollback error = %v", err)
	}

	var count int
	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FROM roles WHERE id = $1`, roleID).Scan(&count); err != nil {
		t.Fatalf("rollback query error = %v", err)
	}
	if count != 0 {
		t.Fatalf("rollback left %d role rows", count)
	}

	if err := store.WithTx(ctx, func(q Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO roles (id, name) VALUES ($1, $2)`, roleID, "tx-commit-"+roleID.String())
		return err
	}); err != nil {
		t.Fatalf("WithTx() commit error = %v", err)
	}
	defer func() {
		_, _ = store.Queryer().Exec(ctx, `DELETE FROM roles WHERE id = $1`, roleID)
	}()

	if err := store.Queryer().QueryRow(ctx, `SELECT count(*) FROM roles WHERE id = $1`, roleID).Scan(&count); err != nil {
		t.Fatalf("commit query error = %v", err)
	}
	if count != 1 {
		t.Fatalf("commit left %d role rows", count)
	}
}
