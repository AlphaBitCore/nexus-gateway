package audit

import (
	"errors"
	"os"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
)

// perfNoSpill is a THROWAWAY perf-ablation switch (NEXUS_PERF_NO_SPILL=1): on
// buffer overflow, drop instead of the synchronous NDJSON spill that otherwise
// runs ON THE REQUEST GOROUTINE. Isolates the pure bounded-heap effect from the
// request-path cost of the overflow spill. Never set in production.
var perfNoSpill = os.Getenv("NEXUS_PERF_NO_SPILL") == "1"

// WithMaxQueuedRecords sets the in-heap record-buffer cap (overflow → durable
// spill). n <= 0 keeps the structural default. The cap bounds the queue's
// POINTER count only — the audit side-path's heap footprint is bounded by the
// byte budget (memBudget), which accounts each record's REAL body bytes at
// Enqueue admission. Wired from AuditConfig.MaxQueuedRecords at startup; call
// before any Enqueue. Returns the receiver for chaining.
func (w *Writer) WithMaxQueuedRecords(n int) *Writer {
	if n > 0 {
		w.maxQueued = n
	}
	return w
}

// effectiveMaxQueue is the active in-heap buffer cap for this Writer.
func (w *Writer) effectiveMaxQueue() int {
	if w.maxQueued > 0 {
		return w.maxQueued
	}
	return maxQueueSize
}

// WithLossMode selects the overflow policy: lossModeSpillBlock (the no-loss
// default — spill to the durable on-disk spool first, and only back-pressure the
// request path when that large spool is ALSO saturated; makes the cheap 50GB
// spool the primary overflow buffer instead of stalling intake at the small
// in-heap queue), lossModeBlock (no-loss back-pressure at the in-heap queue,
// never touches the spool), lossModeSpill (async durable spill, counted drop only
// when the spill channel is also saturated), or lossModeDrop (counted bounded
// drop). An empty or unrecognised value keeps the spillBlock default — audit must
// never silently start dropping because of a config typo, and spillBlock is
// no-loss whenever the spool is wired (prod/rig always set AI_GATEWAY_AUDIT_SPOOL_DIR;
// spillOverflow falls back to block-style behaviour if no spool sink is wired).
// Wired from AuditConfig.LossMode at startup; call before any Enqueue. Returns the receiver.
func (w *Writer) WithLossMode(mode string) *Writer {
	switch mode {
	case lossModeSpill, lossModeDrop, lossModeBlock:
		w.lossMode = mode
	default:
		w.lossMode = lossModeSpillBlock
	}
	return w
}

// LossMode reports the resolved overflow policy (after the WithLossMode fallback),
// so startup wiring can log what is actually in effect — making a config typo that
// silently fell back to the block default observable instead of mysterious.
func (w *Writer) LossMode() string { return w.lossMode }

// perfNoAudit is a THROWAWAY perf-ablation switch (NEXUS_PERF_NO_AUDIT=1): it
// drops every audit record at the queue door so the request hot path and the
// async side-path pay ZERO audit-marshal / NATS cost. It exists ONLY to
// measure how much of the GC tail-latency comes from the audit marshal
// allocation (recordToMessage + Body.MarshalJSON + the per-record copy-out);
// it must never be set in production (every audit row would silently vanish).
// Remove once the pooled-marshal optimization lands and the ablation is done.
var perfNoAudit = os.Getenv("NEXUS_PERF_NO_AUDIT") == "1"

// This file owns the buffer-admission path: how a record enters the in-memory
// queue, what happens on overflow (bounded backpressure, then durable spill,
// then a loud last-resort drop), and the durable-spill wiring. The Writer
// lifecycle, flush loop, and publish path live in writer.go.

