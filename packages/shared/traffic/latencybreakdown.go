package traffic

// LatencyBreakdown is the typed wrapper over the `traffic_event.latency_breakdown`
// JSONB column. Keys are phase names; values are integer durations in
// milliseconds, EXCEPT keys ending in `_us` (the sub-millisecond hook framing
// segments — hook_extract_us / hook_build_us / hook_pipeline_us /
// hook_rewrite_us), whose values are microseconds so they retain resolution
// instead of flooring to 0/1.
//
// Producers should not construct LatencyBreakdown directly — go through
// PhaseTimer.Snapshot() to keep the closed enum invariant. Consumers (Hub
// writer, control plane reader, UI rendering) may construct or inspect it
// freely.
type LatencyBreakdown map[string]int

// Get reads a phase value by typed Phase key. Returns (value, true) when the
// key is present, (0, false) otherwise.
func (lb LatencyBreakdown) Get(p Phase) (int, bool) {
	if lb == nil {
		return 0, false
	}
	v, ok := lb[string(p)]
	return v, ok
}

// Set writes a phase value by typed Phase key. Zero or negative values are
// omitted to keep the JSONB compact (consistent with PhaseTimer.Snapshot).
func (lb LatencyBreakdown) Set(p Phase, ms int) {
	if lb == nil || p == "" {
		return
	}
	if ms <= 0 {
		delete(lb, string(p))
		return
	}
	lb[string(p)] = ms
}

// MarkStreamAborted sets the conventional `stream_aborted = 1` marker that
// signals the upstream_total_ms reflects a client-side abort rather than a
// natural end-of-stream. The value 1 is chosen so the field marshals into
// JSONB as an integer (the column is an int-map at runtime).
func (lb LatencyBreakdown) MarkStreamAborted() {
	if lb == nil {
		return
	}
	lb[string(PhaseStreamAborted)] = 1
}
