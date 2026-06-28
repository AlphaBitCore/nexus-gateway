package login

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis returns a miniredis-backed go-redis client. The returned
// *miniredis.Miniredis lets a test force a Redis outage via Close() to exercise
// the fail-to-local path.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestLimiter_AllowsUnderBudget(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 3, func() time.Time { return clock })

	for i := range 3 {
		if !l.Allow("1.1.1.1", "a@x") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
}

func TestLimiter_BlocksWhenBudgetExceeded(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 3, func() time.Time { return clock })

	for i := range 3 {
		if !l.Allow("1.1.1.1", "a@x") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("4th attempt in window must be denied")
	}
}

func TestLimiter_WindowRolloff(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 2, func() time.Time { return clock })

	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("first attempt")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("second attempt")
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("third attempt must be denied inside window")
	}

	// Advance past the window — earlier attempts should age out.
	clock = clock.Add(2 * time.Minute)
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("attempt after window rolloff should be allowed")
	}
}

func TestLimiter_KeysAreIsolated(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 1, func() time.Time { return clock })

	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("a@x first attempt")
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("a@x second attempt must be denied")
	}
	// Different email from same IP: separate bucket.
	if !l.Allow("1.1.1.1", "b@x") {
		t.Fatal("b@x first attempt should be allowed")
	}
	// Different IP for same email: also separate bucket.
	if !l.Allow("2.2.2.2", "a@x") {
		t.Fatal("a@x from new IP should be allowed")
	}
}

func TestLimiter_DeniedAttemptsDoNotConsumeBudget(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 2, func() time.Time { return clock })

	l.Allow("1.1.1.1", "a@x")
	l.Allow("1.1.1.1", "a@x")
	// Attacker hammers the endpoint while blocked.
	for range 20 {
		if l.Allow("1.1.1.1", "a@x") {
			t.Fatal("denied attempt leaked through")
		}
	}
	// Advance just past the original two attempts' window.
	clock = clock.Add(time.Minute + time.Second)
	// Budget must be fully restored: the blocked attempts did NOT record timestamps.
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("expected budget restored after original window")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("second post-rolloff attempt should also be allowed")
	}
}

func TestLimiter_EmailCaseInsensitive(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 1, func() time.Time { return clock })

	if !l.Allow("1.1.1.1", "Alice@Corp.com") {
		t.Fatal("first attempt")
	}
	// Same key when normalized — must be denied.
	if l.Allow("1.1.1.1", "alice@corp.com") {
		t.Fatal("case variant must hit same bucket")
	}
}

func TestLimiter_DefaultConstructor(t *testing.T) {
	// Sanity check: NewLimiter uses the module defaults and does not panic.
	l := NewLimiter()
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("NewLimiter should start with budget > 0")
	}
}

// TestLimiter_PerIPCapBlocksEmailSpray asserts that a single IP
// spraying one password across many distinct emails is bounded by the per-IP
// global cap even though every individual (ip,email) pair is still under its
// own budget.
func TestLimiter_PerIPCapBlocksEmailSpray(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	// Pair budget 10 (no pair is exhausted), per-IP cap 3.
	l := newLimiterFull(time.Minute, 10, 3, func() time.Time { return clock }, nil)

	for i, email := range []string{"a@x", "b@x", "c@x"} {
		if !l.Allow("9.9.9.9", email) {
			t.Fatalf("attempt %d (%s) should be under both budgets", i, email)
		}
	}
	// Fourth distinct email from the same IP: the pair budget is untouched
	// (first attempt for d@x) but the per-IP cap of 3 is now reached.
	if l.Allow("9.9.9.9", "d@x") {
		t.Fatal("per-IP cap must block a fourth email even though the pair is under budget")
	}
	// A different IP is unaffected by the first IP's global counter.
	if !l.Allow("8.8.8.8", "d@x") {
		t.Fatal("a different IP must have its own independent per-IP budget")
	}
}