// WithFramePublish enables NDJSON record-batching on the publish path: marshaled
// records are packed into newline-delimited frames bounded by maxBytes and
// published one NATS message per frame, cutting the per-record PublishAsync op
// count that bottlenecks the audit drain. maxBytes <= 0 keeps the legacy
// one-message-per-record path. Bound maxBytes below the deployment's NATS
// max_payload and the Hub frame-size cap. Returns the receiver for chaining.
func (w *Writer) WithFramePublish(maxBytes int) *Writer {
	if maxBytes > 0 {
		w.frameMaxBytes = maxBytes
	}
	return w
}

// WithNDJSONSpill wires the durable on-disk fallback used when the in-memory
// buffer overflows after backpressure (or a failed publish cannot be
// re-buffered). Records spill there instead of being dropped silently.
// Returns the receiver for chaining.
func (w *Writer) WithNDJSONSpill(s *sharedndjson.Writer) *Writer {
	w.ndjsonSpill = s
	return w
}

// Enqueue is the PRODUCER side of the bounded producer/multi-consumer queue: it
// sends rec onto recCh, which the N consumer workers drain and publish. The send
// discipline is the overflow policy (lossMode):
//   - block (default, no-loss): a BLOCKING send back-pressures the request path
//     until a consumer frees a slot — admission self-throttles to the audit publish
//     rate, nothing dropped. Bounded by backpressureMaxWait so a wedged pipeline
//     (NATS down) spills durably instead of hanging forever.
//   - spill / drop (lossy opt-out): a NON-BLOCKING send; on a full queue the record
//     is handed to the async spill worker (spill) or counted-dropped (drop).
func (w *Writer) Enqueue(rec *Record) {
	if rec == nil || perfNoAudit {
		return
	}
	// Authoritative coerce for embedding rows — the single entry point every
	// producer (proxy live + cache, ai-guard sink) flows through.
	if rec.EndpointType == EndpointTypeEmbeddings {
		coerceEmbeddingRow(rec, w.logger)
	}
	w.ensureStarted()

	// Byte-bounded admission: reserve the record's REAL captured-body weight
	// against the in-memory byte budget before it may pin heap. This is the memory
	// bound — the channel caps bound pointer counts only (a slot count derived
	// from an ASSUMED body size let ~145K queued ~100 KiB records OOM the process
	// at 18 GiB). No-loss modes BLOCK here on a full budget, back-pressuring the
	// request path to the audit drain rate; lossy modes never block — a full
	// budget is an immediate counted drop, because handing an UNACCOUNTED record
	// to spillCh would re-open the unbounded-memory hole this bound closes.
	if n := int64(len(rec.RequestBody) + len(rec.ResponseBody)); n > 0 {
		switch w.lossMode {
		case lossModeBlock, lossModeSpillBlock:
			if !w.memBudget.TryAcquire(n) {
				w.metrics.incMemBackpressure()
				if !w.memBudget.Acquire(n) {
					// Shutdown fired while blocked: nothing was reserved — spill
					// durably without enqueuing (still no loss).
					w.spillRecord(rec)
					return
				}
			}
			rec.reserved = n
		default: // lossModeSpill, lossModeDrop — never block the request path
			if !w.memBudget.TryAcquire(n) {
				// Count the budget-exhausted event on the same counter the blocking
				// modes use, so an operator can tell "byte budget full" from a plain
				// queue-full drop; the drop itself is counted in dropOverflow.
				w.metrics.incMemBackpressure()
				w.dropOverflow(rec)
				return
			}
			rec.reserved = n
		}
	}

	switch w.lossMode {
	case lossModeDrop:
		select {
		case w.recCh <- rec:
		default:
			w.dropOverflow(rec)
		}
	case lossModeSpill, lossModeSpillBlock:
		select {
		case w.recCh <- rec:
		default:
			w.spillOverflow(rec)
		}
	default: // lossModeBlock — no-loss back-pressure (compliance default)
		w.blockEnqueue(rec)
	}
}

