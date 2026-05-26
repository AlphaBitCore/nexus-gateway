package budget

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestTracker starts a fresh miniredis instance and returns a Tracker backed
// by it.  cleanup must be called by the test to stop the server.
func newTestTracker(t *testing.T) (*Tracker, *miniredis.Miniredis, func()) {
	t.Helper()
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatalf("miniredis.Start: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleanup := func() {
		_ = rdb.Close()
		mr.Close()
	}
	return NewTracker(rdb, log, "nexus"), mr, cleanup
}

// TestAllow_Unlimited verifies that a ceilingUSD <= 0 always returns (true, 0, nil)
// without touching Redis.
func TestAllow_Unlimited(t *testing.T) {
	tracker, _, cleanup := newTestTracker(t)
	defer cleanup()

	for _, ceiling := range []float64{0, -1, -100} {
		allowed, spend, err := tracker.Allow(context.Background(), "route-1", ceiling)
		if err != nil {
			t.Errorf("ceiling=%v: unexpected error: %v", ceiling, err)
		}
		if !allowed {
			t.Errorf("ceiling=%v: expected allowed=true for unlimited", ceiling)
		}
		if spend != 0 {
			t.Errorf("ceiling=%v: expected spend=0, got %v", ceiling, spend)
		}
	}
}

// TestAllow_NoKeyYet verifies that Allow returns (true, 0, nil) when no budget
// has been spent today (Redis key absent).
func TestAllow_NoKeyYet(t *testing.T) {
	tracker, _, cleanup := newTestTracker(t)
	defer cleanup()

	allowed, spend, err := tracker.Allow(context.Background(), "route-nokey", 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true when no budget has been spent")
	}
	if spend != 0 {
		t.Errorf("expected spend=0, got %v", spend)
	}
}

// TestAllow_BelowCeiling verifies that Allow returns true when the accumulated
// spend is below the ceiling.
func TestAllow_BelowCeiling(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	// Pre-seed the budget key with a spend value below the ceiling.
	date := budgetKey("route-below")
	if err := mr.Set(date, "0.50"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}

	allowed, spend, err := tracker.Allow(context.Background(), "route-below", 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true when spend (0.50) < ceiling (1.00)")
	}
	if spend < 0.49 || spend > 0.51 {
		t.Errorf("expected spend≈0.50, got %v", spend)
	}
}

// TestAllow_AtOrAboveCeiling verifies that Allow returns false when spend >= ceiling.
func TestAllow_AtOrAboveCeiling(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	// Pre-seed a spend that exceeds the ceiling.
	date := budgetKey("route-over")
	if err := mr.Set(date, "2.00"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}

	allowed, spend, err := tracker.Allow(context.Background(), "route-over", 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("expected allowed=false when spend (2.00) >= ceiling (1.00)")
	}
	if spend < 1.99 || spend > 2.01 {
		t.Errorf("expected spend≈2.00, got %v", spend)
	}
}

// TestAllow_ParseError verifies that a corrupt Redis value (non-float) is treated
// as fail-open (allowed=true) and returns a non-nil error.
func TestAllow_ParseError(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	date := budgetKey("route-corrupt")
	if err := mr.Set(date, "not-a-number"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}

	allowed, _, err := tracker.Allow(context.Background(), "route-corrupt", 1.0)
	if err == nil {
		t.Error("expected non-nil error for corrupt Redis value")
	}
	if !allowed {
		t.Error("expected fail-open (allowed=true) on parse error")
	}
}

// TestAdd_IncrByFloat verifies that Add accumulates spend in Redis and sets the TTL.
func TestAdd_IncrByFloat(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	const routeID = "route-add"
	ctx := context.Background()

	if err := tracker.Add(ctx, routeID, 0.10); err != nil {
		t.Fatalf("Add 0.10: %v", err)
	}
	if err := tracker.Add(ctx, routeID, 0.20); err != nil {
		t.Fatalf("Add 0.20: %v", err)
	}

	key := budgetKey(routeID)
	val, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	// Check the accumulated value is ≈ 0.30.
	if !strings.HasPrefix(val, "0.3") {
		t.Errorf("expected accumulated spend ≈ 0.30, got %q", val)
	}

	// Check TTL is set (non-zero).
	ttlDur := mr.TTL(key)
	if ttlDur <= 0 {
		t.Errorf("expected positive TTL; got %v", ttlDur)
	}
}

