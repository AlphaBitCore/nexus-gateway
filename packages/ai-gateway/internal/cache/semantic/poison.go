package semantic

// poison.go — negative-feedback poison list for the L2 semantic cache.
//
// When an admin marks a semantic cache hit as bad via
// POST /api/admin/cache/semantic-feedback, the Control Plane calls
// PoisonList.Add(ctx, entryKey, vkScope, ttl). The AI Gateway Reader checks
// IsPoisoned(ctx, entryKey, vkScope) after every FT.SEARCH hit and treats
// poisoned entries as misses.
//
// Storage: Redis SET keys under the namespace
//
//	nexus:l2:poison:<vkScope>:<entryKey>
//
// TTL = 10× the original entry's hit TTL, capped at 30 days.

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// poisonKeyPrefix is the Redis key namespace for poisoned entry markers.
	poisonKeyPrefix = "nexus:l2:poison:"

	// poisonTTLMultiplier is how many times the original entry TTL we keep the
	// poison marker alive. 10× means a 24-hour entry is blocked for 10 days.
	poisonTTLMultiplier = 10

	// poisonMaxTTL is the absolute cap on poison TTL to prevent unbounded growth.
	poisonMaxTTL = 30 * 24 * time.Hour
)

// PoisonList is the interface for the negative-feedback poison list.
// Both the Gateway reader (IsPoisoned) and the Control Plane feedback handler
// (Add) depend on this interface; the Redis-backed impl satisfies both.
type PoisonList interface {
	// IsPoisoned returns true when the given entryKey/vkScope pair has been
	// marked as a bad cache hit by an admin. A true result causes Reader.Read
	// to treat the candidate as a miss and stamp
	// GatewayCacheSkipReasonPoisoned.
	IsPoisoned(ctx context.Context, entryKey, vkScope string) (bool, error)

	// Add marks an entry as poisoned. ttl is the remaining TTL of the original
	// entry (obtained from PTTL); the poison marker lives for 10× that value,
	// capped at 30 days. When ttl ≤ 0, a default of 24 h × poisonTTLMultiplier
	// is used.
	Add(ctx context.Context, entryKey, vkScope string, ttl time.Duration) error
}

// RedisPoisonList is the Redis-backed implementation of PoisonList.
// Thread-safe; all methods may be called concurrently.
type RedisPoisonList struct {
	rdb redis.UniversalClient
}

// NewRedisPoisonList constructs a RedisPoisonList backed by the given client.
// rdb must not be nil.
func NewRedisPoisonList(rdb redis.UniversalClient) *RedisPoisonList {
	return &RedisPoisonList{rdb: rdb}
}

// poisonKey returns the Redis key for the given entry/scope pair.
func poisonKey(entryKey, vkScope string) string {
	return fmt.Sprintf("%s%s:%s", poisonKeyPrefix, vkScope, entryKey)
}

// IsPoisoned checks whether a Redis SET key exists for the given pair.
// Returns (true, nil) when the key exists (poisoned), (false, nil) on miss,
// and (false, err) on a Redis error.
func (p *RedisPoisonList) IsPoisoned(ctx context.Context, entryKey, vkScope string) (bool, error) {
	k := poisonKey(entryKey, vkScope)
	val, err := p.rdb.Exists(ctx, k).Result()
	if err != nil {
		return false, fmt.Errorf("poison: EXISTS %q: %w", k, err)
	}
	return val > 0, nil
}

// Add marks an entry as poisoned with TTL = min(ttl*10, 30d).
// When ttl ≤ 0 a default of 24 h is substituted before multiplying.
// The value stored is "1" (minimal footprint).
func (p *RedisPoisonList) Add(ctx context.Context, entryKey, vkScope string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	poisonTTL := ttl * poisonTTLMultiplier
	if poisonTTL > poisonMaxTTL {
		poisonTTL = poisonMaxTTL
	}
	k := poisonKey(entryKey, vkScope)
	return p.rdb.Set(ctx, k, "1", poisonTTL).Err()
}

// nopPoisonList is a no-op PoisonList used when no Redis client is available
// (e.g. in tests where poison checking is not under test). IsPoisoned always
// returns (false, nil) and Add is a no-op.
type nopPoisonList struct{}

func (nopPoisonList) IsPoisoned(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (nopPoisonList) Add(_ context.Context, _, _ string, _ time.Duration) error { return nil }
