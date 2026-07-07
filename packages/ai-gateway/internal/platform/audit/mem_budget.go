// mem_budget.go — the audit Writer's in-memory byte budget: the config surface
// (WithMemMaxBytes / MemBudgetBytes) and the exactly-once release helper the
// terminal sites call. The admission (reserve) side lives in Enqueue
// (backpressure.go), which owns the whole buffer-admission path.
package audit

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bytebudget"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// memPinWarnFraction is the pinned-budget share of available RAM past which
// WithMemMaxBytes warns. The budget accounts only raw body bytes; the process's
// REAL memory is roughly 2× the pinned value (Go GC headroom at GOGC=100, plus
// marshal/compression/frame copies) on a box that also hosts the NATS memory
// stream (~15% RAM) — a 10 GB pin on a 32 GB box OOM-killed the gateway at
// 15.9 GB RSS in rig validation. 25% keeps 2×pin + NATS + PG under the ceiling.
const memPinWarnFraction = 0.25

// WithMemMaxBytes sets the in-memory audit byte budget from config. Semantics
// mirror NEXUS_EVENTS_MAX_BYTES exactly: empty or "auto" keeps the auto default
// (~15% of available RAM, set in NewWriter); an explicit human size ("4GB",
// "2048MB", raw bytes) pins it; an unparseable value keeps the auto default so a
// typo can never disable the OOM bound. A pin above memPinWarnFraction of
// available RAM logs a WARN (never clamps — the operator stays in charge, but an
// OOM-risky value must not pass silently). Call before Start / any Enqueue
// (rebinds the budget, matching the other With* startup-only options). Returns
// the receiver.
func (w *Writer) WithMemMaxBytes(v string) *Writer {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "auto") {
		return w
	}
	n := mq.ParseByteSize(v, 0)
	if n <= 0 {
		return w
	}
	if avail := availableMemoryBytes(); avail > 0 && n > int64(float64(avail)*memPinWarnFraction) {
		w.logger.Warn("audit memory budget pinned above the safe share of available RAM — "+
			"real process memory runs ~2x the budget (GC headroom + marshal copies), OOM risk "+
			"alongside the NATS memory stream; prefer <=20% of RAM",
			"pinned_bytes", n, "available_bytes", avail,
			"pinned_fraction", float64(n)/float64(avail))
	}
	w.memBudget = bytebudget.New(n, w.stopCh)
	return w
}

// MemBudgetBytes reports the resolved in-memory byte budget, so startup wiring
// can log what is actually in effect (auto-sized or pinned).
func (w *Writer) MemBudgetBytes() int64 { return w.memBudget.Budget() }

// releaseRecordMem returns rec's byte-budget reservation, exactly once
// (idempotent: the first call zeroes reserved, later calls no-op). Call at every
// TERMINAL — the points where a record leaves the in-memory pipeline: published
// OK, durably spilled, or dropped. A missed release leaks budget (permanent
// over-back-pressure → ingest stall); a double release inflates it (re-opens the
// OOM). The zeroing here guards the double; the per-terminal call sites guard
// the miss. Records batched by the spill worker transfer their reservation to
// the batch aggregate instead (spillLoop's bufReserved).
func (w *Writer) releaseRecordMem(rec *Record) {
	if rec == nil || rec.reserved == 0 {
		return
	}
	w.memBudget.Release(rec.reserved)
	rec.reserved = 0
}
