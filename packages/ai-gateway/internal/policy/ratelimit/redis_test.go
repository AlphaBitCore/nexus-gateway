package ratelimit

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newMiniRedis spins up an in-memory Redis (with EVALSHA + SCRIPT LOAD
// support) and returns both the server handle and a connected client.
func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return s, rdb
}

// TestNewRedisLimiter_LoadsScript verifies the constructor loads the Lua
// script and stores a SHA1 of the expected length.
func TestNewRedisLimiter_LoadsScript(t *testing.T) {
	_, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("NewRedisLimiter returned nil for healthy Redis")
	}
	if rl.script == nil {
		t.Fatal("limiter has no script — Allow would have nothing to dispatch")
	}
	// Asserting the hash is 40 chars would assert a go-redis property, not
	// ours. What this code owns is that the script it dispatches is the
	// SlidingWindow one — see TestRedisLimiter_ConstructorLoadsTheScriptAllowDispatches
	// for the server-side half.
	if rl.script.Hash() != redis.NewScript(SlidingWindowLua).Hash() {
		t.Errorf("the limiter dispatches a script other than SlidingWindowLua")
	}
}

// TestNewRedisLimiter_StartupFailureRecoversWhenRedisReturns replaces an
// assertion that pinned the defect as the contract.
//
// The old test required the constructor to return nil when the startup probe
// failed and called that "degrading gracefully". It could not tell graceful
// from PERMANENT: Limiter.New calls the constructor exactly once, so a nil left
// every later Allow on the per-instance LocalLimiter for the lifetime of the
// process and the fleet enforced N x the configured limit — silently, with no
// recovery short of a restart. A gateway starting during a Redis failover, or
// ahead of Redis in an ordering-free bring-up, is enough to reach it.
//
// Recovery is the outcome that matters, so recovery is what is asserted: a
// limiter built against a dead Redis must enforce cluster-wide again once Redis
// answers, which it can because Allow dispatches through *redis.Script and
// reloads the body on NOSCRIPT.
func TestNewRedisLimiter_StartupFailureRecoversWhenRedisReturns(t *testing.T) {
	s, rdb := newMiniRedis(t)
	addr := s.Addr()
	// Kill the server BEFORE construction so SCRIPT LOAD fails on every retry.
	s.Close()

	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("a failed startup probe must not disable Redis rate limiting for the process lifetime — " +
			"every Allow then falls to the per-instance local limiter and the fleet enforces N x the limit")
	}

	// While Redis is still down Allow reports the error, so Limiter.Allow can
	// fall back per request. It must not fabricate a verdict it did not compute.
	if _, _, err := rl.Allow("k-down", 5, 1000); err == nil {
		t.Fatal("Allow against a dead Redis must return an error, not a fabricated verdict")
	}

	// Redis comes back at the same address the client dials.
	revived := miniredis.NewMiniRedis()
	if err := revived.StartAddr(addr); err != nil {
		t.Skipf("could not re-bind miniredis to %s: %v", addr, err)
	}
	defer revived.Close()

	// The SAME limiter must now work — no reconstruction, no process restart.
	allowed, _, err := rl.Allow("k-up", 5, 1000)
	if err != nil {
		t.Fatalf("the limiter did not resume using Redis once Redis answered: %v", err)
	}
	if !allowed {
		t.Fatal("the first request in a fresh window must be allowed")
	}
}

// TestRedisLimiter_Allow_WithinLimit_ThenBlock asserts the core
// observable behaviour: exactly N allowed, then a reject with retryAfter >= 1.
func TestRedisLimiter_Allow_WithinLimit_ThenBlock(t *testing.T) {
	_, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}

	const limit = 5
	const windowMs int64 = 60_000

	for i := range limit {
		ok, retry, err := rl.Allow("user:abc", limit, windowMs)
		if err != nil {
			t.Fatalf("request %d: unexpected err: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("request %d must be allowed", i+1)
		}
		if retry != 0 {
			t.Errorf("request %d allowed retry = %d, want 0", i+1, retry)
		}
	}

	ok, retry, err := rl.Allow("user:abc", limit, windowMs)
	if err != nil {
		t.Fatalf("blocked request: unexpected err: %v", err)
	}
	if ok {
		t.Fatal("request beyond limit must be blocked")
	}
	if retry < 1 {
		t.Errorf("blocked retry = %d, want >= 1", retry)
	}
	// With a 60s window and oldest timestamp just inserted, retry should
	// be near the window length.
	if retry > 61 {
		t.Errorf("blocked retry = %d, want <= 61s", retry)
	}
}

