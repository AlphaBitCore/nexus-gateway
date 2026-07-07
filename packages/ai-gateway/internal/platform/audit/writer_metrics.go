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
	// reingested counts records the spill-recovery sweeper replayed from durable
	// spool files back into the MQ queue. spilled - reingested ≈ records still on
	// disk awaiting recovery (a backlog gauge for the spill-defer path).
	reingested *opsmetrics.CounterPin
	// recoveryErrors counts spill-recovery publish failures (a file left for the
	// next pass). Distinct from enqueueErrors (the live publish path).
	recoveryErrors *opsmetrics.CounterPin
	// recoveryPoisoned counts spilled records dead-lettered to a .poison sidecar
	// because they exceed the broker max_payload and can never publish. A non-zero
	// value means audit data is durably retained but NOT in the queryable store —
	// an operator signal to fix the inline-body cap / enable out-of-band body spill.
	recoveryPoisoned *opsmetrics.CounterPin
	// spillBackpressure counts how many times the spill worker entered a quota-full
	// back-pressure retry (spool at its total-size quota under a no-loss mode). It is
	// the DISTINCT "spool full → throttling the request path, recovery cannot keep up"
	// signal: a rising value with dropped_total flat at 0 means the gateway is
	// deliberately back-pressuring (not dropping, not hung) because NATS/Hub/PG drain
	// < audit ingest. On-call uses it to tell an audit-spool wedge from an app hang.
	spillBackpressure *opsmetrics.CounterPin
	// memBackpressure counts how many times Enqueue found the in-memory byte
	// budget exhausted. No-loss modes then BLOCK the request goroutine until drain
	// frees bytes; lossy modes shed with a counted drop (this counter is what
	// distinguishes a budget-full drop from a plain queue-full drop). Rising with
	// dropped_total flat at 0 = the gateway is deliberately throttling ingest
	// because in-memory audit bytes hit their RAM share — the designed overload
	// behavior (bounded memory, no loss), not a hang.
	memBackpressure *opsmetrics.CounterPin
}

func newAuditMetrics(reg *opsmetrics.Registry) *auditMetrics {
	if reg == nil {
		return nil
	}
	// No labels today — single audit pipeline per process. The pin pattern
	// still applies; With() with zero values returns a CounterPin bound to
	// the empty label set.
	return &auditMetrics{
		enqueueTotal:      reg.NewCounter("audit.mq_enqueue_total", nil).With(),
		enqueueErrors:     reg.NewCounter("audit.mq_enqueue_errors_total", nil).With(),
		dropped:           reg.NewCounter("audit.mq_dropped_total", nil).With(),
		spilled:           reg.NewCounter("audit.mq_spilled_total", nil).With(),
		reingested:        reg.NewCounter("audit.mq_reingested_total", nil).With(),
		recoveryErrors:    reg.NewCounter("audit.mq_recovery_errors_total", nil).With(),
		recoveryPoisoned:  reg.NewCounter("audit.mq_recovery_poisoned_total", nil).With(),
		spillBackpressure: reg.NewCounter("audit.mq_spill_backpressure_total", nil).With(),
		memBackpressure:   reg.NewCounter("audit.mem_backpressure_total", nil).With(),
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
func (m *auditMetrics) addSpilled(n int) {
	if m != nil && n > 0 {
		m.spilled.Add(float64(n))
	}
}
func (m *auditMetrics) addDropped(n int) {
	if m != nil && n > 0 {
		m.dropped.Add(float64(n))
	}
}
func (m *auditMetrics) addReingested(n int) {
	if m != nil && n > 0 {
		m.reingested.Add(float64(n))
	}
}
func (m *auditMetrics) incRecoveryErrors() {
	if m != nil {
		m.recoveryErrors.Inc()
	}
}
func (m *auditMetrics) addPoisoned(n int) {
	if m != nil && n > 0 {
		m.recoveryPoisoned.Add(float64(n))
	}
}
func (m *auditMetrics) incSpillBackpressure() {
	if m != nil {
		m.spillBackpressure.Inc()
	}
}
func (m *auditMetrics) incMemBackpressure() {
	if m != nil {
		m.memBackpressure.Inc()
	}
}
