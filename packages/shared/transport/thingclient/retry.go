package thingclient

import (
	"log/slog"
	"math"
	"math/rand"
	"time"
)

// failureEntry tracks one config key whose most recent apply/pull attempt
// failed, retained with its latest desired state so the bounded retry loop can
// re-attempt it. The state field is the desired ConfigState last seen for the
// key; the retry re-applies the full desired snapshot rather than this single
// entry, but keeping the per-key state makes the registry self-describing for
// status/introspection callers.
type failureEntry struct {
	state ConfigState
}

// reconcileFailures updates the failed-key registry from one apply round, then
// arms or cancels the proactive retry timer.
//
// For every key the round attempted (present in desired): a key that appears in
// reported applied successfully and is cleared; a key missing from reported
// failed (pull/parse/apply error, or no registered handler) and is recorded
// with its latest desired state. Keys NOT in this round's desired are left
// untouched — a per-key delta only carries one key, so prior failures of other
// keys must persist across rounds (this is exactly why moving to per-key
// dispatch needs an explicit retry: the old full-map re-dispatch retried every
// failed key implicitly on each delta).
//
// A non-empty registry holds the global reportedVer behind desiredVer (drift
// stays visible) and arms the retry timer. An empty registry cancels it and
// resets the attempt budget. A brand-new failure also resets the budget so a
// fresh problem gets a full retry allotment even if an older key already
// exhausted its attempts.
func (c *Client) reconcileFailures(desired, reported map[string]ConfigState) {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()

	newlyFailed := false
	for key, cs := range desired {
		if _, ok := reported[key]; ok {
			delete(c.failed, key)
			continue
		}
		if _, existed := c.failed[key]; !existed {
			newlyFailed = true
		}
		c.failed[key] = failureEntry{state: cs}
	}

	// Prune failures for keys no longer in desired state (e.g. a template
	// deleted out from under a key that was mid-failure). A key that is no
	// longer desired cannot be "failing to converge", so leaving it parked
	// would hold reportedVer behind desired_ver forever (false perpetual
	// drift). A key still present but only carried on an unrelated per-key
	// delta this round stays in desiredCache, so it survives the prune.
	if len(c.failed) > 0 {
		desiredKeys := c.snapshotDesiredKeys()
		for key := range c.failed {
			if _, ok := desiredKeys[key]; !ok {
				delete(c.failed, key)
			}
		}
	}

	if len(c.failed) == 0 {
		c.cancelRetryLocked()
		c.retryAttempt = 0
		return
	}
	if newlyFailed {
		c.retryAttempt = 0
	}
	c.armRetryLocked()
}

// snapshotDesiredKeys returns the set of keys currently in desiredCache. Read
// under c.mu; safe to call while holding retryMu (lock order is retryMu → mu,
// and no path holds mu while acquiring retryMu).
func (c *Client) snapshotDesiredKeys() map[string]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]struct{}, len(c.desiredCache))
	for k := range c.desiredCache {
		out[k] = struct{}{}
	}
	return out
}

// outstandingFailures returns the number of config keys currently parked in the
// failure registry. dispatchConfig uses it to decide whether the global
// reportedVer may advance: it may only when this round's apply succeeded AND no
// key (from this or a prior round) is still failing.
func (c *Client) outstandingFailures() int {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	return len(c.failed)
}

// armRetryLocked schedules the next retry fire if the lifecycle is running, no
// timer is already pending, and the attempt budget is not exhausted. Caller
// must hold retryMu.
func (c *Client) armRetryLocked() {
	if !c.running.Load() || c.retryCtx == nil {
		// Client not started (unit tests driving applyConfig directly, or
		// pre-Start). The failure is still recorded for status; there is just
		// no background timer to fire.
		return
	}
	if c.retryTimer != nil {
		return
	}
	if c.retryAttempt >= c.cfg.MaxConfigRetryAttempts {
		c.logger.Warn("config retry budget exhausted; failed keys remain in drift until the next reconnect snapshot",
			slog.Int("failed_keys", len(c.failed)),
			slog.Int("attempts", c.retryAttempt),
		)
		return
	}
	c.retryAttempt++
	delay := c.retryBackoff(c.retryAttempt)
	c.retryTimer = time.AfterFunc(delay, c.fireRetry)
}

// cancelRetryLocked stops and clears the pending retry timer. Caller must hold
// retryMu. Safe to call when no timer is pending.
func (c *Client) cancelRetryLocked() {
	if c.retryTimer != nil {
		c.retryTimer.Stop()
		c.retryTimer = nil
	}
}

// retryBackoff computes the exponential-with-jitter delay for the given attempt
// number, capped at RetryMaxBackoff. attempt is 1-based.
func (c *Client) retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := float64(c.cfg.RetryInitialBackoff) * math.Pow(2, float64(attempt-1))
	maxBackoff := float64(c.cfg.RetryMaxBackoff)
	if backoff > maxBackoff || math.IsInf(backoff, 1) {
		backoff = maxBackoff
	}
	jitter := backoff * 0.25 * rand.Float64()
	return time.Duration(backoff + jitter)
}

// fireRetry is the retry timer callback. It re-applies the FULL current desired
// snapshot at the latest known global version, forcing past the version gate.
//
// A full re-apply (rather than failed-keys-only) is deliberate: on the recovery
// edge — when the last failed key finally applies and the global reportedVer is
// allowed to advance — the complete reported map must land at the advanced
// version. The Hub stores reported with a per-key jsonb merge guarded by a
// monotonic version (a report older than the stored reported_ver is dropped). A
// failed-keys-only retry would advance the version while reporting only those
// keys, leaving a sibling whose earlier held report was dropped permanently
// stale on the Hub. Re-reporting the whole map closes that gap. Retries are
// exceptional (only during a failure window) so the extra applier work is
// acceptable.
func (c *Client) fireRetry() {
	c.retryMu.Lock()
	c.retryTimer = nil
	ctx := c.retryCtx
	pending := len(c.failed)
	c.retryMu.Unlock()

	if ctx == nil || ctx.Err() != nil || pending == 0 {
		return
	}

	desired := c.SnapshotDesired()
	ver := c.desiredVer.Load()
	c.logger.Info("retrying failed config keys",
		slog.String("event", "config_retry"),
		slog.Int("failed_keys", pending),
		slog.Int64("desired_ver", ver),
	)
	// force=true bypasses the version gate; dispatchConfig → reconcileFailures
	// re-arms the next backoff if anything still fails, or cancels the loop and
	// advances reportedVer once everything has converged.
	c.dispatchConfig(desired, ver, true)
}
