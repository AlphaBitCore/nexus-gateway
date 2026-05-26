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
	if len(rl.scriptSHA) != 40 {
		t.Errorf("scriptSHA = %q (len=%d), want 40-char SHA1", rl.scriptSHA, len(rl.scriptSHA))
	}
}

// TestNewRedisLimiter_FailureReturnsNil: when the Redis endpoint is dead the
// constructor must retry, log, and ultimately return nil so the wrapper can
// degrade gracefully (no script SHA cached).
func TestNewRedisLimiter_FailureReturnsNil(t *testing.T) {
	s, rdb := newMiniRedis(t)
	// Kill the server BEFORE construction so SCRIPT LOAD fails on every retry.
	s.Close()

	rl := NewRedisLimiter(rdb, discardLogger())
	if rl != nil {
		t.Fatal("NewRedisLimiter must return nil when SCRIPT LOAD fails")
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