// TestRedisLimiter_Allow_PerKeyIsolation: rate-limit state is keyed; one key
// hitting the limit must not affect another key (canonical multi-tenancy
// invariant).
func TestRedisLimiter_Allow_PerKeyIsolation(t *testing.T) {
	_, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}

	// Exhaust key A.
	for range 2 {
		if ok, _, err := rl.Allow("A", 2, 60_000); err != nil || !ok {
			t.Fatalf("key A seed failed: ok=%v err=%v", ok, err)
		}
	}
	ok, _, _ := rl.Allow("A", 2, 60_000)
	if ok {
		t.Fatal("key A must be blocked after 2 requests")
	}
	// Key B is independent.
	if ok, _, err := rl.Allow("B", 2, 60_000); err != nil || !ok {
		t.Fatalf("key B must be independent: ok=%v err=%v", ok, err)
	}
}

// TestRedisLimiter_Allow_WindowExpiry asserts that fast-forwarding miniredis
// past the window length releases the limit — the sliding-window refill path.
func TestRedisLimiter_Allow_WindowExpiry(t *testing.T) {
	s, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}

	// Saturate.
	for range 2 {
		if ok, _, err := rl.Allow("k", 2, 1000); err != nil || !ok {
			t.Fatalf("seed: ok=%v err=%v", ok, err)
		}
	}
	if ok, _, _ := rl.Allow("k", 2, 1000); ok {
		t.Fatal("must be blocked at saturation")
	}

	// Advance miniredis clock past the window AND let the key's PEXPIRE
	// fire — wiping the sorted set.
	s.FastForward(2_000_000_000) // 2s in ns

	ok, _, err := rl.Allow("k", 2, 1000)
	if err != nil {
		t.Fatalf("post-expiry: err=%v", err)
	}
	if !ok {
		t.Fatal("post-expiry request must be allowed (sliding window refilled)")
	}
}

// TestRedisLimiter_Allow_RedisErrorPropagates: when EVALSHA fails (server
// down), Allow must surface the error so the wrapper can fall back. The
// returned bool must be false (no implicit allow) and retry zero.
func TestRedisLimiter_Allow_RedisErrorPropagates(t *testing.T) {
	s, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}
	// Kill server post-construction; script SHA stays cached but EVALSHA fails.
	s.Close()

	ok, retry, err := rl.Allow("k", 5, 60_000)
	if err == nil {
		t.Fatal("Allow must return error when Redis is down")
	}
	if ok {
		t.Error("Allow must not silently allow on Redis error")
	}
	if retry != 0 {
		t.Errorf("retry on error = %d, want 0", retry)
	}
}

// TestRedisLimiter_KeyPrefix_Applied verifies the Redis key namespace prefix
// is actually applied — protects against cross-tenant key collisions if the
// prefix were ever dropped.
func TestRedisLimiter_KeyPrefix_Applied(t *testing.T) {
	s, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}
	if _, _, err := rl.Allow("user:xyz", 5, 60_000); err != nil {
		t.Fatalf("Allow err: %v", err)
	}
	// The sorted set must exist at the prefixed key.
	if !s.Exists(KeyPrefix + "user:xyz") {
		t.Errorf("expected key %q to exist; got keys = %v", KeyPrefix+"user:xyz", s.Keys())
	}
	if s.Exists("user:xyz") {
		t.Errorf("unprefixed key %q must not exist (prefix not applied)", "user:xyz")
	}
}

// TestRandomSuffix verifies the hex suffix used to deduplicate identical
// timestamps inside the sorted set is 10 hex chars (5 random bytes), changes
// across calls, and consists of valid hex.
func TestRandomSuffix(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		s := randomSuffix()
		if len(s) != 10 {
			t.Fatalf("randomSuffix() = %q (len=%d), want 10", s, len(s))
		}
		for _, c := range s {
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'f':
			default:
				t.Fatalf("randomSuffix() = %q contains non-hex %q", s, c)
			}
		}
		seen[s] = struct{}{}
	}
	// 1000 samples from a 2^40 space — collisions should be effectively zero.
	if len(seen) < 999 {
		t.Errorf("randomSuffix has too many collisions: %d unique / 1000", len(seen))
	}
}
