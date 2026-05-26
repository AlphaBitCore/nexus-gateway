// Package audit provides audit-event emission paths (MQ + NDJSON fallback +
// SIEM tee) for compliance-proxy.
//
// Metrics are registered against a shared *registry.Registry which both
// registers underlying Prometheus instruments (so /metrics scrapes keep
// working) and records bindings so the per-tick Sampler.Collect() includes
// them in metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention. Pre-GA: the old
// `nexus_compliance_proxy_audit_*` namespace prefix is dropped per CLAUDE.md
// "no backcompat" rule.
package audit

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

var (
	// BatchSize — histogram: audit.batch_size (records per batch flush)
	BatchSize *registry.Histogram
	// BatchLatency — histogram: audit.batch_latency_ms (ms per batch write)
	BatchLatency *registry.Histogram
	// QueueDepth — gauge: audit.queue_depth (current channel occupancy)
	QueueDepth *registry.Gauge
	// EnqueueTotal — counter: audit.enqueue_total{destination}
	EnqueueTotal *registry.Counter
	// WriteErrors — counter: audit.write_errors_total
	WriteErrors *registry.Counter
	// NDJSONWrites — counter: audit.ndjson_writes_total
	NDJSONWrites *registry.Counter
	// NDJSONBytes — counter: audit.ndjson_bytes_total
	NDJSONBytes *registry.Counter
	// NDJSONActive — gauge: audit.ndjson_active (0|1)
	NDJSONActive *registry.Gauge
	// BumpStatusTotal — counter: audit.bump_status_total{status}
	BumpStatusTotal *registry.Counter
	// ComplianceCoverage — gauge: audit.compliance_coverage (0..1)
	ComplianceCoverage *registry.Gauge
)

// Register binds the package-level instruments to the supplied opsmetrics
// registry. Must be called once at process startup before any audit traffic
// is served. Safe to call again with the same registry (registry
// re-registration is idempotent).
func Register(reg *registry.Registry) {
	if reg == nil {
		return
	}
	BatchSize = reg.NewHistogram("audit.batch_size", nil)
	BatchLatency = reg.NewHistogram("audit.batch_latency_ms", nil)
	QueueDepth = reg.NewGauge("audit.queue_depth", nil)
	EnqueueTotal = reg.NewCounter("audit.enqueue_total", []string{"destination"})
	WriteErrors = reg.NewCounter("audit.write_errors_total", nil)
	NDJSONWrites = reg.NewCounter("audit.ndjson_writes_total", nil)
	NDJSONBytes = reg.NewCounter("audit.ndjson_bytes_total", nil)
	NDJSONActive = reg.NewGauge("audit.ndjson_active", nil)
	BumpStatusTotal = reg.NewCounter("audit.bump_status_total", []string{"status"})
	ComplianceCoverage = reg.NewGauge("audit.compliance_coverage", nil)
}
