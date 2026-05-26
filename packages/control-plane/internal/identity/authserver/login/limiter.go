package login

import (
	"strings"
	"sync"
	"time"
)

// Default limiter policy. Matches the old handler's protection budget: ten
// attempts per (ip, email) pair inside a five-minute sliding window is tight
// enough to blunt online password-guessing while leaving room for legitimate
// retries on typos and MFA confusion.
const (
	defaultLimiterWindow = 5 * time.Minute
	defaultLimiterBudget = 10
)

// Limiter is a trivial in-memory sliding-window rate limiter keyed by
// ip+":"+email. It is deliberately not distributed: the auth server is a
// single process in the Control Plane and replicating rate state across
// instances is out of scope for Phase 1. When we scale the Control Plane
// horizontally, replace this with a Redis-backed equivalent.
//
// Old timestamps are evicted inline on each Allow call, so there is no
// separate janitor goroutine. Entries self-prune once they fall outside
// the window and the key stops being queried (the map entry is deleted on
// the next call that visits it).
type Limiter struct {
	window time.Duration
	budget int

	mu      sync.Mutex
	history map[string][]time.Time
	now     func() time.Time // overridden in tests
}

// NewLimiter returns a Limiter with the default policy (10 attempts /
// 5 minutes per (ip, email) pair).
func NewLimiter() *Limiter {
	return newLimiterWith(defaultLimiterWindow, defaultLimiterBudget, time.Now)
}

// newLimiterWith is the internal constructor used by tests to inject a
// deterministic clock and a shorter window.
func newLimiterWith(window time.Duration, budget int, now func() time.Time) *Limiter {
	return &Limiter{
		window:  window,
		budget:  budget,
		history: make(map[string][]time.Time),
		now:     now,
	}
}

// Allow reports whether a login attempt for (ip, email) is permitted under
// the current budget. Every call that returns true records a timestamp;
// calls that return false do not, so a blocked attacker cannot lengthen
// their own window by retrying.
func (l *Limiter) Allow(ip, email string) bool {
	key := ip + ":" + strings.ToLower(strings.TrimSpace(email))
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	hist := l.history[key]
	// Drop timestamps older than the window.
	pruned := hist[:0]
	for _, ts := range hist {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}

	if len(pruned) >= l.budget {
		// Keep the pruned slice so later calls do not re-scan the evicted
		// entries, but do NOT record this failed attempt.
		l.history[key] = pruned
		return false
	}

	pruned = append(pruned, now)
	l.history[key] = pruned
	return true
}