// TestAdd_ZeroOrNegative verifies that Add with usd <= 0 is a no-op (no Redis write).
func TestAdd_ZeroOrNegative(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	const routeID = "route-noop"
	ctx := context.Background()

	if err := tracker.Add(ctx, routeID, 0); err != nil {
		t.Fatalf("Add 0: %v", err)
	}
	if err := tracker.Add(ctx, routeID, -1.0); err != nil {
		t.Fatalf("Add -1: %v", err)
	}

	// Key should not exist.
	key := budgetKey(routeID)
	if mr.Exists(key) {
		t.Errorf("expected no Redis key after zero/negative Add; key %q exists", key)
	}
}

// TestAdd_AllowRoundtrip verifies the Allow → Add → Allow cycle: spend
// accumulates and eventually blocks further calls.
func TestAdd_AllowRoundtrip(t *testing.T) {
	tracker, _, cleanup := newTestTracker(t)
	defer cleanup()

	const routeID = "route-roundtrip"
	const ceiling = 0.25
	ctx := context.Background()

	// Initially allowed.
	allowed, _, err := tracker.Allow(ctx, routeID, ceiling)
	if err != nil || !allowed {
		t.Fatalf("initial Allow: allowed=%v err=%v", allowed, err)
	}

	// Spend 0.10 twice.
	if err := tracker.Add(ctx, routeID, 0.10); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	if err := tracker.Add(ctx, routeID, 0.10); err != nil {
		t.Fatalf("Add 2: %v", err)
	}

	// Still allowed (0.20 < 0.25).
	allowed, spend, err := tracker.Allow(ctx, routeID, ceiling)
	if err != nil || !allowed {
		t.Fatalf("mid Allow: allowed=%v spend=%v err=%v", allowed, spend, err)
	}

	// Spend past the ceiling.
	if err := tracker.Add(ctx, routeID, 0.10); err != nil {
		t.Fatalf("Add 3: %v", err)
	}

	// Now blocked (0.30 >= 0.25).
	allowed, spend, err = tracker.Allow(ctx, routeID, ceiling)
	if err != nil {
		t.Fatalf("final Allow err: %v", err)
	}
	if allowed {
		t.Errorf("expected blocked after exceeding ceiling; spend=%v ceiling=%v", spend, ceiling)
	}
}

// TestAllow_RedisError verifies that a Redis error on GET is treated as
// fail-open (allowed=true) and the error is returned to the caller.
func TestAllow_RedisError(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	// Inject a Redis error so that GET fails.
	mr.SetError("ERR simulated connection error")
	defer mr.SetError("") // restore

	allowed, _, err := tracker.Allow(context.Background(), "route-redis-err", 1.0)
	if err == nil {
		t.Error("expected non-nil error on Redis GET failure")
	}
	if !allowed {
		t.Error("expected fail-open (allowed=true) on Redis GET error")
	}
}

// TestAdd_RedisError verifies that a Redis error on INCRBYFLOAT is propagated
// to the caller as a non-nil error.
func TestAdd_RedisError(t *testing.T) {
	tracker, mr, cleanup := newTestTracker(t)
	defer cleanup()

	// Inject a Redis error so that INCRBYFLOAT fails.
	mr.SetError("ERR simulated incr error")
	defer mr.SetError("")

	err := tracker.Add(context.Background(), "route-add-err", 0.10)
	if err == nil {
		t.Error("expected non-nil error on INCRBYFLOAT failure")
	}
}

// TestBudgetKey verifies the key format includes the routeID and today's date.
func TestBudgetKey(t *testing.T) {
	key := budgetKey("my-route")
	if !strings.HasPrefix(key, keyPrefix+":my-route:") {
		t.Errorf("unexpected key format: %q", key)
	}
	// Date portion must be 10 chars (YYYY-MM-DD).
	parts := strings.Split(key, ":")
	if len(parts) < 4 {
		t.Fatalf("expected >=4 colon-separated parts; got %d in %q", len(parts), key)
	}
	date := parts[len(parts)-1]
	if len(date) != 10 {
		t.Errorf("date part %q should be 10 chars (YYYY-MM-DD)", date)
	}
}
