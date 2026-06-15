package audit

import (
	"encoding/json"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
)

// This file owns the buffer-admission path: how a record enters the in-memory
// queue, what happens on overflow (bounded backpressure, then durable spill,
// then a loud last-resort drop), and the durable-spill wiring. The Writer
// lifecycle, flush loop, and publish path live in writer.go.

// WithNDJSONSpill wires the durable on-disk fallback used when the in-memory
// buffer overflows after backpressure (or a failed publish cannot be
// re-buffered). Records spill there instead of being dropped silently.
// Returns the receiver for chaining.
func (w *Writer) WithNDJSONSpill(s *sharedndjson.Writer) *Writer {
	w.ndjsonSpill = s
	return w
}

// Enqueue adds an audit record to the write queue. The fast path is a
// non-blocking buffer append. When the buffer is full it applies BOUNDED
// BACKPRESSURE — waking the flush loop and briefly waiting (up to
// backpressureWait) for drain space — and only if it is still full spills the
// record to the durable NDJSON sink. A record is therefore never lost
// silently: it is buffered, or it back-pressures the producer, or it spills to
// disk; the loud, counted drop is the last resort when no spill is wired and
// disk-spill is impossible.
func (w *Writer) Enqueue(rec *Record) {
	if rec == nil {
		return
	}
	// Authoritative coerce for embedding rows. Every producer emits
	// through this entry point (proxy live + cache, ai-guard sink), so
	// running the coerce here is the single source of truth — a codec bug
	// that populates a chat-only field gets warned + zeroed uniformly.
	if rec.EndpointType == EndpointTypeEmbeddings {
		coerceEmbeddingRow(rec, w.logger)
	}

	if w.tryAppend(rec) {
		return
	}

	// Buffer full: apply bounded backpressure. Wake the flush loop and poll
	// for space, deliberately slowing this producer rather than dropping.
	// finalizeAudit runs after the client already has its response, so the
	// brief hold is invisible to the caller; the bound keeps goroutines from
	// piling up if the drain is genuinely down.
	deadline := time.Now().Add(backpressureWait)
	for time.Now().Before(deadline) {
		w.signalFlush()
		time.Sleep(backpressurePoll)
		if w.tryAppend(rec) {
			return
		}
	}

	// Still full after the backpressure window → spill durably to disk.
	if w.spillRecord(rec) {
		return
	}

	// No spill wired (or the spill itself failed) → last-resort LOUD drop.
	w.logger.Error("audit queue full and spill unavailable, dropping record", "requestId", rec.RequestID)
	w.metrics.incDropped()
}

// tryAppend appends rec under the lock if there is room, waking the flush loop
// when the buffer crosses the high-water mark. Returns false (without
// blocking) when the buffer is at capacity.
func (w *Writer) tryAppend(rec *Record) bool {
	w.mu.Lock()
	if len(w.buf) >= maxQueueSize {
		w.mu.Unlock()
		return false
	}
	w.buf = append(w.buf, rec)
	depth := len(w.buf)
	w.mu.Unlock()

	if depth >= flushHighWater {
		w.signalFlush()
	}
	return true
}

// signalFlush wakes the flush loop without blocking; a coalesced wakeup is
// enough, so a pending signal is left as-is.
func (w *Writer) signalFlush() {
	select {
	case w.flushSignal <- struct{}{}:
	default:
	}
}

// spillRecord writes one record to the durable NDJSON fallback. Returns false
// when no fallback is wired or the marshal/write fails (the caller then does a
// loud drop). A successful spill is counted on the spilled metric.
func (w *Writer) spillRecord(rec *Record) bool {
	if w.ndjsonSpill == nil {
		return false
	}
	data, err := json.Marshal(w.recordToMessage(rec))
	if err != nil {
		w.logger.Error("audit: spill marshal failed", "requestId", rec.RequestID, "error", err)
		return false
	}
	if err := w.ndjsonSpill.Write(data); err != nil {
		w.logger.Error("audit: spill write failed", "requestId", rec.RequestID, "error", err)
		return false
	}
	w.metrics.incSpilled()
	return true
}