// blockEnqueue is the no-loss producer path: a BLOCKING send parks the request
// goroutine efficiently (no polling) until a consumer frees a queue slot. The
// number of simultaneously back-pressured goroutines is bounded by the server's
// request concurrency, so it cannot pile up unboundedly. A genuinely wedged
// pipeline (e.g. NATS down) past backpressureMaxWait spills the record durably
// instead of hanging forever — still no loss.
func (w *Writer) blockEnqueue(rec *Record) {
	// Fast path: a slot is free, no timer needed.
	select {
	case w.recCh <- rec:
		return
	default:
	}
	timer := time.NewTimer(backpressureMaxWait)
	defer timer.Stop()
	select {
	case w.recCh <- rec:
	case <-timer.C:
		w.spillRecord(rec) // wedged past the wait → durable spill, never a drop
	case <-w.stopCh:
		w.spillRecord(rec) // shutting down → spill durably
	}
}

// spillOverflow is the spill policy's overflow handler. The common path is a single
// NON-BLOCKING hand-off to the async spill worker (spillCh), which batches the
// record to the durable NDJSON sink OFF the request goroutine — audit is a
// delay-tolerant side-path, so the proxy runs at line rate regardless of how far
// behind the drain is. When spillCh is ALSO full, the worker is already saturated
// against the disk's sustainable write rate, and the last resort depends on the
// loss mode:
//   - lossModeSpill: a counted, throttled-log drop — never blocks the request
//     goroutine (the `dropped_total` counter makes the rare overload drop loud).
//   - lossModeSpillBlock: park the request goroutine on the bounded spillCh until
//     the SINGLE spill worker frees a slot — pure channel back-pressure, no
//     per-goroutine ndjson.Write/dirSize (the worker is the sole spool writer). The
//     worker NEVER drops a full spool quota (it holds the batch and retries while
//     recovery frees space), and a genuinely error-ing disk makes the worker drop +
//     keep draining, so this park always resolves except on a hung disk. Only
//     shutdown escapes (stopCh → one-shot durable spill). So it is lossless up to a
//     real (non-quota) disk failure, throttling ingest to the recovery-drain rate.
//
// No-loss vs never-block is a physical trilemma once ingest exceeds disk capacity:
// you cannot be lossless AND non-blocking AND memory-bounded. lossModeSpill chooses
// non-blocking (a bounded drop); lossModeSpillBlock and lossModeBlock choose
// no-loss (back-pressure on the request path).
func (w *Writer) spillOverflow(rec *Record) {
	if perfNoSpill || w.ndjsonSpill == nil {
		w.dropOverflow(rec) // no durable sink wired
		return
	}
	// Non-blocking hand-off to the async spill worker.
	select {
	case w.spillCh <- rec:
		return
	default:
	}
	if w.lossMode != lossModeSpillBlock {
		w.dropOverflow(rec) // lossModeSpill: counted drop, never blocks the request path
		return
	}
	// lossModeSpillBlock: the spill channel is full, so the single spill worker is
	// saturated (writing at the disk rate, or holding a batch while it back-pressures
	// a full spool quota). Park the request goroutine on the CHANNEL until a slot
	// frees — pure channel back-pressure, no per-goroutine ndjson.Write / dirSize, so
	// the spool stays single-writer. The worker never drops a full spool quota (it
	// retries), and a genuinely error-ing disk makes the worker drop + keep draining,
	// so this park always resolves except on a hung disk. Only shutdown escapes, with
	// a one-shot durable spill (quota-full at shutdown → counted drop, bounded Close).
	select {
	case w.spillCh <- rec:
	case <-w.stopCh:
		w.spillRecord(rec)
	}
}

// dropOverflow is the lossy "drop" policy: a counted, throttled-log bounded drop.
func (w *Writer) dropOverflow(rec *Record) {
	w.metrics.incDropped()
	w.releaseRecordMem(rec)
	w.reclaimRecordBody(rec)
	if n := w.dropLogCount.Add(1); n%dropLogEvery == 1 {
		w.logger.Warn("audit overflow, dropping records (lossy mode, throttled)",
			"requestId", rec.RequestID, "droppedSoFar", n)
	}
}

