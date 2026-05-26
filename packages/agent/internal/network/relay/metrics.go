// Package relay holds the agent's outbound HTTP client used by the MITM
// relay and a small helper for the mTLS-bootstrap path.
package relay

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// metricsBundle holds the relay's L3 instruments. Both counters live in the
// caller-supplied registry.Registry so the same instrument is scraped via
// /metrics AND emitted as a metrics_sample to Hub on every heartbeat.
type metricsBundle struct {
	// dials is the per-(host, mode) outbound HTTP request connection
	// counter. mode is "reused" (HTTP/2 multiplex or HTTP/1 idle-pool
	// reuse) or "new" (freshly dialed).
	dials *registry.Counter

	// handshakes counts TLS handshakes initiated by this relay client. The
	// signal is sourced from httptrace.TLSHandshakeStart, so each dial that
	// reaches the TLS phase increments it exactly once. With HTTP/2
	// multiplex the ratio dial_total{mode=new}/handshake_total over time is
	// expected to be ~1; a sustained mismatch indicates a TLS-failure /
	// dial-rejection regression.
	handshakes *registry.Counter
}

// newMetrics returns a metricsBundle bound to opsReg. opsReg must be
// non-nil; callers in tests pass a fresh registry.NewRegistry(...) so
// each test gets isolated counters.
func newMetrics(opsReg *registry.Registry) *metricsBundle {
	return &metricsBundle{
		dials:      opsReg.NewCounter("relay.dial_total", []string{"host", "mode"}),
		handshakes: opsReg.NewCounter("relay.handshake_total", []string{}),
	}
}
