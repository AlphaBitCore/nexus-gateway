// pureforward.go — startup surface for the NEXUS_PERF_PURE_FORWARD benchmark
// switch: a loud WARN banner and a self-identifying Prometheus gauge so a process
// running with audit disabled is obvious in logs and on dashboards.
package wiring

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
)

// RegisterPureForwardMode registers the pure_forward_mode gauge and, when the
// NEXUS_PERF_PURE_FORWARD benchmark switch is active, sets it to 1 and logs a
// loud WARN banner. The gauge stays 0 in normal operation. Registering it always
// (not only when on) means dashboards and alerts can rely on the series existing.
func RegisterPureForwardMode(reg prometheus.Registerer, logger *slog.Logger) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "ai_gateway",
		Name:      "pure_forward_mode",
		Help:      "1 when NEXUS_PERF_PURE_FORWARD benchmark mode is active (traffic_event audit persistence DISABLED). MUST be 0 in production.",
	})
	reg.MustRegister(g)
	if proxy.PerfPureForward() {
		g.Set(1)
		logger.Warn("PURE-FORWARD BENCHMARK MODE ACTIVE: traffic_event audit persistence is DISABLED (NEXUS_PERF_PURE_FORWARD=1). This MUST NOT run in production — the audit trail is not being written.")
	}
}