// TestLimiter_PerIPCapWindowRolls confirms the per-IP cap is a sliding window:
// once the window elapses the blocked IP is allowed again.
func TestLimiter_PerIPCapWindowRolls(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterFull(time.Minute, 10, 2, func() time.Time { return clock }, nil)

	if !l.Allow("9.9.9.9", "a@x") || !l.Allow("9.9.9.9", "b@x") {
		t.Fatal("first two distinct emails should pass")
	}
	if l.Allow("9.9.9.9", "c@x") {
		t.Fatal("third email must hit the per-IP cap")
	}
	clock = clock.Add(2 * time.Minute) // advance past the window
	if !l.Allow("9.9.9.9", "c@x") {
		t.Fatal("per-IP cap must reset after the window rolls")
	}
}

// TestLimiter_RedisDistributedPerIP asserts the Redis-backed per-IP cap: two
// limiter instances sharing one Redis enforce a single per-IP budget across
// BOTH instances. An in-memory limiter would give each replica its own budget;
// the Redis-backed counter makes the cap global.
func TestLimiter_RedisDistributedPerIP(t *testing.T) {
	_, rdb := newTestRedis(t)
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }

	// Two independent limiter instances (simulating two CP replicas) share rdb.
	// Pair budget 10 (no pair exhausted), per-IP cap 4.
	l1 := newLimiterFull(time.Minute, 10, 4, now, rdb)
	l2 := newLimiterFull(time.Minute, 10, 4, now, rdb)

	// Four distinct emails from one IP, alternating replicas — all under both
	// budgets, all should pass.
	if !l1.Allow("9.9.9.9", "a@x") {
		t.Fatal("a@x on replica 1 should pass")
	}
	if !l2.Allow("9.9.9.9", "b@x") {
		t.Fatal("b@x on replica 2 should pass")
	}
	if !l1.Allow("9.9.9.9", "c@x") {
		t.Fatal("c@x on replica 1 should pass")
	}
	if !l2.Allow("9.9.9.9", "d@x") {
		t.Fatal("d@x on replica 2 should pass")
	}
	// Fifth distinct email — pair budget untouched, but the SHARED per-IP cap of
	// 4 is reached. Either replica must now block it.
	if l1.Allow("9.9.9.9", "e@x") {
		t.Fatal("per-IP cap must hold ACROSS instances (replica 1)")
	}
	if l2.Allow("9.9.9.9", "f@x") {
		t.Fatal("per-IP cap must hold ACROSS instances (replica 2)")
	}
	// A different IP is unaffected by the first IP's shared counter.
	if !l2.Allow("8.8.8.8", "a@x") {
		t.Fatal("a different IP must have its own independent per-IP budget")
	}
}

// TestLimiter_RedisDistributedPerPair asserts the per-(ip,email) budget is also
// shared across instances via Redis.
func TestLimiter_RedisDistributedPerPair(t *testing.T) {
	_, rdb := newTestRedis(t)
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }

	// Per-pair budget 2, per-IP cap 50 (so the pair budget is the binding limit).
	l1 := newLimiterFull(time.Minute, 2, 50, now, rdb)
	l2 := newLimiterFull(time.Minute, 2, 50, now, rdb)

	if !l1.Allow("1.1.1.1", "a@x") {
		t.Fatal("first attempt (replica 1) should pass")
	}
	if !l2.Allow("1.1.1.1", "a@x") {
		t.Fatal("second attempt (replica 2) should pass — budget is 2")
	}
	// Third attempt against the same pair, on either replica, exhausts the
	// SHARED pair budget.
	if l1.Allow("1.1.1.1", "a@x") {
		t.Fatal("third attempt must be blocked by the shared pair budget")
	}
}

// TestLimiter_RedisErrorFallsBackToLocal asserts the graceful degradation
// decision: on a Redis outage the limiter (a) does NOT lock out (login still
// works) and (b) still blocks a local spray via the in-memory fallback.
func TestLimiter_RedisErrorFallsBackToLocal(t *testing.T) {
	mr, rdb := newTestRedis(t)
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }
	// Per-pair budget 2 so the local fallback blocks quickly.
	l := newLimiterFull(time.Minute, 2, 50, now, rdb)

	// Force a Redis outage: every Redis op now errors.
	mr.Close()

	// Not locked out: the first attempt still succeeds via the local limiter.
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("Redis outage must NOT lock out login — first attempt should pass locally")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("second attempt should pass locally (budget 2)")
	}
	// Still protected: the local limiter blocks the spray past its budget.
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("Redis outage must NOT leave login unprotected — local budget must block")
	}
	for range 10 {
		if l.Allow("1.1.1.1", "a@x") {
			t.Fatal("blocked attempt leaked through the local fallback")
		}
	}
}

