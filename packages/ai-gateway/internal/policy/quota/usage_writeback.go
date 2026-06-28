package quota

import (
	"context"
	"sync"
	"time"
)

// usageDelta is one accumulated per-key write-behind entry: the summed cost and
// the periodKey that key's Expire must be re-applied against on flush.
type usageDelta struct {
	cents     int64
	periodKey string
}

// usageAggregator is the in-process write-behind accumulator for quota cost
// increments. Instead of one Redis pipeline (IncrBy+Expire) per request, the
// hot path adds the cost to a local per-key counter; a background flusher drains
// the accumulated deltas to Redis on a fixed interval. Redis stays the
// authoritative convergent sum: each instance flushes IncrBy(delta), Redis sums
// across all N instances. Soft consistency — bounded overshoot ≤ one flush
// interval of in-flight cost per instance (acceptable for an enterprise-intranet
// quota; the user accepted soft semantics).
//
// The key is the FULL usage key (quota:usage:{targetType}:{targetID}:{periodKey})
// so two periods or two targets never merge; the carried periodKey lets a
// period-boundary flush still re-apply the correct Expire to the (old) key.
//
// drain returns the accumulated deltas and resets to empty. The reset is the
// load-bearing invariant: each interval accumulates from 0, so the flusher's
// IncrBy never double-counts a delta Redis already absorbed (and the read cache
// must never seed the local delta — accumulate from 0, never read-modify-write).
type usageAggregator struct {
	mu     sync.Mutex
	deltas map[string]usageDelta
}

func newUsageAggregator() *usageAggregator {
	return &usageAggregator{deltas: make(map[string]usageDelta)}
}

// RunFlusher drains the write-behind accumulator to Redis every interval until
// ctx is cancelled, then does a FINAL flush so a graceful shutdown loses nothing
// (only a hard crash loses ≤ one interval). Blocks; run in its own goroutine.
// No-op when write-behind is not enabled.
func (c *UsageCache) RunFlusher(ctx context.Context, interval time.Duration) {
	if c.agg == nil {
		return
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain whatever accumulated since the last tick. Use a fresh
			// context so the shutdown flush is not cancelled by the same ctx.
			fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := c.FlushUsage(fctx); err != nil && c.logger != nil {
				c.logger.Error("usage write-behind: shutdown flush failed", "error", err)
			}
			cancel()
			return
		case <-t.C:
			if err := c.FlushUsage(ctx); err != nil && c.logger != nil {
				c.logger.Warn("usage write-behind: periodic flush failed", "error", err)
			}
		}
	}
}

// add accumulates cents against the full usage key. costCents <= 0 or empty key
// is ignored (mirrors IncrMulti's no-op on non-positive cost).
func (a *usageAggregator) add(key, periodKey string, cents int64) {
	if cents <= 0 || key == "" {
		return
	}
	a.mu.Lock()
	d := a.deltas[key]
	d.cents += cents
	d.periodKey = periodKey
	a.deltas[key] = d
	a.mu.Unlock()
}

// drain returns the accumulated deltas and resets the aggregator to empty.
// Returns nil when empty so the flusher does zero Redis work on an idle tick.
func (a *usageAggregator) drain() map[string]usageDelta {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.deltas) == 0 {
		return nil
	}
	out := a.deltas
	a.deltas = make(map[string]usageDelta)
	return out
}
