// writer_lifecycle.go — the audit Writer's start/stop lifecycle: queue sizing
// and worker launch (Start/ensureStarted), the durable spill-recovery sweeper
// wiring (WithSpillRecovery/startSpillRecovery), and the drain-everything Close.
// Split from writer.go, which owns the Writer struct, constructor, and With*
// option setters.
package audit

import (
	"context"
	"time"
)

// Start sizes the bounded queue and launches the consumer workers + spill worker.
// Wiring calls it after all With* options are applied, so recCh is sized from the
// final maxQueued. Idempotent (sync.Once); Enqueue also triggers it lazily, so a
// test that skips Start still works. Returns the receiver for chaining.
func (w *Writer) Start() *Writer {
	w.ensureStarted()
	return w
}

func (w *Writer) ensureStarted() {
	w.startOnce.Do(func() {
		// spillBlock uses the durable on-disk spool as its overflow buffer and only
		// back-pressures the request path when that spool ALSO saturates. Without a
		// spool wired — an explicit empty spoolDir, OR a spool-dir creation failure at
		// wiring time (which only logs and leaves ndjsonSpill nil) — spillOverflow has
		// no durable sink and would DROP on the first overflow. Downgrade to block, the
		// stricter no-loss mode that back-pressures at the in-heap queue and needs no
		// spool, so audit is never silently lossy from a missing/failed spool. This is
		// the documented "spillBlock without a spool falls back to block-style
		// back-pressure" — enforced here rather than assumed.
		if w.lossMode == lossModeSpillBlock && w.ndjsonSpill == nil {
			w.logger.Warn("audit: lossMode=spillBlock but no durable spool is wired; " +
				"downgrading to block (no-loss back-pressure) — wire a spool dir for spillBlock")
			w.lossMode = lossModeBlock
		}
		// Binary wire MUST always travel framed: an unframed (per-record) binary
		// message begins with field-id 1's uvarint (0x01), which is exactly the frame
		// magic — the Hub would mis-detect it as a multi-record binary frame. The
		// framed publish path prepends the magic + length-prefixes each record, which
		// is unambiguous. So when binary is selected without an explicit frame cap,
		// default one to force the framed path.
		if w.wireBinary && w.frameMaxBytes == 0 {
			w.frameMaxBytes = defaultBinaryFrameBytes
		}
		w.recCh = make(chan *Record, w.effectiveMaxQueue())
		w.wg.Add(w.workers + 1)
		for i := range w.workers {
			go w.consumeLoop(i)
		}
		go w.spillLoop()
		w.startSpillRecovery()
	})
}

// WithSpillRecovery enables the background sweeper that replays sealed durable
// spool files back into the MQ queue (the drain half of spill-defer). interval is
// the sweep period; pace throttles between files to yield the box to the gateway's
// core request path (0 = no throttle). interval <= 0 disables recovery. Recovery
// also requires WithNDJSONSpill and a batch-capable producer; without either it is
// a no-op (logged once at start). Call before Start. Returns the receiver.
func (w *Writer) WithSpillRecovery(interval, pace time.Duration) *Writer {
	w.spillRecoveryInterval = interval
	w.spillRecoveryPace = pace
	return w
}

// startSpillRecovery launches the recovery sweeper goroutine when enabled and its
// prerequisites (a spool + a batch producer) are wired. Registered in w.wg and
// driven by a context cancelled on stopCh, so Close waits for an in-flight sweep
// to finish. A missing prerequisite is logged once and recovery stays off — the
// spool is then a durable safety net drained out-of-band, never a silent loss.
func (w *Writer) startSpillRecovery() {
	if w.spillRecoveryInterval <= 0 {
		return
	}
	bp, ok := w.producer.(batchProducer)
	if !ok || w.ndjsonSpill == nil {
		w.logger.Warn("audit: spill recovery requested but disabled",
			"haveSpool", w.ndjsonSpill != nil, "batchProducer", ok)
		return
	}
	r := newSpillRecovery(w.ndjsonSpill, bp, w.queue, w.frameMaxBytes, batchMaxBytes, w.spillRecoveryPace, w.wireBinary, w.logger)
	r.onReingested = w.metrics.addReingested
	r.onError = w.metrics.incRecoveryErrors
	r.onPoisoned = w.metrics.addPoisoned
	// Source the broker max_payload so an oversize record is dead-lettered rather
	// than wedging its spool file. Leave a margin for the NATS message envelope
	// (subject + headers). Unknown (producer without the accessor) → 0 = no
	// proactive dead-letter.
	if mp, ok := w.producer.(interface{ MaxPayload() int64 }); ok {
		// Wire the accessor so runOnce can late-bind maxRecordBytes if the broker is
		// not connected yet at wiring time (MaxPayload()==0 pre-connect); also seed it
		// now for the common case where NATS is already up.
		r.maxPayload = mp.MaxPayload
		if max := mp.MaxPayload(); max > maxPayloadMargin {
			r.maxRecordBytes = int(max - maxPayloadMargin)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer cancel()
		go func() { <-w.stopCh; cancel() }()
		r.run(ctx, w.spillRecoveryInterval)
	}()
}

// Close stops accepting records and waits for the consumer + spill workers to
// drain and publish/spill everything in flight. Nothing is lost: workers drain
// recCh on stopCh, then the final sweep spills any straggler that raced in after
// their last drain check — from BOTH recCh and spillCh. spillCh must be drained
// too: after stopCh a producer's `spillCh <- rec` (the primary spillBlock
// back-pressure path) can land a record in the channel buffer AFTER the spill
// worker's final drain already returned; without this sweep that straggler is
// neither published, spilled, nor counted — a silent shutdown loss.
func (w *Writer) Close() {
	close(w.stopCh)
	w.wg.Wait()
	if w.recCh == nil {
		return // never started
	}
	drain := func(ch <-chan *Record) {
		for {
			select {
			case rec := <-ch:
				w.spillRecord(rec)
			default:
				return
			}
		}
	}
	drain(w.recCh)
	drain(w.spillCh)
}