// TestLimiter_RedisRecoversAfterOutage asserts distributed counting resumes
// once Redis is reachable again: a blocked-locally spray does not poison the
// Redis path, and a fresh pair is allowed against Redis.
func TestLimiter_RedisRecoversAfterOutage(t *testing.T) {
	mr, rdb := newTestRedis(t)
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }
	l := newLimiterFull(time.Minute, 2, 50, now, rdb)

	// Outage → local path. Capture the address before closing (a closed
	// miniredis panics on Addr()).
	addr := mr.Addr()
	mr.Close()
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("first attempt during outage should pass locally")
	}

	// Recover Redis on the same address so the existing client reconnects.
	mr2 := miniredis.NewMiniRedis()
	if err := mr2.StartAddr(addr); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	t.Cleanup(mr2.Close)

	// Distributed counting resumes: a brand-new pair is allowed via Redis, and
	// the per-pair budget of 2 is enforced against the shared store.
	if !l.Allow("2.2.2.2", "z@x") {
		t.Fatal("post-recovery attempt should pass via Redis")
	}
	if !l.Allow("2.2.2.2", "z@x") {
		t.Fatal("second post-recovery attempt should pass (budget 2)")
	}
	if l.Allow("2.2.2.2", "z@x") {
		t.Fatal("third post-recovery attempt must be blocked by the shared budget")
	}
}

// TestLimiter_RedisWindowTTLResets asserts the Redis sliding window rolls: an
// exhausted budget is restored once the window elapses (driven by the injected
// clock, which the script uses as its window reference).
func TestLimiter_RedisWindowTTLResets(t *testing.T) {
	_, rdb := newTestRedis(t)
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }
	l := newLimiterFull(time.Minute, 2, 50, now, rdb)

	// Evaluate both attempts unconditionally — each Allow call consumes a unit
	// of the budget, so a short-circuiting || would skip the second attempt and
	// leave the window under-counted for the assertions below.
	first := l.Allow("1.1.1.1", "a@x")
	second := l.Allow("1.1.1.1", "a@x")
	if !first || !second {
		t.Fatal("first two attempts should pass")
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("third attempt inside the window must be blocked")
	}
	// Advance past the window: the script's ZREMRANGEBYSCORE drops the aged
	// entries and the budget is restored.
	clock = clock.Add(2 * time.Minute)
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("attempt after the Redis window rolls should be allowed")
	}
}

// TestLimiter_RedisBlockedAttemptsDoNotConsumeBudget asserts the script records
// nothing on a blocked attempt, so an attacker cannot lengthen their own window
// by hammering while blocked — parity with the local limiter.
func TestLimiter_RedisBlockedAttemptsDoNotConsumeBudget(t *testing.T) {
	_, rdb := newTestRedis(t)
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }
	l := newLimiterFull(time.Minute, 2, 50, now, rdb)

	l.Allow("1.1.1.1", "a@x")
	l.Allow("1.1.1.1", "a@x")
	for range 20 {
		if l.Allow("1.1.1.1", "a@x") {
			t.Fatal("blocked attempt leaked through the Redis path")
		}
	}
	// Advance just past the original two attempts' window. The blocked attempts
	// recorded nothing, so the budget is fully restored.
	clock = clock.Add(time.Minute + time.Second)
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("budget should be restored after the original window (blocked attempts did not record)")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("second post-rolloff attempt should also pass")
	}
}

