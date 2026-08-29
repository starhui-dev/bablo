package apikey

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryLimiterConcurrentRPMAndTPM(t *testing.T) {
	limiter := NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })
	keyID := uuid.New()
	now := time.Date(2026, 8, 30, 12, 34, 20, 0, time.UTC)
	rpm := int64(10)
	tpm := int64(50)

	var allowed atomic.Int64
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := limiter.Allow(context.Background(), keyID, &rpm, &tpm, 3, now)
			switch {
			case err == nil:
				allowed.Add(1)
			case errors.Is(err, ErrRateLimited):
			default:
				t.Errorf("Allow() error = %v", err)
			}
		}()
	}
	wait.Wait()
	if got := allowed.Load(); got != rpm {
		t.Fatalf("allowed requests = %d, want %d", got, rpm)
	}
	if err := limiter.Allow(context.Background(), uuid.New(), nil, nil, -1, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative token error = %v, want ErrInvalidInput", err)
	}
}

func TestRedisLimiterRejectsMalformedURLWithoutEchoingSecret(t *testing.T) {
	_, err := NewRedisLimiter(context.Background(), "redis://user:top-secret@[::1")
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("malformed Redis URL error = %q", err)
	}
}

func TestRedisLimiterConcurrentAtomicWindow(t *testing.T) {
	rawURL := os.Getenv("BABLO_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("BABLO_TEST_REDIS_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	limiter, err := NewRedisLimiter(ctx, rawURL)
	if err != nil {
		t.Fatalf("NewRedisLimiter() error = %v", err)
	}
	t.Cleanup(func() { _ = limiter.Close() })

	keyID := uuid.New()
	now := time.Now().UTC()
	rpm := int64(25)
	var allowed atomic.Int64
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := limiter.Allow(context.Background(), keyID, &rpm, nil, 0, now)
			switch {
			case err == nil:
				allowed.Add(1)
			case errors.Is(err, ErrRateLimited):
			default:
				t.Errorf("Allow() error = %v", err)
			}
		}()
	}
	wait.Wait()
	if got := allowed.Load(); got != rpm {
		t.Fatalf("Redis allowed requests = %d, want %d", got, rpm)
	}
	if err := limiter.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := limiter.Allow(context.Background(), uuid.New(), &rpm, nil, 0, now); !errors.Is(err, ErrRateLimitUnavailable) {
		t.Fatalf("closed Redis Allow() error = %v, want ErrRateLimitUnavailable", err)
	}
}
