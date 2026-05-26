// Package budget implements the per-route daily embedding-cost ceiling described
// in response-cache-architecture.md §7.4.
//
// The tracker accumulates an INCRBYFLOAT counter in Redis keyed by
// nexus:cache:embedding-budget:<routeID>:<UTCdate> with a 26-hour TTL (so the
// key survives slightly past UTC midnight for in-flight requests). The counter
// resets naturally when the key expires. No explicit reset cron is required.
//
// Callers invoke Allow before issuing an embedding call to check whether the
// daily budget has been exhausted. They invoke Add after any embedding call
// (regardless of L2 outcome) to accumulate the spend.
package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// keyPrefix is the Redis key namespace for embedding budget counters.
	keyPrefix = "nexus:cache:embedding-budget"

	// ttl is the key TTL: 26h ensures the counter survives past UTC midnight
	// for in-flight requests while still resetting on the following UTC day.
	ttl = 26 * time.Hour
)

// Tracker accumulates per-route daily embedding spend and enforces a ceiling.
// Thread-safe; all methods may be called concurrently.
type Tracker struct {
	rdb *redis.Client
	log *slog.Logger
	ns  string // unused today; reserved for multi-tenant expansion
}

// NewTracker constructs a Tracker.
//   - rdb: the shared *redis.Client pointing at the Valkey instance.
//   - log: service-level slog.Logger; never nil.
//   - ns: namespace string (e.g. "nexus") — reserved for future use.
func NewTracker(rdb *redis.Client, log *slog.Logger, ns string) *Tracker {
	return &Tracker{
		rdb: rdb,
		log: log,
		ns:  ns,
	}
}

// Allow reports whether the current day's embedding spend for routeID is below
// ceilingUSD.  Routes with ceilingUSD <= 0 (unlimited) always return
// (true, 0, nil).
//
// Returns:
//   - allowed: true when spend < ceiling (caller may proceed with embedding).
//   - currentSpend: today's accumulated spend in USD (0 when ceiling is unset).
//   - err: non-nil only on Redis connection failures; callers should treat a
//     Redis error as allow=true (fail-open per §6 failure-mode policy).
func (t *Tracker) Allow(ctx context.Context, routeID string, ceilingUSD float64) (allowed bool, currentSpend float64, err error) {
	if ceilingUSD <= 0 {
		// Unlimited — skip the Redis read.
		return true, 0, nil
	}

	key := budgetKey(routeID)
	val, redisErr := t.rdb.Get(ctx, key).Result()
	if errors.Is(redisErr, redis.Nil) {
		// No entry today; spend is 0.
		return true, 0, nil
	}
	if redisErr != nil {
		t.log.Warn("budget/tracker: Allow Redis error; failing open",
			"routeID", routeID, "error", redisErr)
		return true, 0, redisErr
	}

	spend, parseErr := strconv.ParseFloat(val, 64)
	if parseErr != nil {
		t.log.Warn("budget/tracker: Allow parse error; failing open",
			"routeID", routeID, "val", val, "error", parseErr)
		return true, 0, parseErr
	}

	return spend < ceilingUSD, spend, nil
}

// Add increments today's embedding spend for routeID by usd.  It is safe to
// call from multiple goroutines simultaneously (INCRBYFLOAT is atomic in Redis).
// If the key does not yet exist, it is created with the 26-hour TTL.
//
// Errors are logged at WARN but do not propagate to the caller; the audit
// budget is best-effort accounting, not a hard gate.
func (t *Tracker) Add(ctx context.Context, routeID string, usd float64) error {
	if usd <= 0 {
		return nil
	}
	key := budgetKey(routeID)

	// INCRBYFLOAT atomically adds usd to the current value and returns the new total.
	newVal, err := t.rdb.IncrByFloat(ctx, key, usd).Result()
	if err != nil {
		t.log.Warn("budget/tracker: Add IncrByFloat error",
			"routeID", routeID, "usd", usd, "error", err)
		return fmt.Errorf("budget/tracker: IncrByFloat %q: %w", key, err)
	}

	// Set (or refresh) the TTL only when we just wrote the first cent of today's
	// budget.  We approximate "first write" by checking whether the returned value
	// is approximately equal to usd (within float noise).  This is not perfectly
	// race-free but is acceptable: a concurrent Add that wins the INCRBYFLOAT may
	// race the PEXPIRE; the effect is at most one missing TTL refresh in the same
	// UTC second, which is harmless.
	if newVal <= usd+0.0000001 {
		if pexpErr := t.rdb.PExpire(ctx, key, ttl).Err(); pexpErr != nil {
			t.log.Warn("budget/tracker: Add PEXPIRE error (non-fatal)",
				"routeID", routeID, "ttl", ttl, "error", pexpErr)
		}
	}

	return nil
}

// budgetKey returns the Redis key for a route's today's embedding budget.
// Format: nexus:cache:embedding-budget:<routeID>:<YYYY-MM-DD> (UTC date).
func budgetKey(routeID string) string {
	date := time.Now().UTC().Format("2006-01-02")
	return fmt.Sprintf("%s:%s:%s", keyPrefix, routeID, date)
}
