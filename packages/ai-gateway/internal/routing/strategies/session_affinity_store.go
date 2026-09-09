// session_affinity_store.go — where a conversation's model is remembered.
//
// Two tiers, because the two failure modes are different. The local tier is a
// bounded in-process map: it cannot fail, cannot add latency, and on a
// single-instance deployment it is the whole answer. Redis is the shared tier
// that lets consecutive turns landing on different gateway instances still find
// the entry.
//
// The ordering rule is the important part: THE LOCAL TIER IS ALWAYS READ FIRST
// AND WRITTEN FIRST. Redis is consulted only on a local miss and written
// best-effort afterwards. A Redis that is down, slow, or hung therefore cannot
// hold up a request — the local answer is already in hand, and every Redis call
// carries its own short deadline independent of the request's.
//
// Session affinity is BEST EFFORT, and the shape follows the one this gateway
// already uses for the same trade-off — quota's UsageCache is "Redis, with an
// in-memory fallback" on redis.UniversalClient, so standalone, sentinel and
// cluster deployments all work. What is deliberately NOT copied from it is the
// write-behind aggregator: quota needs every increment eventually, affinity does
// not need any particular entry at all, so there is nothing to retry and nothing
// to drain.
//
// Everything here is an optimisation. Losing an entry costs one prompt-cache
// miss: the router's own pick is used instead, which is a correct answer, just a
// more expensive one. Nothing in this file may ever fail a request.
package strategies

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// affinityRedisTimeout bounds every Redis call.
//
// This is the number that protects live traffic, not the error handling: an
// unreachable Redis returns an error immediately and the fail-open branch works,
// but a SLOW or hung one would add its latency to the routing path of every
// chat request. The budget is deliberately far below anything a caller would
// notice — the entry is worth having, but not worth waiting for.
const affinityRedisTimeout = 50 * time.Millisecond

// affinityMemoryEntries caps the local tier.
//
// Session ids are caller-asserted, so an unbounded map is a memory-growth vector
// a caller controls: one client emitting a fresh id per request would grow it
// without limit. When the cap is reached the map is dropped wholesale rather
// than evicted entry-by-entry — affinity is an optimisation, so the cheapest
// bound that cannot leak is better than an LRU's per-entry bookkeeping on the
// routing hot path.
const affinityMemoryEntries = 50_000

type memoryAffinityEntry struct {
	modelID   string
	expiresAt time.Time
}

// memorySessionAffinity is the local tier: bounded, TTL'd, lock-guarded.
type memorySessionAffinity struct {
	mu      sync.Mutex
	entries map[string]memoryAffinityEntry
}

func newMemorySessionAffinity() *memorySessionAffinity {
	return &memorySessionAffinity{entries: make(map[string]memoryAffinityEntry)}
}

func (m *memorySessionAffinity) get(key string, now time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return ""
	}
	if now.After(e.expiresAt) {
		delete(m.entries, key)
		return ""
	}
	return e.modelID
}

func (m *memorySessionAffinity) put(key, modelID string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= affinityMemoryEntries {
		// Wholesale reset. Every dropped entry costs its conversation one
		// prompt-cache miss and nothing else, and the alternative — walking the
		// map to evict — runs on the routing path.
		m.entries = make(map[string]memoryAffinityEntry, affinityMemoryEntries/4)
	}
	m.entries[key] = memoryAffinityEntry{modelID: modelID, expiresAt: now.Add(affinityTTL)}
}

// TieredSessionAffinity is the production [SessionAffinityStore].
type TieredSessionAffinity struct {
	local  *memorySessionAffinity
	rdb    redis.UniversalClient // optional; nil = local tier only
	prefix string
	log    *slog.Logger
	now    func() time.Time // seam for tests
}

// NewSessionAffinityStore builds the store. A nil redis client is fully
// supported and yields a local-only store, which is the correct configuration
// for a single-instance gateway: every turn of a conversation reaches the same
// process, so the shared tier would add a network hop and buy nothing.
func NewSessionAffinityStore(rdb redis.UniversalClient, prefix string, log *slog.Logger) *TieredSessionAffinity {
	if prefix == "" {
		prefix = "nexus"
	}
	if log == nil {
		log = slog.Default()
	}
	return &TieredSessionAffinity{
		local:  newMemorySessionAffinity(),
		rdb:    rdb,
		prefix: prefix,
		log:    log,
		now:    time.Now,
	}
}

// key is scoped by virtual key so one caller's session tag can never address
// another caller's entry. The tag has already been trimmed and length-capped by
// affinityKey, the only thing that produces the arguments to these methods.
func (s *TieredSessionAffinity) key(vkID, sessionID string) string {
	return s.prefix + ":route:affinity:" + vkID + ":" + sessionID
}

func (s *TieredSessionAffinity) Get(ctx context.Context, vkID, sessionID string) string {
	if s == nil {
		return ""
	}
	k := s.key(vkID, sessionID)
	if v := s.local.get(k, s.now()); v != "" {
		return v
	}
	if s.rdb == nil {
		return ""
	}
	// Own deadline, derived from the request's only so a cancelled request stops
	// the call too. A hung Redis costs affinityRedisTimeout, once, and then the
	// router's pick is used.
	rctx, cancel := context.WithTimeout(ctx, affinityRedisTimeout)
	defer cancel()
	v, err := s.rdb.Get(rctx, k).Result()
	if err != nil {
		// redis.Nil is the ordinary "this conversation is new" answer. Anything
		// else is a real fault, logged at debug because the request proceeds
		// correctly either way and this runs on every chat request.
		if !errors.Is(err, redis.Nil) {
			s.log.Debug("session affinity: shared tier unavailable, using the router's pick", "error", err)
		}
		return ""
	}
	if v != "" {
		// Populate the local tier so the next turn on this instance skips Redis.
		s.local.put(k, v, s.now())
	}
	return v
}

func (s *TieredSessionAffinity) Put(ctx context.Context, vkID, sessionID, modelID string) {
	if s == nil {
		return
	}
	k := s.key(vkID, sessionID)
	// Local first and unconditionally: it cannot fail, so the entry exists
	// before anything that can.
	s.local.put(k, modelID, s.now())
	if s.rdb == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, affinityRedisTimeout)
	defer cancel()
	// The TTL is refreshed on every turn because what it tracks is the age of
	// the provider's cache entry, and each turn writes a new one.
	if err := s.rdb.Set(rctx, k, modelID, affinityTTL).Err(); err != nil {
		s.log.Debug("session affinity: shared-tier write failed, the local tier still holds it", "error", err)
	}
}

var _ SessionAffinityStore = (*TieredSessionAffinity)(nil)
