package audit

import (
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
// spill). n <= 0 keeps the maxQueueSize default. The cap bounds the audit body
// pool's working set, since every queued record pins its pooled ~50 KB body until
// it is marshaled — so this is the primary control over the audit side-path's gw
// heap footprint. Wired from AuditConfig.MaxQueuedRecords at startup; call before
// any Enqueue. Returns the receiver for chaining.
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

// WithLossMode selects the overflow policy: lossModeBlock (no-loss back-pressure,
// the compliance default), lossModeSpill (async durable spill, counted drop only
// when the spill channel is also saturated), lossModeSpillBlock (spill, but
// back-pressure instead of that last-resort drop), or lossModeDrop (counted
// bounded drop). An empty or unrecognised value keeps the block default — audit
// must never silently start dropping because of a config typo. Wired from
// AuditConfig.LossMode at startup; call before any Enqueue. Returns the receiver.
func (w *Writer) WithLossMode(mode string) *Writer {
	switch mode {
	case lossModeSpill, lossModeDrop, lossModeSpillBlock:
		w.lossMode = mode
	default:
		w.lossMode = lossModeBlock
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
//   - lossModeSpillBlock: back-pressure the request goroutine until a spill slot
//     frees, bounded by backpressureMaxWait → durable spill, then a stopCh escape
//     for shutdown. This keeps spilling to disk as long as the disk can absorb it
//     and only throttles ingest (to the disk rate) under genuine overload, so it is
//     lossless up to disk-write success rather than shedding records.
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
	// lossModeSpillBlock: the spill channel is full, so the worker is saturated
	// against the disk write rate. Park the request goroutine until a slot frees
	// rather than dropping. Bound the wait so a fully STALLED (not merely slow)
	// disk falls back to a durable spill instead of parking intake forever, and
	// escape on shutdown so Close() can drain.
	timer := time.NewTimer(backpressureMaxWait)
	defer timer.Stop()
	select {
	case w.spillCh <- rec:
	case <-timer.C:
		w.spillRecord(rec)
	case <-w.stopCh:
		w.spillRecord(rec)
	}
}

// dropOverflow is the lossy "drop" policy: a counted, throttled-log bounded drop.
func (w *Writer) dropOverflow(rec *Record) {
	w.metrics.incDropped()
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

	flush := func() {
		if len(buf) == 0 {
			return
		}
		if w.ndjsonSpill == nil {
			w.metrics.addDropped(cnt)
		} else {
			start := time.Now()
			err := w.ndjsonSpill.WriteBatch(buf)
			if err != nil {
				w.metrics.addDropped(cnt)
				if n := w.spillLogCount.Add(1); n%dropLogEvery == 1 {
					w.logger.Warn("audit: durable spill batch failed, dropping records (throttled)",
						"error", err, "records", cnt)
				}
			} else {
				w.metrics.addSpilled(cnt)
				// Feed this write's size + latency back to the adaptive flush sizer so
				// the next flush threshold rides just under the disk's real write rate.
				w.spillFlush.observe(len(buf), time.Since(start))
			}
		}
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
			w.metrics.incDropped()
			return
		}
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
