package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type loginLimitEntry struct {
	attempts int
	resetAt  time.Time
}

// LoginLimiter is a bounded single-instance limiter. It never creates a
// permanent account lock; Redis coordination is added with the HA stage.
type LoginLimiter struct {
	mu         sync.Mutex
	entries    map[string]loginLimitEntry
	limit      int
	window     time.Duration
	maxEntries int
}

// NewLoginLimiter constructs a bounded fixed-window login limiter.
func NewLoginLimiter(limit int, window time.Duration, maxEntries int) *LoginLimiter {
	if limit <= 0 {
		limit = 8
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = 10_000
	}
	return &LoginLimiter{
		entries:    make(map[string]loginLimitEntry),
		limit:      limit,
		window:     window,
		maxEntries: maxEntries,
	}
}

func (l *LoginLimiter) allow(email, remoteAddr string, now time.Time) bool {
	if l == nil {
		return true
	}
	key := loginLimitKey(email, remoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[key]
	if ok && !now.Before(entry.resetAt) {
		delete(l.entries, key)
		ok = false
	}
	if !ok {
		if len(l.entries) >= l.maxEntries {
			l.pruneExpired(now)
			if len(l.entries) >= l.maxEntries {
				l.evictEarliestReset()
			}
		}
		entry = loginLimitEntry{resetAt: now.Add(l.window)}
	}
	if entry.attempts >= l.limit {
		return false
	}
	entry.attempts++
	l.entries[key] = entry
	return true
}

func (l *LoginLimiter) reset(email, remoteAddr string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.entries, loginLimitKey(email, remoteAddr))
	l.mu.Unlock()
}

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

func loginLimitKey(email, remoteAddr string) string {
	digest := sha256.Sum256([]byte(email + "\x00" + remoteAddr))
	return hex.EncodeToString(digest[:])
}
