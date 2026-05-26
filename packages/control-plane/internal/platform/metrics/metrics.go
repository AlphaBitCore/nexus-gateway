// Package metrics owns the control-plane's L3 business instruments.
//
// Instruments are registered against a shared *metricsreg.Registry which
// both:
//
//  1. registers the underlying Prometheus instruments on
//     prometheus.DefaultRegisterer (so /metrics scrapes keep working), and
//  2. records bindings so the per-tick Sampler.Collect() includes them in
//     metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention (spec §6.3 catalog for
// Control Plane). Pre-GA: the old `nexus_control_plane_*` namespace prefix
// is dropped per CLAUDE.md "no backcompat" rule.
//
// route_class label cardinality is bounded by always feeding the Echo route
// template (c.Path()) into the middleware, never the concrete URL — so a
// path like /admin/users/u_abc123 collapses to /admin/users/:id.
package metrics

import (
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

var (
	// RequestsTotal — counter: http.requests_total{method, route_class, status_class}
	RequestsTotal *metricsreg.Counter
	// RequestDurationMs — histogram: http.duration_ms{route_class}
	RequestDurationMs *metricsreg.Histogram
	// AuthAttemptsTotal — counter: auth.attempts_total{result, method}
	// method ∈ {password, sso, apikey, jwt}; result ∈ {success, missing,
	// invalid_jwt, invalid_api_key, error}.
	AuthAttemptsTotal *metricsreg.Counter
	// IAMEvalTotal — counter: iam.eval_total{decision, cache}
	// decision ∈ {allow, deny}; cache ∈ {hit, miss}.
	IAMEvalTotal *metricsreg.Counter
	// AdminAuditLogFailedTotal — counter: admin.audit_log_failed_total{action}
	// Incremented when audit.Writer cannot publish an admin audit entry
	// (marshal failure, MQ enqueue error). Surfaces an ops-visibility gap
	// — the request itself succeeded at Hub, but the audit row didn't
	// land, so on-call needs the metric to drive alerts. Promauto-registered
	// against prometheus.DefaultRegisterer so /metrics scrapes pick it up.
	AdminAuditLogFailedTotal *metricsreg.Counter
)

// Register binds the package-level instruments to the supplied opsmetrics
// registry. Must be called once at process startup before the HTTP server
// starts serving traffic. Safe to call again with the same registry
// (registry registration is idempotent on instrument name).
func Register(reg *metricsreg.Registry) {
	if reg == nil {
		return
	}
	RequestsTotal = reg.NewCounter("http.requests_total", []string{"method", "route_class", "status_class"})
	RequestDurationMs = reg.NewHistogram("http.duration_ms", []string{"route_class"})
	AuthAttemptsTotal = reg.NewCounter("auth.attempts_total", []string{"result", "method"})
	IAMEvalTotal = reg.NewCounter("iam.eval_total", []string{"decision", "cache"})
	AdminAuditLogFailedTotal = reg.NewCounter("admin.audit_log_failed_total", []string{"action"})
}
