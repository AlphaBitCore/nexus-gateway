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
// Three-service unification (#115/R3): all three data planes
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
