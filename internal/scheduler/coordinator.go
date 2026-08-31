package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	leaseKeyPrefix    = "bablo:scheduler:lease:"
	cursorKeyPrefix   = "bablo:scheduler:cursor:"
	affinityKeyPrefix = "bablo:scheduler:affinity:"
)

// CoordinatorOptions controls transient scheduler state TTLs.
type CoordinatorOptions struct {
	LeaseTTL    time.Duration
	AffinityTTL time.Duration
	CursorTTL   time.Duration
}

func (o CoordinatorOptions) withDefaults() (CoordinatorOptions, error) {
	if o.LeaseTTL == 0 {
		o.LeaseTTL = defaultLeaseTTL
	}
	if o.AffinityTTL == 0 {
		o.AffinityTTL = defaultAffinityTTL
	}
	if o.CursorTTL == 0 {
		o.CursorTTL = defaultCursorTTL
	}
	if o.LeaseTTL <= 0 || o.AffinityTTL <= 0 || o.CursorTTL <= 0 {
		return CoordinatorOptions{}, ErrInvalidInput
	}
	return o, nil
}

type memoryCoordinator struct {
	mu         sync.Mutex
	leases     map[string]memoryLeaseEntry
	cursors    map[string]memoryCursorEntry
	affinities map[string]memoryAffinityEntry
}

type memoryLeaseEntry struct {
	owner     string
	expiresAt time.Time
}

type memoryCursorEntry struct {
	value     int64
	expiresAt time.Time
}

type memoryAffinityEntry struct {
	credentialID uuid.UUID
	expiresAt    time.Time
}

// NewMemoryCoordinator returns a bounded-process coordinator for P0 single
// instance use and deterministic tests. All state is transient and expires.
func NewMemoryCoordinator() Coordinator {
	return &memoryCoordinator{
		leases:     make(map[string]memoryLeaseEntry),
		cursors:    make(map[string]memoryCursorEntry),
		affinities: make(map[string]memoryAffinityEntry),
	}
}

