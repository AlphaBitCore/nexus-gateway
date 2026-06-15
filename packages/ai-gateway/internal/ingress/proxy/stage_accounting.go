// stage_accounting.go — the accounting tail of the proxy stage chain:
// centralized audit + latency finalization, registered as a defer by the
// ServeProxy driver so it runs on every exit path after the chain stops.
package proxy

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// finalizeAudit reads the upstream PhaseSink populated by the singleton
// tracing transport and snapshots the PhaseTimer's long-tail keys into
// rec before enqueueing the audit message. Registered via defer in
// ServeProxy immediately after the state is built, so it covers every
// stage's exit path.
func (s *proxyState) finalizeAudit() {
	deferStart := time.Now()
	s.rec.UpstreamTtfbMs = s.phaseSink.TtfbMs()
	s.rec.UpstreamTotalMs = s.phaseSink.TotalMs()
	// Latency detail toggle: yaml-only operator flag. When true
	// (typically during a perf-investigation window) we surface
	// sub-ms phases as 1 so the row carries evidence of every
	// phase that ran. Default false keeps prod rows compact.
	detail := s.h.deps != nil && s.h.deps.LatencyDetail
	snap := s.phaseTimer.SnapshotDetail(detail)
	// Merge codec-layer stamps from the sink (resp_adapter_ms)
	// into the timer snapshot before persisting.
	for k, v := range s.phaseSink.Breakdown() {
		if snap == nil {
			snap = map[string]int{}
		}
		snap[k] += v
	}
	// upstream_body_ms: gap between TTFB and last-byte received
	// from upstream. Non-streaming: JSON body read window after
	// the first byte. Streaming: TTFB → last SSE chunk arrival
	// (matches phaseTrackedBody.Read stamping in shared/traffic).
	// Lets analytics distinguish "upstream slow to first byte"
	// (TTFB high) from "upstream slow to stream completion"
	// (upstream_body_ms high). Skip when either source is nil
	// — derived columns must not silently zero genuine missing
	// data.
	if s.rec.UpstreamTtfbMs != nil && s.rec.UpstreamTotalMs != nil {
		bodyMs := *s.rec.UpstreamTotalMs - *s.rec.UpstreamTtfbMs
		if bodyMs > 0 || detail {
			if snap == nil {
				snap = map[string]int{}
			}
			if bodyMs <= 0 {
				bodyMs = 1
			}
			snap[string(traffic.PhaseUpstreamBody)] = bodyMs
		}
	}
	// Inline the finalize body so audit_emit_ms can capture the
	// defer-tail cost BEFORE Enqueue hands rec off to the audit
	// writer goroutine (after which mutating rec is racy).
	if s.rec.LatencyMs == 0 {
		us := time.Since(s.start).Microseconds()
		ms := int((us + 999) / 1000)
		if ms < 1 {
			ms = 1
		}
		s.rec.LatencyMs = ms
	}
	// audit_emit_ms: time elapsed in the audit defer up to the
	// Enqueue hand-off. Captures sink reads + snapshot build +
	// LatencyMs compute. The background audit writer's flush
	// time is NOT included (separate goroutine — invisible from
	// this site). Use this column as evidence that the inline
	// emit path isn't the slow link when total >> upstream +
	// our_overhead.
	emitMs := int(time.Since(deferStart).Milliseconds())
	if emitMs > 0 || detail {
		if snap == nil {
			snap = map[string]int{}
		}
		if emitMs <= 0 {
			emitMs = 1
		}
		snap[string(traffic.PhaseAuditEmit)] = emitMs
	}
	s.rec.LatencyBreakdown = snap
	s.h.deps.AuditWriter.Enqueue(s.rec)
}
