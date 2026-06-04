package assistant

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// ownerRegistry tracks which CP instance is currently handling a given assistant
// session, so a confirm POST that the load balancer misroutes to the wrong pod
// (the parked confirm channel lives only on the owner pod, in memory) can be
// answered with 421 — telling the LB / client to retry at the owner — instead of
// a confusing 409 "expired". This is the multi-replica affinity SAFETY NET: with
// correct consistent-hash affinity the confirm already lands on the owner; the
// registry catches the transient misroutes during pod scale events.
//
// It is intentionally minimal: a single Redis key per session with a TTL longer
// than one turn, set unconditionally at turn start (the newest turn owns the
// session). No heartbeat — the TTL covers a full turn and a dead pod's ownership
// simply expires (dead-owner takeover via TTL). Correctness does NOT depend on
// ownership being accurate: the Confirm handler checks the in-memory registry
// FIRST and only consults this when the confirm is not parked locally, so a
// parked-here confirm always resolves regardless of what Redis says.
type ownerRegistry struct {
	rdb     redis.UniversalClient
	ownerID string        // this instance's identity (hostname)
	ttl     time.Duration // > turnDeadline so ownership never expires mid-turn
}

// newOwnerRegistry returns nil when no Redis is wired (single-replica / pool-less
// dev) — callers treat a nil registry as "always local owner" (no 421).
func newOwnerRegistry(rdb redis.UniversalClient, ownerID string, ttl time.Duration) *ownerRegistry {
	if rdb == nil || ownerID == "" {
		return nil
	}
	return &ownerRegistry{rdb: rdb, ownerID: ownerID, ttl: ttl}
}

func ownerKey(sessionScope string) string { return "assistant:owner:" + sessionScope }

// claim records this instance as the owner of sessionScope for ttl. Best-effort:
// a Redis error is swallowed (the registry is a safety net, not a correctness
// dependency — a failed claim just means a later misrouted confirm won't 421).
func (o *ownerRegistry) claim(ctx context.Context, sessionScope string) {
	if o == nil {
		return
	}
	_ = o.rdb.Set(ctx, ownerKey(sessionScope), o.ownerID, o.ttl).Err()
}

// owner reports whether THIS instance owns sessionScope. `known` is false when no
// live owner is recorded (key absent/expired) OR Redis is unreachable — in both
// cases the caller must NOT 421 (fail-open: fall back to the local-only answer).
// A nil registry reports (true, false): single-pod, always local, never 421.
func (o *ownerRegistry) owner(ctx context.Context, sessionScope string) (mine bool, known bool) {
	if o == nil {
		return true, false
	}
	// Bound the lookup tightly: this gates a confirm response, so a Redis outage
	// must fail open in well under a second rather than stall on the client's
	// default dial/read timeout.
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	val, err := o.rdb.Get(cctx, ownerKey(sessionScope)).Result()
	if err != nil { // redis.Nil (absent), timeout, or a transport error → fail-open
		return false, false
	}
	return val == o.ownerID, true
}
