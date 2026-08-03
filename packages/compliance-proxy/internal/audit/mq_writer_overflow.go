package audit

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

// Audit-overflow policy for the MQ batch writer.
//
// The DECISION lives in shared/audit/lossmode so all three services agree on it; only the
// execution below is compliance-proxy-specific, because the durable sink here is the NDJSON
// spool while the agent's is its encrypted SQLite queue. The switch in handleOverflow is
// deliberately the same three-arm shape every service uses.
//
// Before this existed, the overflow branch was unconditional: it logged "channel full, writing
// to NDJSON fallback", incremented enqueue_total{destination=ndjson} and set ndjson_active=1,
// and THEN checked whether a spool existed. With NDJSON disabled — which is what
// compliance-proxy.dev.yaml ships — every one of those signals claimed a durable write that
// never happened, and the event was silently discarded. A lying metric on an audit path is
// worse than a missing one: it tells an operator the record was saved.

// overflowBlockMaxWait bounds ActionBlock. Unbounded back-pressure on the audit path would let
// a wedged MQ producer stall request handling indefinitely, which is a worse failure than a
// counted drop — so the wait is bounded and a timeout is counted and logged. The compliance
// proxy is an appliance rather than a host data path, so it can afford a longer wait than the
// agent, whose network-extension path may never stall the user's machine at all.
const overflowBlockMaxWait = 2 * time.Second

// WithLossMode selects the audit-overflow policy. An empty or unrecognised value resolves to
// the no-loss default via lossmode.Resolve — a config typo must never be able to make the audit
// trail silently start dropping records. Call before any Enqueue; returns the receiver.
func (w *MQBatchWriter) WithLossMode(mode string) *MQBatchWriter {
	w.lossMode = lossmode.Resolve(mode)
	return w
}

// LossMode reports the configured policy, for a startup log line and for tests.
func (w *MQBatchWriter) LossMode() lossmode.Mode { return w.lossMode }

// EffectiveLossMode reports the policy actually in force, which differs from the configured one
// when a spooling mode was selected without a spool. Startup logs this so an operator who chose
// spillblock specifically to avoid early stalls can see they are getting block instead.
func (w *MQBatchWriter) EffectiveLossMode() lossmode.Mode {
	if w.ndjson != nil {
		return w.lossMode
	}
	return w.lossMode.WithoutDurableSink()
}

// handleOverflow runs the policy for one event that could not enter the in-memory channel.
func (w *MQBatchWriter) handleOverflow(event AuditEvent) {
	switch w.lossMode.OnOverflow(w.ndjson != nil) {
	case lossmode.ActionSpool:
		w.spoolOverflow(event)
	case lossmode.ActionBlock:
		if !w.blockForRoom(event) {
			w.dropOverflow(event, "back-pressure window expired", nil)
		}
	default: // lossmode.ActionDrop
		w.dropOverflow(event, "loss mode is drop", nil)
	}
}

// spoolOverflow writes the event to the durable NDJSON spool. OnOverflow only returns
// ActionSpool when w.ndjson is non-nil, so the sink is present here by construction.
func (w *MQBatchWriter) spoolOverflow(event AuditEvent) {
	if err := w.ndjson.Write(event); err != nil {
		// The spool itself refused. Under spillblock that must NOT become a drop — the whole
		// point of the mode is that the spool is the primary overflow buffer and the emitting
		// path back-pressures when it saturates. Under spill, a bounded drop is the documented
		// behaviour.
		if w.lossMode == lossmode.SpillBlock && w.blockForRoom(event) {
			return
		}
		w.dropOverflow(event, "durable spool write failed", err)
		return
	}
	// Metrics fire HERE, after a durable write actually happened — not before the attempt.
	if EnqueueTotal != nil {
		EnqueueTotal.With("ndjson").Inc()
	}
	if NDJSONActive != nil {
		NDJSONActive.With().Set(1)
	}
	// Throttled, and the healthy case is why. A sustained MQ backlog with a working
	// spool loses NOTHING, so one WARN per event would flood the log to say that
	// everything is fine — and it would do so on the same disk the spool is writing
	// to. The AI Gateway throttles its equivalent 1-in-2000; this path is bounded
	// per event too, so it uses the same shape.
	if n := w.spoolLogCount.Add(1); n%overflowLogEvery == 1 {
		w.logger.Warn("audit/mq_writer: channel full, events written to the durable NDJSON spool (throttled)",
			"event_id", event.ID,
			"loss_mode", w.lossMode.String(),
			"spooled_so_far", n,
		)
	}
}

// blockForRoom back-pressures until the channel has room, the bound expires, or the writer is
// closing. Reports whether the event made it in.
func (w *MQBatchWriter) blockForRoom(event AuditEvent) bool {
	timer := time.NewTimer(overflowBlockMaxWait)
	defer timer.Stop()
	select {
	case w.ch <- event:
		if EnqueueTotal != nil {
			EnqueueTotal.With("mq").Inc()
		}
		return true
	case <-timer.C:
		return false
	case <-w.done:
		// Closing: the loop's own drain will not see this event, so report failure and let the
		// caller count it rather than blocking shutdown.
		return false
	}
}

// overflowLogEvery throttles both overflow log lines. Chosen to match the AI
// Gateway's dropLogEvery so an operator reading two services' logs is reading the
// same sampling.
const overflowLogEvery = 2000

// dropOverflow discards the event and accounts for it. The counter is not optional — a dropped
// audit record leaves no other trace, so an uncounted drop is invisible data loss.
func (w *MQBatchWriter) dropOverflow(event AuditEvent, reason string, err error) {
	if EnqueueTotal != nil {
		EnqueueTotal.With("dropped").Inc()
	}
	attrs := []any{
		"event_id", event.ID,
		"target_host", event.TargetHost,
		"loss_mode", w.lossMode.String(),
		"effective_loss_mode", w.EffectiveLossMode().String(),
		"reason", reason,
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	// Error, not Warn: on a compliance product a discarded audit record is a data-loss event,
	// and the previous code logged the same situation at Warn while claiming it had been saved.
	// Throttled for the same reason as the spool line, and this one matters more:
	// the condition that produces it (NATS unreachable, no usable spool) is exactly
	// when the box is saturated, and at 1000 rps an unthrottled ERROR per event puts
	// ~1000 lines/second onto the disk the spool needs. The COUNTER is what carries
	// the true rate — it is incremented above, unconditionally, before this line.
	if n := w.dropLogCount.Add(1); n%overflowLogEvery == 1 {
		w.logger.Error("audit/mq_writer: audit events DROPPED on overflow (throttled)",
			append(attrs, "dropped_so_far", n)...)
	}
}
