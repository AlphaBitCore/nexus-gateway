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
			"workers", w.workers, "queueCap", cap(w.recCh), "chunk", batchMaxCount)
	})
	batch := make([]*Record, 0, batchMaxCount)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	armed := false
	publish := func() {
		if len(batch) == 0 {
			return
		}
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
			for {
				select {
				case rec := <-w.recCh:
					batch = append(batch, rec)
					if len(batch) >= batchMaxCount {
						publish()
					}
				default:
					publish()
					return
				}
			}
		}
	}
}

// publishBatchOn marshals a batch SERIALLY (one core — the parallelism is the N
// workers) and publishes it on connection connIdx, framed or per-chunk. Per-record
// failures route through handlePublishFailure (retry/spill, never silent loss).
func (w *Writer) publishBatchOn(connIdx int, batch []*Record) {
	if w.producer == nil {
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

// handlePublishFailure retries a record whose publish failed by re-queuing its
// already-marshaled bytes (non-blocking), or spilling them durably when the queue
// is full. The pooled body was reclaimed at marshal, so the retry unit is the bytes
// (rec.marshaled), never a re-marshal. data aliases a pooled buffer the caller
// reclaims after this returns, so re-queue takes a copy. Never a silent loss.
func (w *Writer) handlePublishFailure(data []byte, rec *Record) {
	if rec.marshaled == nil {
		rec.marshaled = append([]byte(nil), data...)
		rec.RequestBody = nil // body already reclaimed; never re-read it
		rec.ResponseBody = nil
	}
	select {
	case w.recCh <- rec: // re-queue for a retry (a worker re-publishes rec.marshaled)
	default:
		// Queue full: spill the marshaled bytes durably rather than block a worker.
		if !w.spillData(data) {
			w.metrics.incDropped()
		}
	}
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
	// Terminal: published OK. The pooled body was already reclaimed at marshal time.
}
