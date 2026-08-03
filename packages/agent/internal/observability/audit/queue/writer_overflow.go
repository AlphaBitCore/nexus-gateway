package queue

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

// Audit-overflow policy for the agent's queue writer.
//
// The DECISION comes from shared/audit/lossmode, so this file's switch is the same three-arm
// shape the AI Gateway and the compliance proxy use. Only the execution is agent-specific, and
// the difference is genuine: the other two spool to an NDJSON file on local disk, whereas the
// agent's durable store is the encrypted SQLite queue THIS writer already feeds. The channel in
// front of it is nothing but a batching buffer, so "spool the overflow" here means "write the row
// to SQLite now instead of waiting for the flush loop".
//
// Before this existed, Enqueue's full-channel branch unconditionally counted a drop and logged a
// WARN. Behaviourally that IS lossmode.Drop — the agent simply had no way to be configured to
// anything else, which is what the owner identified.
//
// # Why the agent's shipped default is spill rather than the no-loss spillblock
//
// CLAUDE.md's safety rule for the agent is that its path must never stall the host: the macOS
// network extension sits in the machine's outbound packet path, and a stalled write there is not a
// slow request but a hung network. A no-loss mode necessarily blocks the emitting goroutine until
// the record is durable, and a SQLite write under WAL contention can wait up to the DSN's 5 s
// busy_timeout — on the agent that is a user-visible freeze caused by AUDIT bookkeeping.
//
// So the agent ships lossmode.Spill: the durable write happens off the caller's goroutine, and a
// drop is counted only when that async path is ALSO saturated. That is exactly what Spill means,
// it keeps the host rule intact, and the no-loss modes remain selectable for a deployment that
// wants them and accepts the stalls. The choice is now configuration rather than a fact baked into
// the code.

const (
	// overflowQueueDepth bounds the async durable-write path. It is deliberately far smaller
	// than the main channel: this is a pressure-relief buffer, not a second queue, and letting
	// it grow large would just move the unbounded-memory problem rather than solve it.
	overflowQueueDepth = 256

	// overflowBlockMaxWait bounds every blocking arm. Nothing on the agent's audit path may
	// wait without a bound — see the host-stall rule above. 250 ms is long enough to ride out
	// a normal SQLite commit and short enough that a user would not perceive it.
	overflowBlockMaxWait = 250 * time.Millisecond
)

// WithLossMode selects the audit-overflow policy. An empty or unrecognised value resolves to the
// shared no-loss default via lossmode.Resolve — a config typo must never be able to make the audit
// trail silently start dropping records. Call before any Enqueue; returns the receiver.
//
// Note the asymmetry with the agent's SHIPPED default: NewQueueWriter selects Spill explicitly for
// the host-path reason documented above, while an unrecognised config value still resolves to the
// safe no-loss default. A deliberate choice and a typo must not land in the same place.
func (w *QueueWriter) WithLossMode(mode string) *QueueWriter {
	w.lossMode = lossmode.Resolve(mode)
	return w
}

// LossMode reports the configured policy, for diagnostics and tests.
func (w *QueueWriter) LossMode() lossmode.Mode { return w.lossMode }

// OverflowWrites reports how many events took the durable overflow path instead of the batching
// channel. Surfaced next to Drops() so an operator can tell "the pipeline is under pressure but
// nothing was lost" from "records are being discarded" — two very different situations that the
// old single drops counter could not distinguish.
func (w *QueueWriter) OverflowWrites() int64 { return w.overflowWrites.Load() }

