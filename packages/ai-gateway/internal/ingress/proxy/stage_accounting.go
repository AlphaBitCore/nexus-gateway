// stage_accounting.go — the accounting tail of the proxy stage chain:
// centralized audit + latency finalization, registered as a defer by the
// ServeProxy driver so it runs on every exit path after the chain stops.
package proxy

import (
	"github.com/goccy/go-json"
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
	s.stampNormalizeGate()
	s.stampRequestNormalizedReuse()
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

// stampNormalizeGate decides, per direction, whether the async audit writer must
// produce the normalized projection at write-time, and stamps the deferral onto
// the Record (lazy-audit-normalize). A direction is deferred only when the
// NEXUS_LAZY_AUDIT_NORMALIZE flag is on AND no write-time consumer needs the
// normalized text for that direction:
//
//   - request_normalized deferred ⟺ the request canonical was NOT materialized
//     (no smart-rule match and cache off) AND no stage-request hook is active.
//     When the canonical WAS materialized (smart routing / cache pulled it) the
//     audit reuses it for free; when a request hook is active the writer must
//     normalize fresh because redaction spans reference the projection.
//   - response_normalized deferred ⟺ no stage-response hook is active AND cache
//     is off (the cache response pipeline / L2 path needs the projection).
//
// Hooks force write-time normalize per direction because redaction spans are
// direction-scoped and cannot be faithfully recomputed on view (the hook
// decision may be non-deterministic). When the flag is OFF this is a no-op and
// both directions normalize as before (byte-identical legacy path). The
// canonical-computed signal is read WITHOUT triggering the lazy compute, so the
// gate never forces the work it exists to skip.
func (s *proxyState) stampNormalizeGate() {
	if s.h == nil || !s.h.lazyAuditNormalize {
		return
	}
	// The normalized projection is NEVER persisted for audit: the control plane
	// recomputes it at view time from the stored raw body. Redaction applies to
	// the payload (the raw body is masked in storage), so the view-time recompute
	// reads already-redacted bytes and is PII-safe without a stored projection —
	// shipping request_normalized / response_normalized on every audit message is
	// pure publish-frame + storage + CPU waste. Write-time normalize is forced
	// only for a genuine write-time consumer of the projection itself:
	//   - request: a smart-routing/cache canonical was already materialized, so
	//     the audit reuses it for free (stampRequestNormalizedReuse) — no extra
	//     compute.
	//   - response: the cache pipeline needs the projection at write time.
	// Compliance hooks do NOT force it: a hook inspects content via its own
	// lightweight extraction and records its decision/spans on the row, and the
	// projection it would have persisted is identical to the view-time recompute.
	cacheEnabled := s.h.deps != nil && s.h.deps.Cache != nil && s.h.deps.Cache.IsEnabled()
	_, canonicalComputed := s.rctxFull.NormalizedIfComputed()
	if !canonicalComputed {
		s.rec.SkipRequestNormalize = true
	}
	if !cacheEnabled {
		s.rec.SkipResponseNormalize = true
	}
}

// stampRequestNormalizedReuse stamps the already-materialized request canonical
// onto the Record so the async writer reuses it as request_normalized instead of
// re-Normalizing the raw body. The Meta was aligned at compute time, so these
// bytes are byte-identical to a fresh re-Normalize.
//
// It reads NormalizedIfComputed — it never TRIGGERS the lazy compute. The
// canonical is reused only when a request-time consumer (smart routing / cache)
// already produced it; otherwise the request direction either re-normalizes in
// the writer (hooks/legacy) or is deferred (lazy-audit-normalize), both handled
// downstream. Marshaling synchronously here (the live payload is not retained
// past this point) avoids aliasing races with the async writer. A marshal error
// leaves the reuse fields zero, so recordToMessage falls back to re-Normalize.
func (s *proxyState) stampRequestNormalizedReuse() {
	if !s.h.lazyCanonical || s.rctxFull == nil || len(s.rec.RequestBody) == 0 {
		return
	}
	if s.rec.SkipRequestNormalize {
		return
	}
	np, ok := s.rctxFull.NormalizedIfComputed()
	if !ok {
		return
	}
	b, err := json.Marshal(np)
	if err != nil {
		return
	}
	s.rec.RequestNormalizedReuse = b
	s.rec.RequestNormalizedProtocol = np.Protocol
	s.rec.RequestNormalizedKind = string(np.Kind)
}