// TestLimiter_NewLimiterWithRedisCountsInRedis asserts the production
// constructor wires the Redis backend (a successful attempt leaves state in
// Redis under the login key prefix).
func TestLimiter_NewLimiterWithRedisCountsInRedis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	l := NewLimiterWithRedis(rdb)

	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("first attempt should pass")
	}
	// The per-IP sorted set must exist with one member.
	if got := mr.Exists(redisKeyPrefix + "ip:1.1.1.1"); !got {
		t.Fatal("expected the per-IP Redis key to be created")
	}
	if got := mr.Exists(redisKeyPrefix + "pair:1.1.1.1:a@x"); !got {
		t.Fatal("expected the per-pair Redis key to be created")
	}
}

// TestLimiter_NewLimiterWithRedisNilIsLocalOnly asserts passing a nil Redis
// handle yields a working local-only limiter (the no-Redis dev path).
func TestLimiter_NewLimiterWithRedisNilIsLocalOnly(t *testing.T) {
	l := NewLimiterWithRedis(nil)
	if l.rdb != nil {
		t.Fatal("nil Redis handle must leave the limiter local-only")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("local-only limiter should allow the first attempt")
	}
}

// TestLimiter_LocalSweepBoundsMaps asserts the opportunistic sweep prunes
// fully-aged-out keys from the in-memory maps so a never-revisited key does not
// linger forever (bounding unbounded map growth).
func TestLimiter_LocalSweepBoundsMaps(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }
	// Local-only limiter (no Redis) so Allow takes the in-memory path.
	l := newLimiterFull(time.Minute, 5, 50, now, nil)

	// Seed several distinct keys that will never be queried again.
	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		if !l.Allow(ip, "a@x") {
			t.Fatalf("seed attempt for %s should pass", ip)
		}
	}
	l.mu.Lock()
	seeded := len(l.ipHistory)
	l.mu.Unlock()
	if seeded != 3 {
		t.Fatalf("expected 3 seeded ip keys, got %d", seeded)
	}

	// Advance past the window so the seeded keys are fully aged out, then make a
	// single new attempt from a fresh IP — this triggers the once-per-window
	// sweep, which must drop the three stale keys.
	clock = clock.Add(2 * time.Minute)
	if !l.Allow("4.4.4.4", "a@x") {
		t.Fatal("post-window attempt from a new IP should pass")
	}
	l.mu.Lock()
	remaining := len(l.ipHistory)
	l.mu.Unlock()
	// Only the just-active key (4.4.4.4) should remain; the three stale ones are
	// swept.
	if remaining != 1 {
		t.Fatalf("expected stale keys swept (1 remaining), got %d", remaining)
	}
}

// TestLimiter_LocalSweepRetainsActiveKeys asserts the sweep keeps keys that
// still have in-window activity (it must not delete a key that is merely
// partially aged out) while pruning the stale timestamps it does contain.
func TestLimiter_LocalSweepRetainsActiveKeys(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	now := func() time.Time { return clock }
	// window 60s, local-only.
	l := newLimiterFull(time.Minute, 5, 50, now, nil)

	// t0: first attempt for A records at t0 (lastSweep is also t0).
	if !l.Allow("A", "a@x") {
		t.Fatal("t0 attempt should pass")
	}
	// t0+40s: second attempt for A (no sweep yet — 40s < window). A now holds
	// timestamps {t0, t0+40}.
	clock = clock.Add(40 * time.Second)
	if !l.Allow("A", "a@x") {
		t.Fatal("t0+40 attempt should pass")
	}
	// t0+70s: a new IP B triggers the once-per-window sweep (70s >= window).
	// Sweep cutoff = t0+10s: A's t0 entry is dropped but its t0+40 entry
	// survives, so A is RETAINED (not deleted).
	clock = clock.Add(30 * time.Second)
	if !l.Allow("B", "b@x") {
		t.Fatal("t0+70 attempt should pass")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.ipHistory["A"]; !ok {
		t.Fatal("active key A must be retained by the sweep, not deleted")
	}
	if len(l.ipHistory["A"]) != 1 {
		t.Fatalf("A's stale timestamp should be pruned to 1 surviving entry, got %d", len(l.ipHistory["A"]))
	}
	if _, ok := l.history["A:a@x"]; !ok {
		t.Fatal("active pair key A:a@x must be retained by the sweep")
	}
}