// spillLoop is the async spill worker: it drains spillCh, batching overflow
// records and marshaling + writing them to the durable NDJSON sink OFF the request
// path. Started by NewWriter; stops (after draining) when stopCh closes, so Close's
// wg.Wait() blocks until every handed-off record is durably written or counted.
func (w *Writer) spillLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(spillFlushInterval)
	defer ticker.Stop()
	var buf []byte // accumulated NDJSON lines; cap retained across flushes for reuse
	cnt := 0       // records in buf (for the spilled / dropped counters)
	// bufReserved aggregates the byte-budget reservations of the records batched
	// into buf. The *Record is discarded at add() (only its marshaled bytes live
	// on, in buf), so per-record release is impossible at the flush terminal —
	// add() transfers each reservation here and flush releases the aggregate once
	// the batch resolves (written, or dropped). While a quota-full back-pressure
	// retry holds the batch, the reservation is correctly still held: the bytes
	// really are pinned in buf. The accounting is knowingly LOW for this buffer —
	// buf holds base64-encoded marshaled lines (~1.33× the wire bytes plus
	// envelope overhead over the raw bodies the reservation counted) — but the
	// error is capped by the adaptive flush ceiling (spillFlushMaxBytes, 256 MiB),
	// noise against a memory-scale budget.
	var bufReserved int64

	flush := func() {
		if len(buf) == 0 {
			return
		}
		releaseBatch := func() {
			w.memBudget.Release(bufReserved)
			bufReserved = 0
		}
		if w.ndjsonSpill == nil {
			w.metrics.addDropped(cnt)
			releaseBatch()
			buf = buf[:0]
			cnt = 0
			return
		}
		// The spill worker is the SINGLE writer to the spool: request/consume
		// goroutines only ever park on the bounded channels, never call ndjson here,
		// so the O(files) quota scan runs from just this goroutine at the retry
		// cadence and never thrashes the mutex the recovery sweeper needs.
		retried := false
	retryLoop:
		for {
			start := time.Now()
			err := w.ndjsonSpill.WriteBatch(buf)
			if err == nil {
				w.metrics.addSpilled(cnt)
				// Only a clean write is a valid disk-write-latency sample for the
				// adaptive flush sizer. A batch that waited out a quota back-pressure is
				// contention, not device bandwidth — feeding that wait into the EMA would
				// collapse the flush threshold to the floor and re-inflate IOPS once
				// recovery frees space, so skip observe for a retried batch.
				if !retried {
					w.spillFlush.observe(len(buf), time.Since(start))
				}
				break
			}
			// spillBlock + spool AT QUOTA (soft, relieved by the recovery drain): keep
			// the SAME batch and back-pressure — wait for recovery to free space, then
			// retry. Never a drop. Every OTHER error (genuine I/O / ENOSPC) and every
			// non-spillBlock mode falls through to the counted drop below.
			if w.lossMode == lossModeSpillBlock && errors.Is(err, sharedndjson.ErrSpoolQuotaExceeded) {
				if !retried {
					retried = true
					w.metrics.incSpillBackpressure()
					if n := w.spillLogCount.Add(1); n%dropLogEvery == 1 {
						w.logger.Warn("audit: spool quota full — back-pressuring request path "+
							"(recovery draining; check NATS/Hub/PG if sustained)", "records", cnt)
					}
				}
				t := time.NewTimer(spillFlushInterval)
				select {
				case <-t.C:
					continue // retry the same batch after a pause
				case <-w.stopCh:
					t.Stop()
					w.metrics.addDropped(cnt) // shutdown one-shot → Close()/wg.Wait() stays bounded
					break retryLoop
				}
			}
			w.metrics.addDropped(cnt)
			if n := w.spillLogCount.Add(1); n%dropLogEvery == 1 {
				w.logger.Warn("audit: durable spill batch failed, dropping records (throttled)",
					"error", err, "records", cnt)
			}
			break
		}
		// Terminal for the whole batch — written durably or counted-dropped either
		// way; the batched bytes are about to be discarded, so return their
		// aggregated byte-budget reservation.
		releaseBatch()
		buf = buf[:0]
		cnt = 0
	}

	add := func(rec *Record) {
		// Marshal in the ACTIVE wire format (binary TLV when w.wireBinary, else JSON),
		// matching the publish path (spillData) and what spill-recovery re-frames for
		// the broker. Marshaling as JSON here while recovery re-framed as binary made
		// the Hub read a leading '{' as a field-id and drop the record — silent audit
		// loss on the default binary+spill config. marshalRecord reclaims the pooled
		// body; the returned bytes alias msgBuf, so reclaim it once they are copied.
		data, msgBuf, ok := w.marshalRecord(rec)
		if !ok {
			// Terminal: marshal failure dropped the record (marshalRecord released
			// its byte-budget reservation).
			w.metrics.incDropped()
			return
		}
		// The *Record is discarded past this point (only its marshaled bytes live
		// on, in buf) — transfer its reservation to the batch aggregate, released
		// at the flush terminal.
		bufReserved += rec.reserved
		rec.reserved = 0
		buf = appendSpillLine(buf, data) // base64 line copies data — binary-safe for the spool
		reclaimMsgBuf(msgBuf)            // data captured in buf; release the pooled buffer
		cnt++
		if len(buf) >= w.spillFlush.threshold() { // adaptive: sized to disk write latency
			flush()
		}
	}

	for {
		select {
		case rec := <-w.spillCh:
			add(rec)
		case <-ticker.C:
			flush()
		case <-w.stopCh:
			// Drain everything still queued so a shutdown never loses a handed-off
			// record, then flush the partial buffer and exit.
			for {
				select {
				case rec := <-w.spillCh:
					add(rec)
				default:
					flush()
					return
				}
			}
		}
	}
}

