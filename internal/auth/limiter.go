package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultAttemptEntries = 10_000

type loginLimitEntry struct {
	attempts int
	resetAt  time.Time
}

// AttemptLimiter limits both one account and one client address. Implementations
// hash identities before storing keys and fail closed when their backend fails.
type AttemptLimiter interface {
	Allow(context.Context, string, string, time.Time) bool
	Close() error
}

// LoginLimiter is the bounded single-instance implementation used when Redis
// is intentionally absent.
type LoginLimiter struct {
	mu           sync.Mutex
	entries      map[string]loginLimitEntry
	accountLimit int
	ipLimit      int
	window       time.Duration
	maxEntries   int
}

// NewMemoryAttemptLimiter constructs the bounded single-instance limiter.
func NewMemoryAttemptLimiter(accountLimit, ipLimit int, window time.Duration, maxEntries int) *LoginLimiter {
	if accountLimit <= 0 {
		accountLimit = 8
	}
	if ipLimit <= 0 {
		ipLimit = accountLimit * 5
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	if maxEntries < 2 {
		maxEntries = defaultAttemptEntries
	}
	return &LoginLimiter{
		entries: make(map[string]loginLimitEntry), accountLimit: accountLimit,
		ipLimit: ipLimit, window: window, maxEntries: maxEntries,
	}
}

func (l *LoginLimiter) Allow(_ context.Context, email, remoteAddr string, now time.Time) bool {
	if l == nil {
		return false
	}
	keys := attemptKeys(email, remoteAddr)
	limits := [...]int{l.accountLimit, l.ipLimit}
	now = now.UTC()
	resetAt := now.Truncate(l.window).Add(l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		if entry, ok := l.entries[key]; ok && !now.Before(entry.resetAt) {
			delete(l.entries, key)
		}
	}
	newEntries := 0
	for _, key := range keys {
		if _, exists := l.entries[key]; !exists {
			newEntries++
		}
	}
	for len(l.entries)+newEntries > l.maxEntries {
		l.pruneExpired(now)
		if len(l.entries)+newEntries <= l.maxEntries {
			break
		}
		l.evictEarliestReset()
		if len(l.entries) == 0 {
			break
		}
	}
	allowed := true
	for index, key := range keys {
		entry := l.entries[key]
		if entry.resetAt.IsZero() {
			entry.resetAt = resetAt
		}
		entry.attempts++
		l.entries[key] = entry
		if entry.attempts > limits[index] {
			allowed = false
		}
	}
	return allowed
}

func (l *LoginLimiter) Close() error { return nil }

func (l *LoginLimiter) pruneExpired(now time.Time) {
	for key, entry := range l.entries {
		if !now.Before(entry.resetAt) {
			delete(l.entries, key)
		}
	}
}

func (l *LoginLimiter) evictEarliestReset() {
	var oldestKey string
	var oldestReset time.Time
	for key, entry := range l.entries {
		if oldestKey == "" || entry.resetAt.Before(oldestReset) {
			oldestKey = key
			oldestReset = entry.resetAt
		}
	}
	if oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

type redisAttemptLimiter struct {
	client       *redis.Client
	namespace    string
	accountLimit int64
	ipLimit      int64
	window       time.Duration
}

const authAttemptScript = `
local account = redis.call('INCR', KEYS[1])
if account == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local address = redis.call('INCR', KEYS[2])
if address == 1 then redis.call('PEXPIRE', KEYS[2], ARGV[1]) end
return {account, address}
`

// NewRedisAttemptLimiter creates the HA-safe authentication limiter and probes
// Redis before returning it to the process bootstrap.
func NewRedisAttemptLimiter(ctx context.Context, rawURL, namespace string, accountLimit, ipLimit int, window time.Duration) (AttemptLimiter, error) {
	namespace = strings.TrimSpace(namespace)
	if !validAttemptNamespace(namespace) || accountLimit <= 0 || ipLimit <= 0 || window < time.Second {
		return nil, errors.New("invalid authentication limiter configuration")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("parse Redis URL: invalid configuration")
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping Redis for authentication limiter: %w", err)
	}
	return &redisAttemptLimiter{
		client: client, namespace: namespace, accountLimit: int64(accountLimit),
		ipLimit: int64(ipLimit), window: window,
	}, nil
}

func (l *redisAttemptLimiter) Allow(ctx context.Context, email, remoteAddr string, now time.Time) bool {
	if l == nil || l.client == nil {
		return false
	}
	windowMillis := l.window.Milliseconds()
	windowStart := now.UTC().UnixMilli() / windowMillis * windowMillis
	ttl := windowStart + windowMillis - now.UTC().UnixMilli() + int64(time.Second/time.Millisecond)
	keys := attemptKeys(email, remoteAddr)
	accountKey := "bablo:auth:attempt:" + l.namespace + ":account:" + keys[0] + ":" + strconv.FormatInt(windowStart, 10)
	addressKey := "bablo:auth:attempt:" + l.namespace + ":address:" + keys[1] + ":" + strconv.FormatInt(windowStart, 10)
	result, err := l.client.Eval(ctx, authAttemptScript, []string{accountKey, addressKey}, ttl).Result()
	if err != nil {
		return false
	}
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return false
	}
	accountAttempts, accountOK := values[0].(int64)
	addressAttempts, addressOK := values[1].(int64)
	return accountOK && addressOK && accountAttempts <= l.accountLimit && addressAttempts <= l.ipLimit
}

func (l *redisAttemptLimiter) Close() error {
	if l == nil || l.client == nil {
		return nil
	}
	return l.client.Close()
}

func attemptKeys(email, remoteAddr string) [2]string {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	normalizedAddress := strings.TrimSpace(remoteAddr)
	if normalizedAddress == "" {
		normalizedAddress = "unknown"
	}
	return [2]string{attemptIdentityKey(normalizedEmail), attemptIdentityKey(normalizedAddress)}
}

func attemptIdentityKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validAttemptNamespace(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}
