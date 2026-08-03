// backpressure_noloss.go — the two waits that implement the no-loss promise.
// `block` and `spillblock` guarantee that a record is never discarded, so every
// escape they take is gated on somewhere durable to escape TO; without one they
// wait for a consumer rather than discard. The lossy policies (`spill`, `drop`)
// and the shared admission path live in backpressure.go.
package audit

import "time"

// blockEnqueue is the no-loss producer path: a BLOCKING send parks the request
// goroutine efficiently (no polling) until a consumer frees a queue slot. The
// number of simultaneously back-pressured goroutines is bounded by the server's
// request concurrency, so it cannot pile up unboundedly. A genuinely wedged
// pipeline (e.g. NATS down) past backpressureMaxWait spills the record durably
// instead of hanging forever — still no loss. That timed escape exists only
// when there is somewhere durable to escape TO; see the no-sink branch below.
func (w *Writer) blockEnqueue(rec *Record) {
	// Fast path: a slot is free, no timer needed.
	select {
	case w.recCh <- rec:
		return
	default:
	}
	if !w.hasDurableSink() {
		// Nowhere durable to escape to. A bounded wait would have to end in a
		// discard, and this mode's whole contract is that it does not discard, so
		// it waits for a consumer instead. Only shutdown ends the wait, and the
		// spill there is the one drop this mode cannot avoid.
		select {
		case w.recCh <- rec:
		case <-w.stopCh:
			w.spillRecord(rec)
		}
		return
	}
	timer := time.NewTimer(backpressureMaxWait)
	defer timer.Stop()
	select {
	case w.recCh <- rec:
	case <-timer.C:
		w.spillRecord(rec) // wedged past the wait → durable spill, no loss
	case <-w.stopCh:
		w.spillRecord(rec) // shutting down → spill durably
	}
}

// backpressureMaxWait bounds how long Enqueue back-pressures on a full buffer
// before falling back to a durable spill, so a genuinely wedged pipeline (NATS
// down) cannot hang a request goroutine forever. Under healthy NATS the flush
// frees space in well under this, so the wait is short and the request path
// simply throttles to the drain rate.
//
// It applies ONLY when a durable sink exists — the escape it bounds is a spill,
// and without a sink that spill is a discard, which the no-loss modes do not do.
// A variable rather than a constant so tests can shrink it and observe which of
// the two waits a given configuration takes; production never assigns it.
var backpressureMaxWait = 10 * time.Second

// hasDurableSink reports whether a record handed to spillRecord will actually
// reach disk. It gates every escape a no-loss mode takes: without a sink,
// spillRecord discards, so an escape that looks like back-pressure relief would
// be a silent discard by another name. perfNoSpill is the ablation switch that
// deliberately turns spilling off, so it counts as no sink.
func (w *Writer) hasDurableSink() bool {
	return !perfNoSpill && w.ndjsonSpill != nil
}
