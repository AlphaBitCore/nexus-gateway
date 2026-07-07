// writer_publish.go — the audit Writer's bounded-queue consumer loop and the
// publish dispatch it drives: the per-connection batch consume/publish
// (consumeLoop/publishBatchOn), the never-silent-loss failure routing
// (handlePublishFailure/spillData), and the non-batch per-record fallback
// (publishRecord). Split from writer.go, which owns the Writer struct, the
// constructor, and the With* option setters.
package audit

import (
	"context"
	"time"
)

// consumeLoop is one of the N bounded-queue consumers. It batches records from recCh
// (up to batchMaxCount, or consumerLinger for a partial batch) and publishes each
// batch on its OWN pool connection (connIdx), so the N workers' per-connection ack
// barriers pipeline concurrently instead of serialising on a single flush loop. On
// stopCh it drains the remaining queued records, then exits.
func (w *Writer) consumeLoop(connIdx int) {
	defer w.wg.Done()
	w.batchPathLogOnce.Do(func() {
		w.logger.Info("audit: bounded-queue consumer workers engaged",
			"workers", w.workers, "queueCap", cap(w.recCh), "chunk", batchMaxCount,
			"memBudgetBytes", w.memBudget.Budget())
	})
	batch := make([]*Record, 0, batchMaxCount)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	armed := false
	// publish flushes the current batch on connIdx. It is only ever invoked with a
	// NON-EMPTY batch: the recCh arm fires it at len>=batchMaxCount, and the timer.C arm
	// fires it only while armed (armed is set only after a record was appended and is
	// cleared by every publish, so a fired linger timer always has a pending record). The
	// shutdown path's empty-safe flush lives in drainOnStop.
	publish := func() {
		w.publishBatchOn(connIdx, batch)
		batch = batch[:0]
		if armed {
			timer.Stop()
			armed = false
		}
	}
	for {
		select {
		case rec := <-w.recCh:
			batch = append(batch, rec)
			// Greedily absorb whatever else is already queued so we publish ONE full
			// batch instead of one record at a time — amortising the publish ack
			// barrier over batchMaxCount records (the single biggest publish-rate
			// lever: a per-record drain-to-zero barrier caps throughput at ~1/RTT).
		drain:
			for len(batch) < batchMaxCount {
				select {
				case r := <-w.recCh:
					batch = append(batch, r)
				default:
					break drain
				}
			}
			if len(batch) >= batchMaxCount {
				publish()
			} else if !armed {
				timer.Reset(consumerLinger)
				armed = true
			}
		case <-timer.C:
			armed = false
			publish()
		case <-w.stopCh:
			w.drainOnStop(connIdx, batch)
			return
		}
	}
}

