package responseprehook

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// normalizePanicTotal counts panics recovered inside the response
// pre-hook callback. Two locations carry recover() guards for
// panic-safety: the Registry.Normalize call and the OnPayload
// caller-supplied stamp closure. The label `location` distinguishes
// them so admins can tell whether the bug is in normalize codecs or
// in the per-service audit-stamp closure.
//
// Without this counter the WARN log was the only signal — easy to
// miss in a busy log stream.
//
// Single shared registration covers all three data planes because
// every service routes its pre-hook through responseprehook.Build;
// Prometheus job/instance labels distinguish service in scrape
// output.
var normalizePanicTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "nexus_normalize_panic_total",
	Help: "Count of response pre-hook panics recovered to keep the SSE pipeline alive.",
}, []string{"location"})

// recordPanic bumps the counter for the given location. Exported as
// a package-private helper so the two recover sites stay symmetric.
func recordPanic(location string) {
	normalizePanicTotal.WithLabelValues(location).Inc()
}

// prehookNormalizeDropTotal counts non-panic normalize failures —
// Registry.Normalize returned a non-nil error (ErrUnsupported or a
// tier hard-error), so the pre-hook callback dropped without
// stamping ci.Normalized. Downstream hook executors then see the
// flat-text fallback payload buildCheckpointInput produces, not the
// structured Normalized claim. Without this counter the silent-drop
// path is invisible — admins running
// Modify hooks on flat text would never know they're operating on
// degraded input.
//
// The label is the adapter id (lowercased — same key Registry uses
// for routing) so dashboards can surface "which provider's normalize
// is failing" without scraping logs.
var prehookNormalizeDropTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "nexus_prehook_normalize_drop_total",
	Help: "Count of pre-hook Registry.Normalize calls that returned non-nil error; callback dropped without stamping ci.Normalized (hooks see flat-text fallback).",
}, []string{"adapter"})

// recordNormalizeDrop bumps the silent-drop counter. Called from
// Build's callback when normalizeWithRecover returns a non-nil err.
func recordNormalizeDrop(adapter string) {
	prehookNormalizeDropTotal.WithLabelValues(adapter).Inc()
}
