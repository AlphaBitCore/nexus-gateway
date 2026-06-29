// Package traffic provides shared utilities for `traffic_event` instrumentation
// across all forwarding services (ai-gateway, compliance-proxy, agent).
//
// PhaseTimer captures named per-phase durations on a single request lifetime.
// LatencyBreakdown is the typed wrapper for the JSONB long-tail-phase column
// (`traffic_event.latency_breakdown`).
//
// The phase enum is closed: every accepted key is declared as a Phase constant
// in this file. Producers must not pass arbitrary strings to Mark/MarkBetween;
// the Phase typedef makes this a compile-time check.
package traffic

import (
	"strings"
	"sync"
	"time"
)

// Phase is the closed enum of phase keys persisted into
// `traffic_event.latency_breakdown` JSONB. Per-source applicability:
//
//	ai-gateway       → PhaseAuth, PhaseQuota, PhaseRouting, PhaseCacheLookup,
//	                   PhaseReqAdapter, PhaseRespAdapter, PhaseHookExtract,
//	                   PhaseHookBuild, PhaseHookPipeline, PhaseHookRewrite
//	compliance-proxy → PhaseConnSetup, PhaseTLSHandshake
//	agent            → PhaseIntercept
//
// A Phase used by a service it does not apply to is not enforced at the type
// level — services must respect their own scope. Phase names match the JSONB
// key strings exactly so they marshal one-to-one.
type Phase string

const (
	// ai-gateway phases.
	PhaseAuth        Phase = "auth_ms"
	PhaseQuota       Phase = "quota_ms"
	PhaseRouting     Phase = "routing_ms"
	PhaseCacheLookup Phase = "cache_lookup_ms"
	PhaseReqAdapter  Phase = "req_adapter_ms"
	PhaseRespAdapter Phase = "resp_adapter_ms"
	// PhaseBodyRead is the time spent reading the client request body
	// into memory (network ingress + io.LimitReader). Surfaces slow
	// trans-continental clients separately from "gateway compute".
	PhaseBodyRead Phase = "body_read_ms"
	// PhaseNormUpstream covers the L3+L4 normalisation stage (strip
	// volatile bytes + inject cache markers for Anthropic / Bedrock /
	// Gemini). Distinct from PhaseReqAdapter (which is the
	// canonical-shape PrepareBody encode).
	PhaseNormUpstream Phase = "norm_upstream_ms"
	// PhaseUpstreamBody is the gap between TTFB and the last byte read
	// from upstream — for non-streaming, the JSON body read window;
	// for streaming, TTFB → last chunk arrival. Lets analytics
	// separate "upstream slow to first byte" from "upstream slow to
	// stream completion".
	PhaseUpstreamBody Phase = "upstream_body_ms"
	// PhaseAuditEmit is the time spent building + enqueuing the audit
	// message at request end (the work inside h.finalize). Useful for
	// triaging "why is request latency >> upstream + our overhead?"
	// scenarios where the audit emit path is the slow link.
	PhaseAuditEmit Phase = "audit_emit_ms"

	// Request-hooks stage sub-phases. RequestHooksMs (the per-hook
	// self-timing sum) only covers the hooks' own Execute time; the framing
	// work (content extraction, pipeline build, body rewrite) was previously
	// untimed and unrepresented in latency_breakdown. A hook whose cost
	// scales with body size (e.g. a full-body PII scan over a large prompt)
	// would register near-zero in request_hooks_ms while its real cost lands
	// in the framing segments; these keys make each segment attributable.
	// (For small bodies these segments are sub-millisecond.)
	//
	//	PhaseHookExtract — adapter.ExtractRequest content extraction that
	//	                   produces the Normalized payload hooks consume.
	//	PhaseHookBuild   — resolver.BuildPipeline (config resolve + hook
	//	                   instantiation).
	//	PhaseHookPipeline — pipeline.Execute wall-clock. Named "pipeline"
	//	                   (not "exec") to distinguish it from the
	//	                   request_hooks_ms column, which sums each hook's
	//	                   own self-timing; this is the whole-Execute
	//	                   wall-clock and is ≥ that sum (the gap is
	//	                   in-pipeline framework overhead between hooks).
	//	PhaseHookRewrite — adapter.RewriteRequestBody reverse-encode on a
	//	                   Modify decision (0 when no rewrite occurs).
	// Recorded in MICROSECONDS (`_us` suffix), not milliseconds: for normal-sized
	// prompts these framing segments are sub-millisecond, so a millisecond column
	// floored them to 0 (dropped) or 1 — making per-request hook timing
	// unreadable. Microsecond resolution makes them accurately attributable. The
	// JSONB value stays an integer (Hub deserialises latency_breakdown as
	// map[string]int; CP-UI reads Record<string, number>), so this is additive —
	// no existing `_ms` consumer is affected.
	PhaseHookExtract  Phase = "hook_extract_us"
	PhaseHookBuild    Phase = "hook_build_us"
	PhaseHookPipeline Phase = "hook_pipeline_us"
	PhaseHookRewrite  Phase = "hook_rewrite_us"

	// compliance-proxy phases.
	PhaseConnSetup    Phase = "conn_setup_ms"
	PhaseTLSHandshake Phase = "tls_handshake_ms"

	// agent phases.
	PhaseIntercept Phase = "intercept_ms"

	// PhaseStreamAborted is a marker (value always = 1) set when a client
	// disconnects mid-stream, so analytics can isolate aborted streams when
	// the `upstream_total_ms` value reflects an abort instant rather than a
	// natural end-of-stream.
	PhaseStreamAborted Phase = "stream_aborted"
)

