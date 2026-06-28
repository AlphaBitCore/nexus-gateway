package credstats

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// statsDelta is one credential's accumulated write-behind stats: an ADDITIVE
// attempt count plus LAST-WRITER-WINS timestamps (the timestamps are not
// counters — only the latest matters, so flush HSets them rather than summing).
type statsDelta struct {
	count  int64
	usedAt string
	okAt   string
}

// statsAggregator accumulates per-credential success stats in-process so the
// hot path skips the per-attempt Redis stats pipeline (HINCRBY cnt + HSet
// used_at/ok_at + SAdd dirty). A background flusher drains to Redis on an
// interval; Redis stays the convergent authority (each instance flushes its
// own delta, HINCRBY sums them). cnt is operational stats only (never billed),
// so a hard-crash loss of ≤ one interval is acceptable.
//
// drain returns the deltas and resets to empty (accumulate-from-0 invariant so
// the flusher's HINCRBY never double-counts a delta Redis already absorbed).
type statsAggregator struct {
	mu     sync.Mutex
	deltas map[string]statsDelta
}

func newStatsAggregator() *statsAggregator {
	return &statsAggregator{deltas: make(map[string]statsDelta)}
}

// EnableWriteBehind installs the T1b stats accumulator so a successful
// RecordAttempt defers its Redis stats pipeline off the request hot path. A
// background flusher (RunFlusher) must then drain it, plus a final FlushStats on
// shutdown to bound loss to one interval. No-op when Redis is absent.
func (b *Buffer) EnableWriteBehind() {
	if b == nil || b.rdb == nil {
		return
	}
	b.agg = newStatsAggregator()
	b.clean = make(map[string]struct{})
}

// FlushStats drains the accumulated success stats to Redis: per credential one
// HINCRBY cnt (additive) + HSet used_at/ok_at (last-writer-wins) + SAdd dirty, in
// a single pipeline — the same end state as the synchronous per-attempt path so
// Redis and the Hub stats-flush job stay correct. Safe on an empty accumulator.
func (b *Buffer) FlushStats(ctx context.Context) error {
	if b == nil || b.agg == nil || b.rdb == nil {
		return nil
	}
	deltas := b.agg.drain()
	if len(deltas) == 0 {
		return nil
	}
	pipe := b.rdb.Pipeline()
	for cred, d := range deltas {
		sk := credstate.StatsKey(cred)
		pipe.HIncrBy(ctx, sk, credstate.StatsFieldCount, d.count)
		if d.usedAt != "" {
			pipe.HSet(ctx, sk, credstate.StatsFieldUsedAt, d.usedAt)
		}
		if d.okAt != "" {
			pipe.HSet(ctx, sk, credstate.StatsFieldOkAt, d.okAt)
		}
		pipe.SAdd(ctx, credstate.StatsDirtySet, cred)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("credstats: write-behind flush: %w", err)
	}
	return nil
}

// RunFlusher drains accumulated stats to Redis every interval until ctx is
// cancelled, then a final flush so graceful shutdown loses nothing (only a hard
// crash loses ≤ one interval of operational stats). Blocks; run in a goroutine.
func (b *Buffer) RunFlusher(ctx context.Context, interval time.Duration) {
	if b == nil || b.agg == nil {
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
			fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := b.FlushStats(fctx); err != nil {
				b.warn("stats write-behind shutdown flush failed", "", err)
			}
			cancel()
			return
		case <-t.C:
			if err := b.FlushStats(ctx); err != nil {
				b.warn("stats write-behind periodic flush failed", "", err)
			}
		}
	}
}

// recordSuccess accumulates one successful attempt: count++ (additive) and
// used_at/ok_at set to nowStr (last-writer-wins).
func (a *statsAggregator) recordSuccess(credentialID, nowStr string) {
	if credentialID == "" {
		return
	}
	a.mu.Lock()
	d := a.deltas[credentialID]
	d.count++
	d.usedAt = nowStr
	d.okAt = nowStr
	a.deltas[credentialID] = d
	a.mu.Unlock()
}

// drain returns the accumulated deltas and resets to empty. Returns nil when
// empty so the flusher does zero Redis work on an idle tick.
func (a *statsAggregator) drain() map[string]statsDelta {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.deltas) == 0 {
		return nil
	}
	out := a.deltas
	a.deltas = make(map[string]statsDelta)
	return out
}
