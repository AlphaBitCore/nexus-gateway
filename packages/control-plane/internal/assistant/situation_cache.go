package assistant

import (
	"context"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// situationTTL is how long a per-caller situation snapshot is reused before the next
// turn rebuilds it. Sized for a real chat cadence: a back-and-forth where the operator
// reads an answer and types a follow-up is typically tens of seconds, so 60s lets the
// common rapid exchange reuse the snapshot rather than re-issuing the
// ~8 admin reads every turn. The ambient state it caches (kill-switch, firing alerts,
// fleet sync) can therefore be up to 60s stale in the prompt — acceptable because it
// is advisory context only: the agent has live observe_*/analyze_* tools for anything
// the user actually asks, and every write is confirm-gated on a LIVE state read (the
// confirm impact preview), never on this snapshot. A shorter TTL would rarely hit; a
// longer one would stale the ambient view past usefulness.
const situationTTL = 60 * time.Second

type situationEntry struct {
	snap   agent.Situation
	expiry time.Time
}

// situationCache memoizes the per-turn Situation snapshot per caller. It is keyed by
// the authenticated userId so one caller's IAM-scoped view (the situation swallows
// denied sub-calls into empty fields) can never be served to another principal. Lives
// on the Handler so it survives across a session's turns; one entry per caller, so it
// is bounded by the (small) admin-user count. Safe for concurrent use.
type situationCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]situationEntry
	now     func() time.Time // injectable clock for tests
}

func newSituationCache(ttl time.Duration) *situationCache {
	if ttl <= 0 {
		ttl = situationTTL
	}
	return &situationCache{ttl: ttl, entries: map[string]situationEntry{}, now: time.Now}
}

// get returns the cached snapshot for key when it is still fresh.
func (c *situationCache) get(key string) (agent.Situation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().After(e.expiry) {
		return agent.Situation{}, false
	}
	return e.snap, true
}

func (c *situationCache) put(key string, snap agent.Situation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = situationEntry{snap: snap, expiry: c.now().Add(c.ttl)}
}

// cachedSituation wraps an inner SituationProvider with the per-caller TTL cache. A
// fresh cache hit returns the memoized snapshot without making any admin call; a miss
// builds a new snapshot (the ~8 reads) and stores it. Errors are never cached — but
// the kernel Situation.Snapshot never returns one anyway (it degrades each sub-call
// to an empty field), so a miss always yields a cacheable snapshot.
type cachedSituation struct {
	inner agent.SituationProvider
	cache *situationCache
	key   string
}

func (s cachedSituation) Snapshot(ctx context.Context) (agent.Situation, error) {
	if snap, ok := s.cache.get(s.key); ok {
		return snap, nil
	}
	snap, err := s.inner.Snapshot(ctx)
	if err != nil {
		return snap, err
	}
	s.cache.put(s.key, snap)
	return snap, nil
}