// PhaseTimer records named phase durations against a single request lifetime.
//
// Usage:
//
//	t := traffic.NewPhaseTimer()
//	// ... do auth work
//	t.Mark(traffic.PhaseAuth)
//	// ... do quota work
//	t.Mark(traffic.PhaseQuota)
//	// ... when ready to persist
//	breakdown := t.Snapshot()  // map[string]int suitable for JSONB
//
// Mark records the elapsed time since the previous Mark call (or NewPhaseTimer
// for the first call). MarkBetween records an arbitrary duration when the
// phase's start is not the previous Mark (e.g. TTFB recorded from an
// httptrace.ClientTrace callback).
//
// Concurrency: a single PhaseTimer is owned by one request goroutine and is
// not safe for concurrent Mark calls. The internal mutex protects against
// reordering only when a producer needs to MarkBetween from a callback while
// the request goroutine is still alive; that is the only supported
// cross-goroutine call (e.g. httptrace.GotFirstResponseByte).
type PhaseTimer struct {
	mu       sync.Mutex
	start    time.Time
	prevMark time.Time
	phases   map[Phase]time.Duration
}

// NewPhaseTimer constructs a PhaseTimer with both `start` and `prevMark` set
// to the current time. Returned PhaseTimer is ready to record phases.
func NewPhaseTimer() *PhaseTimer {
	now := time.Now()
	return &PhaseTimer{
		start:    now,
		prevMark: now,
		phases:   make(map[Phase]time.Duration, 8),
	}
}

// Mark records the duration since the previous Mark (or NewPhaseTimer if no
// prior Mark) against `name`, and resets the previous-mark reference. Returns
// the recorded duration so callers can log it alongside other request data
// without a separate Phases() lookup.
//
// If `name` is the empty string, Mark is a no-op (returns 0) — useful when
// instrumenting an optional phase that doesn't get a stored key for some
// branches (e.g. cache_lookup_ms when cache is disabled).
func (p *PhaseTimer) Mark(name Phase) time.Duration {
	if p == nil || name == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	d := now.Sub(p.prevMark)
	if d < 0 {
		d = 0
	}
	p.phases[name] += d
	p.prevMark = now
	return d
}

// MarkBetween records an explicit duration against `name`. Use when the
// phase's start is not the previous Mark — for example, when recording TTFB
// from an httptrace.ClientTrace callback whose "start" is the upstream send
// time saved by the caller, not the previous phase boundary.
//
// MarkBetween does NOT advance the prevMark cursor, because the explicit
// duration is orthogonal to the linear request lifecycle.
func (p *PhaseTimer) MarkBetween(name Phase, d time.Duration) {
	if p == nil || name == "" {
		return
	}
	if d < 0 {
		d = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phases[name] += d
}

// SetMs is a convenience for instrumentation paths that already have an
// integer millisecond value (e.g. derived from a Prometheus histogram-friendly
// time.Since(...).Milliseconds() the caller already computed). It overwrites
// any prior value for `name`.
func (p *PhaseTimer) SetMs(name Phase, ms int) {
	if p == nil || name == "" {
		return
	}
	if ms < 0 {
		ms = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phases[name] = time.Duration(ms) * time.Millisecond
}

// Snapshot returns a copy of the recorded phases as a map[string]int (ms).
// Zero-valued phases are omitted so the JSONB payload stays compact.
// The returned map is safe to mutate independently of the timer.
//
// Equivalent to SnapshotDetail(false).
func (p *PhaseTimer) Snapshot() map[string]int {
	return p.SnapshotDetail(false)
}

// SnapshotDetail mirrors Snapshot but optionally surfaces sub-millisecond
// phases as `1` (instead of dropping them) when `detail` is true. Used by
// the ai-gateway `observability.latency_detail` yaml flag so operators
// running a perf-investigation can see every phase mark — even ones that
// rounded to 0ms — without losing the compact-row default for normal ops.
// Phase keys whose value was never set (the timer's map default) are still
// omitted; only registered phases that recorded < 1ms get the floor.
func (p *PhaseTimer) SnapshotDetail(detail bool) map[string]int {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.phases) == 0 {
		return nil
	}
	out := make(map[string]int, len(p.phases))
	for k, d := range p.phases {
		// Unit is encoded in the phase-name suffix: `_us` phases (the sub-ms
		// hook framing segments) emit microseconds; all others emit milliseconds.
		var v int
		if strings.HasSuffix(string(k), "_us") {
			v = int(d / time.Microsecond)
		} else {
			v = int(d / time.Millisecond)
		}
		switch {
		case v > 0:
			out[string(k)] = v
		case detail && d > 0:
			// Floor sub-resolution phases to 1 so the JSONB row carries the
			// fact they ran, even though the resolution is too coarse to show
			// their actual duration. Sub-unit noise (d <= 0) still drops.
			out[string(k)] = 1
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Elapsed returns total time since the timer started. Useful for the
// top-level latency_ms field that callers already capture today; the timer
// is the single time source so callers don't drift between phase totals and
// the row's total latency.
func (p *PhaseTimer) Elapsed() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Since(p.start)
}
