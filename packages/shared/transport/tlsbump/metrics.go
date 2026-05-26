// metrics.go owns the TLS-bump-stage business instruments. Both the
// compliance-proxy CONNECT ingress and the agent NE bridge ingress invoke
// RegisterMetrics(reg) once at process startup against their own
// opsmetrics.Registry — the registry de-dupes registration so the same
// instruments end up bound to the same Prometheus collectors and the
// per-tick metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention. The instruments here are
// only the TLS-bump-stage ones moved out of compliance-proxy's
// internal/metrics package; cp's other instruments (cert cache, redis
// availability, kill-switch, etc.) stay in cp/internal/metrics because they
// are not produced by the shared tlsbump core.
package tlsbump

import (
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

var (
	// TLSHandshakeMs records the server-side TLS-bump handshake duration in
	// milliseconds. Observed once per BumpConnection call (success or fail).
	TLSHandshakeMs *opsmetrics.Histogram

	// UpstreamRequestMs records the upstream HTTP roundtrip duration per
	// bumped request, labeled by host and status. Observed in upstream.go's
	// roundtrip helper.
	UpstreamRequestMs *opsmetrics.Histogram
)

// RegisterMetrics binds the package-level instruments to the supplied
// opsmetrics registry. Safe to call once per process; safe to call from
// either compliance-proxy or agent. Pre-registration leaves the vars nil
// so the bump.go / upstream.go nil-checks fall through and no Observe
// happens — useful in tests that don't wire metrics.
func RegisterMetrics(reg *opsmetrics.Registry) {
	if reg == nil {
		return
	}
	TLSHandshakeMs = reg.NewHistogram("tls.handshake_ms", nil)
	UpstreamRequestMs = reg.NewHistogram("upstream.request_ms", []string{"host", "status"})
}
