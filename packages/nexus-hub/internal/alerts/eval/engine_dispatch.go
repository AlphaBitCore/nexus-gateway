package alerteval

// dispatchEntry is one (aggregator, its runtime, bitmask of the sources it
// declares) tuple in the precomputed dispatch table.
type dispatchEntry struct {
	agg        Aggregator
	rt         *Runtime
	sourceMask uint8
}

// sourceBit maps each EventSource to a distinct bit. An unknown source maps to
// 0 (no aggregator matches), the same miss behaviour as aggMatchesSource.
func sourceBit(s EventSource) uint8 {
	switch s {
	case SourceAITraffic:
		return 1 << 0
	case SourceCompliance:
		return 1 << 1
	case SourceAgent:
		return 1 << 2
	case SourceAdminAudit:
		return 1 << 3
	}
	return 0
}

// rebuildDispatchLocked snapshots the current aggregators + runtimes into an
// immutable, source-masked dispatch table for the lock-free per-event path. The
// entries slice is never mutated after Store, so dispatchEvent can range it
// without a lock. Caller MUST hold e.mu.
func (e *Engine) rebuildDispatchLocked() {
	entries := make([]dispatchEntry, 0, len(e.aggregators))
	for id, agg := range e.aggregators {
		var mask uint8
		for _, s := range agg.Sources() {
			mask |= sourceBit(s)
		}
		entries = append(entries, dispatchEntry{agg: agg, rt: e.runtimes[id], sourceMask: mask})
	}
	e.dispatchTable.Store(&entries)
}
