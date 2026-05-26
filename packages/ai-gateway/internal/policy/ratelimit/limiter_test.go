package ratelimit

import (
	"io"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLimiter_NewLocalOnly_NilRedis verifies the local-only construction path
// produces a working limiter that does not require Redis.
func TestLimiter_NewLocalOnly(t *testing.T) {
	l := NewLocalOnly(discardLogger())
	if l == nil {
		t.Fatal("NewLocalOnly returned nil")
	}
	if l.redis != nil {
		t.Error("NewLocalOnly must not initialise Redis")
	}
	if l.local == nil {
		t.Error("NewLocalOnly must initialise local limiter")
	}
	// Functional: 3 allowed, 4th blocked.
	for i := range 3 {
		ok, _ := l.Allow("k", 3, 60_000)
		if !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	ok, retry := l.Allow("k", 3, 60_000)
	if ok {
		t.Fatal("4th request must be blocked")
	}
	if retry < 1 {
		t.Fatalf("retry = %d, want >= 1", retry)
	}
}

// TestLimiter_New_NilRedis exercises the New constructor's nil-redis branch.
func TestLimiter_New_NilRedis(t *testing.T) {
	l := New(nil, discardLogger())
	if l.redis != nil {
		t.Error("New(nil, …) must leave redis nil")
	}
	ok, _ := l.Allow("k", 5, 60_000)
	if !ok {
		t.Error("first request must be allowed")
	}
}

// TestLimiter_New_WithRedis exercises the New constructor with a real (mini)
// Redis client and verifies Redis is the primary path.
func TestLimiter_New_WithRedis(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	l := New(rdb, discardLogger())
	if l.redis == nil {
		t.Fatal("New must wire RedisLimiter when rdb != nil")
	}

	// 2 allowed, 3rd blocked — limit=2.
	if ok, _ := l.Allow("k", 2, 60_000); !ok {
		t.Fatal("1st request must be allowed")
	}
	if ok, _ := l.Allow("k", 2, 60_000); !ok {
		t.Fatal("2nd request must be allowed")
	}
	ok, retry := l.Allow("k", 2, 60_000)
	if ok {
		t.Fatal("3rd request must be blocked")
	}
	if retry < 1 {
		t.Fatalf("retry = %d, want >= 1", retry)
	}
}

// TestLimiter_Allow_ZeroLimit covers the limit<=0 short-circuit (never touches storage).
func TestLimiter_Allow_ZeroLimit(t *testing.T) {
	l := NewLocalOnly(discardLogger())
	if ok, retry := l.Allow("k", 0, 60_000); !ok || retry != 0 {
		t.Errorf("Allow(limit=0) = (%v, %d), want (true, 0)", ok, retry)
	}
	if ok, retry := l.Allow("k", -5, 60_000); !ok || retry != 0 {
		t.Errorf("Allow(limit<0) = (%v, %d), want (true, 0)", ok, retry)
	}
}

// TestLimiter_Allow_RedisFailFallsBackToLocal: when the underlying Redis call
// returns an error, the wrapper MUST fall back to the local limiter rather
// than reject the request. We force the error by closing miniredis after
// construction.
func TestLimiter_Allow_RedisFailFallsBackToLocal(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	l := New(rdb, discardLogger())
	if l.redis == nil {
		t.Fatal("RedisLimiter must be wired")
	}

	// Close miniredis. EVALSHA will fail; wrapper must fall back to local.
	s.Close()

	// Local fallback: limit=2, two allowed, third blocked.
	if ok, _ := l.Allow("fbk", 2, 60_000); !ok {
		t.Fatal("local fallback: 1st request must be allowed")
	}
	if ok, _ := l.Allow("fbk", 2, 60_000); !ok {
		t.Fatal("local fallback: 2nd request must be allowed")
	}
	ok, retry := l.Allow("fbk", 2, 60_000)
	if ok {
		t.Fatal("local fallback: 3rd request must be blocked")
	}
	if retry < 1 {
		t.Fatalf("retry = %d, want >= 1", retry)
	}
}

// TestLimiter_Cleanup verifies Cleanup proxies to the local limiter and
// prunes stale entries while leaving recent ones intact.
func TestLimiter_Cleanup(t *testing.T) {
	l := NewLocalOnly(discardLogger())
	if ok, _ := l.Allow("fresh", 5, 60_000); !ok {
		t.Fatal("seed: must allow")
	}
	// Inject a stale window directly.
	l.local.mu.Lock()
	l.local.windows["stale"] = &window{timestamps: []int64{0}}
	l.local.mu.Unlock()

	l.Cleanup()

	l.local.mu.Lock()
	defer l.local.mu.Unlock()
	if _, ok := l.local.windows["stale"]; ok {
		t.Error("stale window must be removed by Cleanup")
	}
	if _, ok := l.local.windows["fresh"]; !ok {
		t.Error("fresh window must survive Cleanup")
	}
}

// TestLimiter_PerKeyIsolation: rate-limit state is keyed; one key hitting the
// limit must not affect another key.
func TestLimiter_PerKeyIsolation(t *testing.T) {
	l := NewLocalOnly(discardLogger())
	for range 3 {
		if ok, _ := l.Allow("A", 3, 60_000); !ok {
			t.Fatal("key A must be allowed under its own limit")
		}
	}
	if ok, _ := l.Allow("A", 3, 60_000); ok {
		t.Fatal("key A 4th request must be blocked")
	}
	// Key B is still unused.
	if ok, _ := l.Allow("B", 3, 60_000); !ok {
		t.Fatal("key B must be independent of A")
	}
}
