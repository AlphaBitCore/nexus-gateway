package audit

import (
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// auditMetrics owns the audit-pipeline opsmetrics counters. Names use the
// shared dotted convention (audit.mq_*) and are not part of the spec
// catalog (§6.3) — they are AI-Gateway-specific MQ-pipeline counters that
// stay observable on /metrics and are also pushed to Hub via the registry.
type auditMetrics struct {
	enqueueTotal  *opsmetrics.CounterPin
	enqueueErrors *opsmetrics.CounterPin
	dropped       *opsmetrics.CounterPin
	spilled       *opsmetrics.CounterPin
}

func newAuditMetrics(reg *opsmetrics.Registry) *auditMetrics {
	if reg == nil {
		return nil
	}
	// No labels today — single audit pipeline per process. The pin pattern
	// still applies; With() with zero values returns a CounterPin bound to
	// the empty label set.
	return &auditMetrics{
		enqueueTotal:  reg.NewCounter("audit.mq_enqueue_total", nil).With(),
		enqueueErrors: reg.NewCounter("audit.mq_enqueue_errors_total", nil).With(),
		dropped:       reg.NewCounter("audit.mq_dropped_total", nil).With(),
		spilled:       reg.NewCounter("audit.mq_spilled_total", nil).With(),
	}
}

func (m *auditMetrics) incEnqueueTotal() {
	if m != nil {
		m.enqueueTotal.Inc()
	}
}
func (m *auditMetrics) incEnqueueErrors() {
	if m != nil {
		m.enqueueErrors.Inc()
	}
}
func (m *auditMetrics) incDropped() {
	if m != nil {
		m.dropped.Inc()
	}
}
func (m *auditMetrics) incSpilled() {
	if m != nil {
		m.spilled.Inc()
	}
}