func (c *memoryCoordinator) Acquire(ctx context.Context, resource, owner string, ttl time.Duration) (Lease, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if resource == "" || owner == "" || ttl <= 0 {
		return nil, ErrInvalidInput
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	if current, ok := c.leases[resource]; ok && current.expiresAt.After(now) {
		return nil, ErrLeaseBusy
	}
	c.leases[resource] = memoryLeaseEntry{owner: owner, expiresAt: now.Add(ttl)}
	return &memoryLease{coordinator: c, resource: resource, owner: owner}, nil
}

func (c *memoryCoordinator) Next(ctx context.Context, key string, modulo int64, ttl time.Duration) (int64, error) {
	if err := contextErr(ctx); err != nil {
		return 0, err
	}
	if key == "" || modulo <= 0 || ttl <= 0 {
		return 0, ErrInvalidInput
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	entry := c.cursors[key]
	value := entry.value
	entry.value++
	entry.expiresAt = now.Add(ttl)
	c.cursors[key] = entry
	return value % modulo, nil
}

func (c *memoryCoordinator) GetAffinity(ctx context.Context, key string) (uuid.UUID, bool, error) {
	if err := contextErr(ctx); err != nil {
		return uuid.Nil, false, err
	}
	if key == "" {
		return uuid.Nil, false, ErrInvalidInput
	}
	now := time.Now()
	key = affinityStateKey(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	entry, ok := c.affinities[key]
	if !ok || !entry.expiresAt.After(now) {
		return uuid.Nil, false, nil
	}
	return entry.credentialID, true, nil
}

func (c *memoryCoordinator) SetAffinity(ctx context.Context, key string, credentialID uuid.UUID, ttl time.Duration) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if key == "" || credentialID == uuid.Nil || ttl <= 0 {
		return ErrInvalidInput
	}
	now := time.Now()
	key = affinityStateKey(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanup(now)
	c.affinities[key] = memoryAffinityEntry{credentialID: credentialID, expiresAt: now.Add(ttl)}
	return nil
}

func (c *memoryCoordinator) Close() error { return nil }

func (c *memoryCoordinator) cleanup(now time.Time) {
	for key, value := range c.leases {
		if !value.expiresAt.After(now) {
			delete(c.leases, key)
		}
	}
	for key, value := range c.cursors {
		if !value.expiresAt.After(now) {
			delete(c.cursors, key)
		}
	}
	for key, value := range c.affinities {
		if !value.expiresAt.After(now) {
			delete(c.affinities, key)
		}
	}
}
func (l *memoryLease) Renew(ctx context.Context, ttl time.Duration) error {
	if l == nil || l.coordinator == nil || ttl <= 0 {
		return ErrInvalidInput
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	l.coordinator.mu.Lock()
	defer l.coordinator.mu.Unlock()
	entry, ok := l.coordinator.leases[l.resource]
	if !ok || !entry.expiresAt.After(time.Now()) || entry.owner != l.owner {
		return ErrLeaseNotOwner
	}
	entry.expiresAt = time.Now().Add(ttl)
	l.coordinator.leases[l.resource] = entry
	return nil
}

type memoryLease struct {
	coordinator *memoryCoordinator
	resource    string
	owner       string
	once        sync.Once
	err         error
}

func (l *memoryLease) Release(ctx context.Context) error {
	if l == nil || l.coordinator == nil {
		return nil
	}
	l.once.Do(func() {
		if err := contextErr(ctx); err != nil {
			l.err = err
			return
		}
		l.coordinator.mu.Lock()
		defer l.coordinator.mu.Unlock()
		entry, ok := l.coordinator.leases[l.resource]
		if !ok || !entry.expiresAt.After(time.Now()) {
			delete(l.coordinator.leases, l.resource)
			return
		}
		if entry.owner != l.owner {
			l.err = ErrLeaseNotOwner
			return
		}
		delete(l.coordinator.leases, l.resource)
	})
	return l.err
}

// RedisCoordinator stores leases, cursors, and affinity in Redis with explicit
// TTLs. Lease release is owner-token protected and safe against stale owners.
type redisCoordinator struct {
	client  *redis.Client
	options CoordinatorOptions
	release *redis.Script
	renew   *redis.Script
	next    *redis.Script
}

const releaseLeaseLua = `
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

const renewLeaseLua = `
local value = redis.call('GET', KEYS[1])
if not value or value ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`

const nextCursorLua = `
local value = redis.call('INCR', KEYS[1]) - 1
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return value
`

// NewRedisCoordinator connects to Redis and verifies the dependency before
// returning. Raw URLs are never included in returned errors.
func NewRedisCoordinator(ctx context.Context, rawURL string, options CoordinatorOptions) (Coordinator, error) {
	options, err := options.withDefaults()
	if err != nil {
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
	return &redisCoordinator{
		client:  client,
		options: options,
		release: redis.NewScript(releaseLeaseLua),
		next:    redis.NewScript(nextCursorLua),
		renew:   redis.NewScript(renewLeaseLua),
	}, nil
}

func (c *redisCoordinator) Acquire(ctx context.Context, resource, owner string, ttl time.Duration) (Lease, error) {
	if resource == "" || owner == "" || ttl <= 0 {
		return nil, ErrInvalidInput
	}
	if c == nil || c.client == nil {
		return nil, ErrStateUnavailable
	}
	ok, err := c.client.SetNX(ctx, leaseKeyPrefix+resource, owner, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("%w: acquire lease", ErrStateUnavailable)
	}
	if !ok {
		return nil, ErrLeaseBusy
	}
	return &redisLease{coordinator: c, key: leaseKeyPrefix + resource, owner: owner}, nil
}

func (c *redisCoordinator) Next(ctx context.Context, key string, modulo int64, ttl time.Duration) (int64, error) {
	if key == "" || modulo <= 0 || ttl <= 0 {
		return 0, ErrInvalidInput
	}
	if c == nil || c.client == nil {
		return 0, ErrStateUnavailable
	}
	value, err := c.next.Run(ctx, c.client, []string{cursorKeyPrefix + key}, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("%w: advance cursor", ErrStateUnavailable)
	}
	return value % modulo, nil
}

func (c *redisCoordinator) GetAffinity(ctx context.Context, key string) (uuid.UUID, bool, error) {
	if key == "" {
		return uuid.Nil, false, ErrInvalidInput
	}
	if c == nil || c.client == nil {
		return uuid.Nil, false, ErrStateUnavailable
	}
	value, err := c.client.Get(ctx, affinityStateKey(key)).Result()
	if errors.Is(err, redis.Nil) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("%w: read affinity", ErrStateUnavailable)
	}
	credentialID, err := uuid.Parse(value)
	if err != nil || credentialID == uuid.Nil {
		return uuid.Nil, false, nil
	}
	return credentialID, true, nil
}

func (c *redisCoordinator) SetAffinity(ctx context.Context, key string, credentialID uuid.UUID, ttl time.Duration) error {
	if key == "" || credentialID == uuid.Nil || ttl <= 0 {
		return ErrInvalidInput
	}
	if c == nil || c.client == nil {
		return ErrStateUnavailable
	}
	if err := c.client.Set(ctx, affinityStateKey(key), credentialID.String(), ttl).Err(); err != nil {
		return fmt.Errorf("%w: write affinity", ErrStateUnavailable)
	}
	return nil
}

func (c *redisCoordinator) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

type redisLease struct {
	coordinator *redisCoordinator
	key         string
	owner       string
	once        sync.Once
	err         error
}

func (l *redisLease) Renew(ctx context.Context, ttl time.Duration) error {
	if l == nil || l.coordinator == nil || l.coordinator.client == nil || ttl <= 0 {
		return ErrInvalidInput
	}
	value, err := l.coordinator.renew.Run(ctx, l.coordinator.client, []string{l.key}, l.owner, ttl.Milliseconds()).Int64()
	if err != nil {
		return fmt.Errorf("%w: renew lease", ErrStateUnavailable)
	}
	if value != 1 {
		return ErrLeaseNotOwner
	}
	return nil
}

func (l *redisLease) Release(ctx context.Context) error {
	if l == nil || l.coordinator == nil || l.coordinator.client == nil {
		return nil
	}
	l.once.Do(func() {
		value, err := l.coordinator.release.Run(ctx, l.coordinator.client, []string{l.key}, l.owner).Int64()
		if err != nil {
			l.err = fmt.Errorf("%w: release lease", ErrStateUnavailable)
			return
		}
		switch value {
		case -1, 1:
			return
		default:
			l.err = ErrLeaseNotOwner
		}
	})
	return l.err
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func affinityStateKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return affinityKeyPrefix + hex.EncodeToString(digest[:])
}