// drainOnStop empties the bounded queue into batch on shutdown and publishes the
// remainder on connIdx, bounding each publish at batchMaxCount (same cap as the
// steady-state path). Split out of consumeLoop's stopCh case so the drain is
// unit-testable deterministically: its inner select is recCh-vs-default (records-ready
// wins over default), unlike consumeLoop's main select where stopCh-vs-recCh is random,
// so exercising the drain THROUGH consumeLoop cannot be forced. Logic-equivalent to the
// previous inline loop; timer/armed cleanup is irrelevant on the exit path, and the
// per-publish non-empty guard lives here (so the steady-state publish() never needs it).
// batch is the consumer's current (possibly lingering) batch, consumed by value.
func (w *Writer) drainOnStop(connIdx int, batch []*Record) {
	for {
		select {
		case rec := <-w.recCh:
			batch = append(batch, rec)
			if len(batch) >= batchMaxCount {
				w.publishBatchOn(connIdx, batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				w.publishBatchOn(connIdx, batch)
			}
			return
		}
	}
}

// publishBatchOn marshals a batch SERIALLY (one core — the parallelism is the N
// workers) and publishes it on connection connIdx, framed or per-chunk. Per-record
// failures route through handlePublishFailure (retry/spill, never silent loss).
func (w *Writer) publishBatchOn(connIdx int, batch []*Record) {
	if w.producer == nil {
		// No-op mode (tests / unwired producer): the batch is discarded — a
		// terminal. Return the pooled bodies and byte-budget reservations, or the
		// no-op Writer would permanently leak budget and stall its own Enqueue.
		for _, rec := range batch {
			w.reclaimRecordBody(rec)
			w.releaseRecordMem(rec)
		}
		return
	}
	bp, ok := w.producer.(batchProducer)
	if !ok {
		for _, rec := range batch { // non-batch producer (tests / other transports)
			w.publishRecord(rec)
		}
		return
	}
	datas, bufs, recs := w.marshalChunkSerial(batch)
	if perfNoPublish {
		// Perf-ablation terminal: records vanish here — release their reservations
		// or the ablated pipeline back-pressures itself into a stall.
		for _, rec := range recs {
			w.releaseRecordMem(rec)
		}
		reclaimMsgBufs(bufs)
		return
	}
	if w.frameMaxBytes > 0 {
		w.publishFramed(bp, connIdx, datas, recs)
		reclaimMsgBufs(bufs)
		return
	}
	for p := 0; p < len(datas); {
		q, acc := p, 0
		for ; q < len(datas) && (q == p || acc+len(datas[q]) <= batchMaxBytes); q++ {
			acc += len(datas[q])
		}
		w.publishChunk(bp, connIdx, datas[p:q], recs[p:q])
		p = q
	}
	reclaimMsgBufs(bufs)
}

// handlePublishFailure routes a record whose publish failed so it is never lost.
// The pooled body was reclaimed at marshal, so the retry unit is the bytes
// (rec.marshaled), never a re-marshal. data aliases a pooled buffer the caller
// reclaims after this returns, so the re-queue/spill take a copy (rec.marshaled).
//
// Retry policy (BOUNDED — this is the death-spiral fix):
//   - Up to maxPublishRetries in-memory re-queues onto recCh for a fast re-publish
//     (transient NAK recovery). When recCh is full, a durable synchronous spill is
//     the immediate fallback (the sustained-wedge no-loss contract).
//   - PAST the retry cap the pipeline is wedged (a sustained stream-full outage:
//     every publish 503s, workers keep draining recCh so a naive re-queue would win
//     a slot and the record would circulate FOREVER — the busy-spin that pins the
//     marshaled copy in the queue and craters throughput). The record is then handed
//     to the durable BATCHED spill (spillCh → spillLoop → WriteBatch, off this
//     publish worker; spillLoop's marshalRecord replays rec.marshaled verbatim),
//     NOT re-queued and NOT per-record spillData (whose single mutex + O(spool-files)
//     dirSize would itself bottleneck the overflow main path). lossMode is honoured
//     on a saturated spill worker: spillBlock back-pressures this worker (which
//     propagates to intake back-pressure via recCh saturation), else a last-resort
//     synchronous spillData. Never a silent loss.
func (w *Writer) handlePublishFailure(data []byte, rec *Record) {
	if rec.marshaled == nil {
		rec.marshaled = append([]byte(nil), data...)
		rec.RequestBody = nil // body already reclaimed; never re-read it
		rec.ResponseBody = nil
	}
	rec.publishRetries++
	if rec.publishRetries <= maxPublishRetries {
		select {
		case w.recCh <- rec: // bounded in-memory retry (a worker re-publishes rec.marshaled)
			return
		default:
			// Queue full: fall through to the durable batched spill hand-off (spillBlock
			// back-pressures on a full spool; lossy modes drop), not a per-record spillData.
		}
	}
	// Retry cap exhausted (or recCh full during retry) → hand off to the durable
	// BATCHED spill worker instead of circulating on recCh or paying per-record
	// spillData (whose single mutex + O(spool-files) dirSize would itself bottleneck).
	// With no durable sink wired, the batched hand-off would only async-drop at the
	// nil-sink flush; drop synchronously + counted instead (a spillBlock config
	// without a spool is already downgraded to block at Start, so this branch is only
	// reached in the lossy spill/drop modes or a no-spool block — all lossy by config).
	if w.ndjsonSpill == nil {
		w.metrics.incDropped()
		w.releaseRecordMem(rec) // terminal: counted drop
		return
	}
	select {
	case w.spillCh <- rec:
		return
	default:
	}
	if w.lossMode == lossModeSpillBlock {
		// Spill worker saturated (writing at the disk rate, or holding a batch while it
		// back-pressures a full spool quota). Park THIS publish worker on the channel
		// until a slot frees — no per-goroutine ndjson.Write, so the spool stays
		// single-writer. A parked publish worker stops draining recCh, so intake
		// back-pressures via Enqueue. The worker never drops a full spool quota (it
		// retries), so this resolves once recovery frees space; only shutdown escapes,
		// with a one-shot durable spill (quota-full at shutdown → counted drop).
		select {
		case w.spillCh <- rec:
			return
		case <-w.stopCh:
			if !w.spillData(data) {
				w.metrics.incDropped()
			}
			w.releaseRecordMem(rec) // terminal: spilled durably or counted drop
			return
		}
	}
	if !w.spillData(data) {
		w.metrics.incDropped()
	}
	w.releaseRecordMem(rec) // terminal: spilled durably or counted drop
}

// spillData writes already-marshaled wire bytes to the durable NDJSON fallback.
// Used on the publish-retry overflow path, where the record's pooled body has been
// reclaimed and only the marshaled bytes remain. The bytes are base64-framed (via
// spillEncodeRecord) so a BINARY-wire record (which can contain raw 0x0A) survives
// the newline-delimited spool — writing it verbatim was the binary-wire dead-letter
// bug. Returns false when no fallback is wired or the write fails (loud drop).
func (w *Writer) spillData(data []byte) bool {
	if w.ndjsonSpill == nil {
		return false
	}
	if err := w.ndjsonSpill.Write(spillEncodeRecord(data)); err != nil {
		// Throttle exactly like the backpressure spill path: under a spool-quota-full
		// burst (every publish overflowing to a full disk spool), a stack-trace Error
		// per failure is itself a top CPU + allocation cost that amplifies the very
		// overload it reports — measured as a self-DoS that craters throughput. One
		// line per dropLogEvery failures; the dropped-total counter stays exact.
		if n := w.spillLogCount.Add(1); n%dropLogEvery == 1 {
			w.logger.Error("audit: spill write failed", "error", err, "spill_fail_total", n)
		}
		return false
	}
	w.metrics.incSpilled()
	return true
}

// publishRecord marshals and publishes one audit record (the non-batch fallback
// path). A transient producer failure routes through handlePublishFailure
// (re-buffer/spill, never silent loss); a hard marshal failure drops it. Safe
// for concurrent use across the flush worker pool.
func (w *Writer) publishRecord(rec *Record) {
	aliased, buf, ok := w.marshalRecord(rec)
	if !ok {
		return
	}
	// The per-record fallback uses sync Enqueue, whose contract does not
	// guarantee a synchronous copy — take a right-sized copy and reclaim the
	// pooled buffer immediately (the hot batch path holds the buffer until the
	// async ack instead, which is what eliminates the copy there).
	data := append([]byte(nil), aliased...)
	reclaimMsgBuf(buf)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.producer.Enqueue(ctx, w.queue, data); err != nil {
		w.logger.Error("audit: MQ enqueue failed", "requestId", rec.RequestID, "error", err)
		w.metrics.incEnqueueErrors()
		w.handlePublishFailure(data, rec)
		return
	}
	w.metrics.incEnqueueTotal()
	// Terminal: published OK. The pooled body was already reclaimed at marshal
	// time; the byte-budget reservation returns here.
	w.releaseRecordMem(rec)
}