// startOverflowWorker launches the goroutine that performs the durable writes for the async
// arms. One worker, not a pool: SQLite serialises writers anyway, so extra workers would only
// contend on the same lock while multiplying the memory held in flight.
func (w *QueueWriter) startOverflowWorker() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			// No `ok` check: nothing ever closes overflowCh — shutdown is signalled by done
			// below, and the drain there empties the buffer. A closed-channel arm here
			// would be unreachable defensive code.
			case e := <-w.overflowCh:
				w.writeThrough(e)
			case <-w.done:
				// Drain what is already queued before exiting, so a clean shutdown does not
				// discard rows that were accepted for durable writing.
				for {
					select {
					case e := <-w.overflowCh:
						w.writeThrough(e)
					default:
						return
					}
				}
			}
		}
	}()
}

// writeThrough persists one event directly, bypassing the batching channel.
func (w *QueueWriter) writeThrough(e event.Event) {
	if err := w.queue.RecordBatch([]event.Event{e}); err != nil {
		n := w.drops.Add(1)
		slog.Error("audit writer: durable overflow write FAILED, event lost",
			"event_id", e.ID,
			"target_host", e.TargetHost,
			"loss_mode", w.lossMode.String(),
			"drops_total", n,
			"error", err,
		)
		return
	}
	w.overflowWrites.Add(1)
}

// handleOverflow runs the configured policy for one event the batching channel refused.
//
// The agent's durable sink is always present — it is the SQLite queue this writer was constructed
// with — so hasDurableSink is true whenever the queue is non-nil. That is why the agent never
// takes lossmode's no-sink degradation path in practice.
func (w *QueueWriter) handleOverflow(e event.Event) {
	switch w.lossMode.OnOverflow(w.queue != nil) {
	case lossmode.ActionSpool:
		w.spoolOverflow(e)
	case lossmode.ActionBlock:
		if !w.blockForRoom(e) {
			w.dropOverflow(e, "back-pressure window expired")
		}
	default: // lossmode.ActionDrop
		w.dropOverflow(e, "loss mode is drop")
	}
}

// spoolOverflow hands the event to the async durable writer.
func (w *QueueWriter) spoolOverflow(e event.Event) {
	select {
	case w.overflowCh <- e:
		return
	default:
	}
	// The relief buffer is full too. Under spillblock that must not become a drop — the mode's
	// contract is that saturation causes back-pressure — so wait, bounded, for either the durable
	// path or the main channel to free up. Under spill, a bounded drop is the documented answer.
	if w.lossMode != lossmode.SpillBlock {
		w.dropOverflow(e, "durable overflow buffer saturated")
		return
	}
	timer := time.NewTimer(overflowBlockMaxWait)
	defer timer.Stop()
	select {
	case w.overflowCh <- e:
	case w.ch <- e:
	case <-timer.C:
		// Even spillblock is bounded here, and this is the one place the agent's host rule
		// overrides the mode's no-loss promise. It is counted and logged as the exception it is
		// rather than being papered over.
		w.dropOverflow(e, "spillblock bounded by the agent's no-host-stall rule")
	case <-w.done:
		w.dropOverflow(e, "writer closing")
	}
}

// blockForRoom back-pressures on the batching channel, bounded. Reports whether the event landed.
func (w *QueueWriter) blockForRoom(e event.Event) bool {
	timer := time.NewTimer(overflowBlockMaxWait)
	defer timer.Stop()
	select {
	case w.ch <- e:
		return true
	case <-timer.C:
		return false
	case <-w.done:
		return false
	}
}

// dropOverflow discards the event and accounts for it. The counter is not optional: a dropped
// audit record leaves no other trace, so an uncounted drop is invisible data loss.
func (w *QueueWriter) dropOverflow(e event.Event, reason string) {
	n := w.drops.Add(1)
	// Error, not Warn. The previous code logged this at Warn, which read as a capacity notice
	// rather than what it is on a compliance product: an audit record that no longer exists.
	slog.Error("audit writer: audit event DROPPED on overflow",
		"event_id", e.ID,
		"target_host", e.TargetHost,
		"method", e.Method,
		"path", e.Path,
		"action", e.Action,
		"loss_mode", w.lossMode.String(),
		"reason", reason,
		"drops_total", n,
	)
}
