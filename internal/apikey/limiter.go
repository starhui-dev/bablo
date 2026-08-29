package apikey

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const maxMemoryWindows = 100_000

// Limiter enforces request and token counters for one UTC minute window.
type Limiter interface {
	Allow(context.Context, uuid.UUID, *int64, *int64, int64, time.Time) error
	Close() error
}

type windowID struct {
	keyID  uuid.UUID
	minute int64
}

type windowCount struct {
	requests int64
	tokens   int64
}

type memoryLimiter struct {
	mu      sync.Mutex
	windows map[windowID]windowCount
}

// NewMemoryLimiter returns the bounded single-instance P0 limiter.
func NewMemoryLimiter() Limiter {
	return &memoryLimiter{windows: make(map[windowID]windowCount)}
}

func (l *memoryLimiter) Allow(_ context.Context, keyID uuid.UUID, rpm, tpm *int64, tokens int64, now time.Time) error {
	if tokens < 0 {
		return ErrInvalidInput
	}
	if rpm == nil && tpm == nil {
		return nil
	}
	minute := now.UTC().Truncate(time.Minute).Unix()
	id := windowID{keyID: keyID, minute: minute}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.windows[id]; !exists && len(l.windows) >= maxMemoryWindows {
		for candidate := range l.windows {
			if candidate.minute != minute {
				delete(l.windows, candidate)
			}
		}
		if len(l.windows) >= maxMemoryWindows {
			return ErrRateLimitUnavailable
		}
	}
	count := l.windows[id]
	count.requests++
	count.tokens += tokens
	l.windows[id] = count
	if rpm != nil && count.requests > *rpm {
		return ErrRateLimited
	}
	if tpm != nil && count.tokens > *tpm {
		return ErrRateLimited
	}
	return nil
}

func (l *memoryLimiter) Close() error { return nil }

type redisLimiter struct {
	client *redis.Client
}

const rateLimitScript = `
local requests = redis.call('HINCRBY', KEYS[1], 'requests', 1)
local tokens = redis.call('HINCRBY', KEYS[1], 'tokens', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return {requests, tokens}
`

// NewRedisLimiter connects to Redis and verifies the configured dependency.
func NewRedisLimiter(ctx context.Context, rawURL string) (Limiter, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("parse Redis URL: invalid configuration")
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping Redis: %w", err)
	}
	return &redisLimiter{client: client}, nil
}

func (l *redisLimiter) Allow(ctx context.Context, keyID uuid.UUID, rpm, tpm *int64, tokens int64, now time.Time) error {
	if tokens < 0 {
		return ErrInvalidInput
	}
	if rpm == nil && tpm == nil {
		return nil
	}
	minute := now.UTC().Truncate(time.Minute)
	ttl := minute.Add(time.Minute).Sub(now.UTC()) + time.Second
	if ttl < time.Second {
		ttl = time.Second
	}
	key := "bablo:apikey:rate:" + keyID.String() + ":" + strconv.FormatInt(minute.Unix(), 10)
	result, err := l.client.Eval(ctx, rateLimitScript, []string{key}, tokens, ttl.Milliseconds()).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRateLimitUnavailable, err)
	}
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return ErrRateLimitUnavailable
	}
	requests, okRequests := values[0].(int64)
	usedTokens, okTokens := values[1].(int64)
	if !okRequests || !okTokens {
		return ErrRateLimitUnavailable
	}
	if rpm != nil && requests > *rpm {
		return ErrRateLimited
	}
	if tpm != nil && usedTokens > *tpm {
		return ErrRateLimited
	}
	return nil
}

func (l *redisLimiter) Close() error {
	if l == nil || l.client == nil {
		return nil
	}
	return l.client.Close()
}