// tryAppend appends rec under the lock if there is room, waking the flush loop
// when the buffer crosses the high-water mark. Returns false (without
// blocking) when the buffer is at capacity.
// spillRecord marshals one record in the ACTIVE wire format and writes it to the
// durable NDJSON fallback, reclaiming its pooled body. Called both from the async
// spill worker and (on a wedged/shutting-down enqueue) the request goroutine. The
// wire format must match the publish path and what spill-recovery re-frames for the
// broker — marshaling as JSON while recovery re-framed as binary made the Hub drop
// the record. On a marshal/write failure (e.g. spool quota full) it counts a drop
// and THROTTLES the log: a per-failure Error carried a ~3 KB stack trace, so under a
// quota-full burst the logging itself became the dominant CPU + allocation cost — a
// self-DoS on the very overload it reported. The dropped_total metric stays exact.
func (w *Writer) spillRecord(rec *Record) bool {
	// Every exit is a terminal (written durably, or counted-dropped) — the record
	// leaves the in-memory pipeline here, so its byte-budget reservation returns.
	defer w.releaseRecordMem(rec)
	if w.ndjsonSpill == nil {
		w.metrics.incDropped()
		w.reclaimRecordBody(rec)
		return false
	}
	data, msgBuf, ok := w.marshalRecord(rec) // wire-format per w.wireBinary; reclaims the body
	if !ok {
		w.metrics.incDropped()
		return false
	}
	line := spillEncodeRecord(data) // base64 copies data into an independent spool line
	reclaimMsgBuf(msgBuf)           // data captured in line; release the pooled buffer
	if err := w.ndjsonSpill.Write(line); err != nil {
		w.metrics.incDropped()
		if n := w.spillLogCount.Add(1); n%dropLogEvery == 1 {
			w.logger.Warn("audit: durable spill failed, dropping records (throttled)",
				"error", err, "droppedSoFar", n)
		}
		return false
	}
	w.metrics.incSpilled()
	return true
}
