package queue

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

// The audit writer's batching machinery: one goroutine that turns a stream of
// individually-enqueued events into batched sqlite transactions. Split out of
// writer_adapter.go, which owns the writer's public surface (construction,
// Enqueue, buildRow, Flush, Close) — the two change for unrelated reasons, and
// the batching rules below carry the load-bearing timing invariants.

// flushLoop consumes the channel, batches up to flushBatch events into
// a single sqlite transaction, and commits on either the batch-size or
// the flushInterval trigger — whichever fires first. Exits on done
// signal after draining whatever is still in the channel.
//
// The interval trigger is a timer armed only while a batch is PENDING, not a
// standing ticker (finding A-4). With flushInterval at its production 100 ms
// this loop used to wake 600 times a minute forever, and on an idle host every
// one of those wakes found an empty batch and returned immediately: 36,000 no-op
// wake-ups an hour, which on the agent is a laptop battery cost rather than a
// throughput one. Arming per batch makes the idle rate exactly zero — the
// goroutine parks in select with no pending timer — while leaving the WORST-CASE
// flush latency unchanged at flushInterval, now measured from the first event of
// the batch rather than from an arbitrary tick boundary.
func (w *QueueWriter) flushLoop() {
	defer w.wg.Done()

	// Created armed (there is no constructor for a stopped timer) and immediately
	// stopped. Go 1.23 changed Stop/Reset so a stopped or reset timer can never
	// deliver a stale value on its channel, which is what makes arm/disarm safe
	// here without draining timer.C by hand; the module targets go 1.26.
	timer := time.NewTimer(w.flushInterval)
	timer.Stop()
	defer timer.Stop()
	armed := false

	batch := make([]event.Event, 0, w.flushBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.queue.RecordBatch(batch); err != nil {
			slog.Warn("audit writer: batch insert failed — rows dropped",
				"error", err,
				"batch_size", len(batch),
			)
		}
		batch = batch[:0]
		// Nothing pending, so nothing to wake for. This is a wake-up saving, NOT a
		// correctness requirement, and the distinction is worth stating because the
		// obvious mutation test for it comes back green: the timer-fire arm below
		// already clears `armed` before calling flush, so this branch only matters for
		// the OTHER three flush paths (batch-size trip, the Flush barrier, and the
		// close-time drain), where the timer is still pending. Without it those paths
		// leave one armed timer to fire on an empty batch — a spurious wake, and a
		// slightly early flush of the next batch, but no lost or delayed rows.
		if armed {
			timer.Stop()
			armed = false
		}
	}
	// add is the single place a batch grows, so the arm-on-first-event rule cannot
	// drift between the four call sites below. Arming only when the batch was empty
	// is what bounds latency: a steady trickle of events must not keep pushing the
	// deadline out, so the timer measures from the FIRST event of each batch.
	add := func(e event.Event) {
		batch = append(batch, e)
		if !armed {
			timer.Reset(w.flushInterval)
			armed = true
		}
		if len(batch) >= w.flushBatch {
			flush()
		}
	}
	for {
		select {
		case <-w.done:
			// Drain remaining events (whatever was in the channel at
			// Close time) then return.
			for {
				select {
				case e := <-w.ch:
					add(e)
				default:
					flush()
					return
				}
			}
		case e := <-w.ch:
			add(e)
		case <-timer.C:
			armed = false
			flush()
		case resp := <-w.flushReq:
			// Synchronous barrier: drain anything that's still
			// in the channel (so the flush covers events that
			// arrived between Flush() and this case firing),
			// then flush, then signal the caller.
			for {
				select {
				case e := <-w.ch:
					add(e)
				default:
					flush()
					close(resp)
					goto nextIter
				}
			}
		nextIter:
		}
	}
}
