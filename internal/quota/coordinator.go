package quota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const probeLeasePrefix = "bablo:quota:probe:"

const releaseProbeLeaseLua = `
local value = redis.call('GET', KEYS[1])
if not value then
  return -1
end
if value ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1
`

// NewMemoryLocker returns a bounded single-process locker for P0 and tests.
// Entries are transient and expire automatically.
func NewMemoryLocker() Locker {
	return &memoryLocker{leases: make(map[string]memoryLeaseEntry)}
}

type memoryLocker struct {
	mu     sync.Mutex
	leases map[string]memoryLeaseEntry
}

type memoryLeaseEntry struct {
	owner     string
	expiresAt time.Time
}

func (l *memoryLocker) Acquire(ctx context.Context, resource, owner string, ttl time.Duration) (Lease, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil || resource == "" || owner == "" || ttl <= 0 {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)
	if current, ok := l.leases[resource]; ok && current.expiresAt.After(now) {
		return nil, ErrProbeBusy
	}
	l.leases[resource] = memoryLeaseEntry{owner: owner, expiresAt: now.Add(ttl)}
	return &memoryLease{locker: l, resource: resource, owner: owner}, nil
}

func (l *memoryLocker) cleanupLocked(now time.Time) {
	for resource, entry := range l.leases {
		if !entry.expiresAt.After(now) {
			delete(l.leases, resource)
		}
	}
}

func (l *memoryLocker) Close() error { return nil }

type memoryLease struct {
	locker   *memoryLocker
	resource string
	owner    string
	once     sync.Once
	err      error
}

func (l *memoryLease) Release(ctx context.Context) error {
	if l == nil || l.locker == nil {
		return nil
	}
	l.once.Do(func() {
		if err := contextError(ctx); err != nil {
			l.err = err
			return
		}
		l.locker.mu.Lock()
		defer l.locker.mu.Unlock()
		entry, ok := l.locker.leases[l.resource]
		if !ok || !entry.expiresAt.After(time.Now().UTC()) {
			delete(l.locker.leases, l.resource)
			return
		}
		if entry.owner != l.owner {
			l.err = ErrStateUnavailable
			return
		}
		delete(l.locker.leases, l.resource)
	})
	return l.err
}

// NewRedisLocker creates a Redis-backed probe locker and verifies connectivity.
// Redis stores only the rebuildable lease, never quota facts.
func NewRedisLocker(ctx context.Context, rawURL string) (Locker, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	parsed, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Redis configuration", ErrStateUnavailable)
	}
	client := redis.NewClient(parsed)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("%w: Redis probe failed", ErrStateUnavailable)
	}
	return &redisLocker{client: client, release: redis.NewScript(releaseProbeLeaseLua)}, nil
}

type redisLocker struct {
	client  *redis.Client
	release *redis.Script
}

func (l *redisLocker) Acquire(ctx context.Context, resource, owner string, ttl time.Duration) (Lease, error) {
	if resource == "" || owner == "" || ttl <= 0 {
		return nil, ErrInvalidInput
	}
	if l == nil || l.client == nil {
		return nil, ErrStateUnavailable
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	ok, err := l.client.SetNX(ctx, probeLeasePrefix+resource, owner, ttl).Result()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: acquire probe lease", ErrStateUnavailable)
	}
	if !ok {
		return nil, ErrProbeBusy
	}
	return &redisLease{locker: l, key: probeLeasePrefix + resource, owner: owner}, nil
}

func (l *redisLocker) Close() error {
	if l == nil || l.client == nil {
		return nil
	}
	return l.client.Close()
}

type redisLease struct {
	locker *redisLocker
	key    string
	owner  string
	once   sync.Once
	err    error
}

func (l *redisLease) Release(ctx context.Context) error {
	if l == nil || l.locker == nil || l.locker.client == nil {
		return nil
	}
	l.once.Do(func() {
		if err := contextError(ctx); err != nil {
			l.err = err
			return
		}
		value, err := l.locker.release.Run(ctx, l.locker.client, []string{l.key}, l.owner).Int()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				l.err = err
			} else {
				l.err = fmt.Errorf("%w: release probe lease", ErrStateUnavailable)
			}
			return
		}
		if value == 0 {
			l.err = ErrStateUnavailable
		}
	})
	return l.err
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
