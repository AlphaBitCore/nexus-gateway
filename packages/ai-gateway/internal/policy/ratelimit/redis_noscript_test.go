package ratelimit

import (
	"testing"
)

// EvalSha against a hash captured once at construction, with no NOSCRIPT
// fallback, turns a Redis restart — or a failover to a replica that never
// loaded the script, or a SCRIPT FLUSH — into an error on EVERY subsequent
// call for the lifetime of the process. Because the caller's error path is
// "fall back to the local limiter", the cluster-wide limit then silently
// becomes per-instance: N replicas each enforcing the full quota, with nothing
// in the logs saying the limit changed meaning.
//
// This is the arm that reproduces it. Losing the server's script cache is a
// routine operational event, not an exotic one, so the limiter has to survive it
// WITHOUT being reconstructed.
func TestRedisLimiter_RecoversAfterScriptCacheIsLost(t *testing.T) {
	s, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}

	const limit = 3
	const windowMs = int64(60_000)

	// Establish that the limiter works at all before the cache is lost —
	// otherwise a limiter that never worked would "survive" trivially.
	if allowed, _, err := rl.Allow("recover-key", limit, windowMs); err != nil || !allowed {
		t.Fatalf("baseline call: allowed=%v err=%v — want allowed with no error", allowed, err)
	}

	// Lose the server-side script cache, exactly as a restart or SCRIPT FLUSH
	// does. The limiter object is NOT rebuilt: surviving this without a restart
	// is the whole point.
	s.FlushAll()
	if err := rdb.ScriptFlush(t.Context()).Err(); err != nil {
		t.Fatalf("setup: SCRIPT FLUSH: %v", err)
	}

	// Same limiter, same process. It must reload and keep enforcing.
	allowed, _, err := rl.Allow("recover-key", limit, windowMs)
	if err != nil {
		t.Fatalf("after the script cache was lost: err=%v — the limiter did not recover, so every "+
			"subsequent request falls back to the per-instance limiter and the cluster quota is gone", err)
	}
	if !allowed {
		t.Errorf("allowed=false on the first call after recovery; the window was flushed too, so this should pass")
	}

	// And it must still ENFORCE, not merely stop erroring: a limiter that
	// recovered into always-allow would satisfy the assertion above.
	var lastRetry int
	blocked := false
	for i := range limit + 3 {
		ok, retry, err := rl.Allow("recover-key", limit, windowMs)
		if err != nil {
			t.Fatalf("call %d after recovery: %v", i, err)
		}
		if !ok {
			blocked, lastRetry = true, retry
			break
		}
	}
	if !blocked {
		t.Errorf("no request was refused within %d calls of a limit-%d window — the limiter recovered into always-allow", limit+3, limit)
	}
	// retryAfter is clamped to >= 1 in the limiter, so asserting that alone
	// cannot fail. What is worth holding is that a refusal carries a usable
	// hint rather than a wildly wrong one: the window is 60s here.
	if blocked && (lastRetry < 1 || lastRetry > 61) {
		t.Errorf("retryAfter=%d, want within (0, 61] for a 60s window", lastRetry)
	}
}

// The script the limiter DISPATCHES must be the one the constructor actually
// loaded into the server. If those diverge, the startup probe stops proving
// anything about what Allow runs.
//
// The first version of this compared rl.script.Hash() to a SHA the TEST
// obtained by loading SlidingWindowLua itself — both sides derived from one
// constant, so it asserted sha1(X) == sha1(X) and stayed green when the
// constructor was mutated to probe a different script entirely. Asking the
// SERVER whether the dispatched script is present is what closes that: only the
// constructor can have put it there.
func TestRedisLimiter_ConstructorLoadsTheScriptAllowDispatches(t *testing.T) {
	_, rdb := newMiniRedis(t)
	rl := NewRedisLimiter(rdb, discardLogger())
	if rl == nil {
		t.Fatal("setup: limiter nil")
	}
	exists, err := rdb.ScriptExists(t.Context(), rl.script.Hash()).Result()
	if err != nil {
		t.Fatalf("SCRIPT EXISTS: %v", err)
	}
	if len(exists) != 1 || !exists[0] {
		t.Errorf("the server does not hold the script Allow dispatches (%s) after construction — the startup probe validated something else",
			rl.script.Hash())
	}
}
