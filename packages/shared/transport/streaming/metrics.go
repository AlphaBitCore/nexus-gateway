package streaming

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// modifyDegradedTotal counts response-hook Modify decisions that the
// buffer-mode pipeline ignored. buffer_full_block has no Modify branch
// in Phase 3 (see buffer.go) — when a hook returns Modify the body is
// replayed unchanged. This counter is the single admin-visible signal
// that a configured Modify hook is being silently no-op'd because of
// the streaming-mode choice.
//
// Three-service unification: all three data planes
// (ai-gateway, compliance-proxy, agent) run buffer mode through the
// same shared.BufferPipeline, so this single shared registration
// covers all three. Prometheus scrape job/instance labels distinguish
// which service emitted the increment — keeps the metric name short
// and avoids per-service registration drift.
//
// Label `reason` is forward-looked: today only "buffer_mode" fires;
// future degraded paths (e.g. chunked_async hold-back race) can reuse
// the same metric with a different reason rather than spawn parallel
// names.
var modifyDegradedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "nexus_streaming_modify_degraded_total",
	Help: "Count of response-hook Modify decisions that the streaming pipeline could not honor (rewrite ignored, body replayed verbatim).",
}, []string{"reason"})

// RecordModifyDegraded bumps the modify-degraded counter for the given
// reason. Exported so callers outside this package (e.g. future
// chunked_async degraded paths) can record without re-registering.
func RecordModifyDegraded(reason string) {
	modifyDegradedTotal.WithLabelValues(reason).Inc()
}

// Bounded `cause` label values for modelAEscalationTotal.
const (
	// ModelAEscalationConfirmed: a prescan hit was confirmed as an enforcing
	// (block/redact) action, so the Model A engine handed off to buffer-to-end
	// redaction — the normal enforcement path.
	ModelAEscalationConfirmed = "confirmed"
	// ModelAEscalationMemoryPressure: the held buffer hit MaxBufferBytes while a
	// content unit was still incomplete, so the engine escalated rather than flush
	// it raw — a tuning signal (the stream out-grew the buffer ceiling), distinct
	// from a real policy hit.
	ModelAEscalationMemoryPressure = "memory_pressure"
)

// modelAEscalationTotal counts Model A streaming escalations split by CAUSE so an
// operator can tell a real policy hit ("confirmed") apart from a buffer-ceiling
// eviction ("memory_pressure") — the latter signals MaxBufferBytes may need raising
// for that traffic. This is a log/metric dimension only: the cause is NOT persisted
// on traffic_event (no schema add) and never reuses ResponseHookReasonCode (which the
// canonical-buffer redaction overwrites). Shared across all three data planes; the
// Prometheus job/instance labels distinguish which service emitted it.
var modelAEscalationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "nexus_streaming_modela_escalation_total",
	Help: "Count of Model A streaming escalations, split by cause (confirmed enforcing hit vs memory-pressure buffer eviction).",
}, []string{"cause"})

// RecordModelAEscalation bumps the Model A escalation counter for the given cause
// (use the ModelAEscalation* constants). Exported so both Model A substrates (the
// ai-gateway canonical relay and the tlsbump wire path) record without re-registering.
func RecordModelAEscalation(cause string) {
	modelAEscalationTotal.WithLabelValues(cause).Inc()
}
