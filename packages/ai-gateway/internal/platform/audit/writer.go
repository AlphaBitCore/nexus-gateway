package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// Writer buffers audit records and publishes them to MQ in batches.
type Writer struct {
	producer mq.Producer
	logger   *slog.Logger
	queue    string
	metrics  *auditMetrics

	// Thing identity of the emitting ai-gateway instance. Stamped onto
	// every TrafficEventMessage so traffic_event.thing_id / thing_name
	// identify which gateway processed the request. Set via
	// WithThingIdentity at startup; empty in tests that don't wire it
	// (the consumer stores SQL NULL).
	thingID   string
	thingName string

	// SpillStore is the optional out-of-band body-storage backend. When
	// non-nil, recordToMessage uses spillstore.EmitBody to choose
	// between inline and spill based on the captured body size and the
	// runtime MaxInlineBodyBytes from payloadCapture. Nil keeps an
	// inline-only behaviour. Set via WithSpillStore.
	spill spillstore.SpillStore

	// ndjsonSpill is the durable on-disk fallback for whole audit records.
	// When the in-memory buffer is full after the backpressure window (or a
	// re-buffer on MQ failure cannot fit), Enqueue/publishRecord write the
	// record here instead of dropping it. Nil disables the fallback — then a
	// genuine overflow is a loud, counted drop. Set via WithNDJSONSpill.
	// Distinct from `spill` above, which stores oversized request/response
	// BODIES out-of-band; this stores entire records on transport failure.
	ndjsonSpill *sharedndjson.Writer

	// payloadCapture is the runtime payload-capture config snapshot
	// store. recordToMessage pulls MaxInlineBodyBytes from here on each
	// flush so admin-driven shadow updates take effect without a
	// service restart. Set via WithPayloadCaptureStore. Nil falls back
	// to payloadcapture.DefaultMaxInlineBodyBytes.
	payloadCapture *payloadcapture.Store

	// normalize, when non-nil, is invoked at recordToMessage time on
	// each captured (RequestBody / ResponseBody) direction to produce
	// the NormalizedPayload persisted on traffic_event_normalized.
	// Wired by ai-gateway main via shared/normcore.Registry. Nil keeps
	// the wire message without normalized fields (test / fallback).
	normalize NormalizeFn

	mu  sync.Mutex
	buf []*Record

	// flushSignal carries a size-triggered flush request from Enqueue to
	// the flush loop. Buffered (cap 1) and sent non-blocking, so a burst
	// of Enqueues coalesces into at most one pending wakeup and never
	// blocks the request path.
	flushSignal chan struct{}

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewWriter creates an audit writer that publishes to the given MQ producer.
// If producer is nil, records are enqueued but discarded on flush (no-op mode).
// If reg is nil, MQ-pipeline metrics are silently skipped (test-only path).
func NewWriter(producer mq.Producer, queue string, reg *opsmetrics.Registry, logger *slog.Logger) *Writer {
	w := &Writer{
		producer:    producer,
		queue:       queue,
		logger:      logger,
		metrics:     newAuditMetrics(reg),
		buf:         make([]*Record, 0, defaultBatchSize),
		flushSignal: make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
	}
	w.wg.Add(1)
	go w.flushLoop()
	return w
}

// WithSpillStore wires an out-of-band body backend. Bodies whose
// captured size exceeds the runtime MaxInlineBodyBytes are written via
// spillstore.Put and the audit row keeps a SpillRef; smaller bodies
// stay inline. Returns the receiver for chaining.
func (w *Writer) WithSpillStore(store spillstore.SpillStore) *Writer {
	w.spill = store
	return w
}

// WithPayloadCaptureStore wires the runtime payload-capture config
// snapshot. The audit writer reads MaxInlineBodyBytes from this store
// on every record flush, so admin-driven shadow updates take effect
// without a restart. Returns the receiver for chaining.
func (w *Writer) WithPayloadCaptureStore(s *payloadcapture.Store) *Writer {
	w.payloadCapture = s
	return w
}

// WithThingIdentity stamps the emitting ai-gateway's Thing ID and
// human-readable name onto every TrafficEventMessage. Persisted as
// traffic_event.thing_id / thing_name. Returns the receiver for chaining.
//
// Must be called during startup before any Enqueue / flushLoop runs;
// mutates w.thingID / w.thingName without a lock, matching the
// WithSpillStore / WithPayloadCaptureStore startup-only convention.
func (w *Writer) WithThingIdentity(id, name string) *Writer {
	w.thingID = id
	w.thingName = name
	return w
}

// NormalizeFn is the closure ai-gateway main wires to invoke
// shared/normalize on captured request/response bodies. Returns the
// marshalled NormalizedPayload (or nil on protocol-mismatch), the
// status ("ok" / "partial" / "failed"), and an error reason for the
// failed/partial path. The audit Writer is intentionally agnostic
// about the normalize package — it accepts bytes in and produces wire
// bytes out, so this package keeps building when shared/normalize is
// not wired (tests, no-op deployments).
//
// adapterType is the wire-format key ("openai", "anthropic", "gemini",
// "vertex", "bedrock", ...) selected by routing; it is the routing
// signal for shared/normalize's Registry. Operator-friendly provider
// names are intentionally NOT used as the routing key.
type NormalizeFn func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (raw json.RawMessage, status, errReason string)

// WithNormalizer wires a normalize closure. When set, recordToMessage
// invokes it for each captured (RequestBody / ResponseBody) direction
// and persists the result onto the TrafficEventMessage.
func (w *Writer) WithNormalizer(fn NormalizeFn) *Writer {
	w.normalize = fn
	return w
}

// closeShutdownDeadline bounds how long Close() spends draining the
// in-memory buffer through a transiently failing producer. Each
// flush() inside the loop is itself bounded by the per-record 5s
// producer.Enqueue timeout, so this is the outer wall on the whole
// drain attempt. 15s is long enough to ride out a typical NATS
// reconnect (sub-second) but short enough that an operator running
// `kubectl rollout restart` does not perceive a hang.
const closeShutdownDeadline = 15 * time.Second

// closeRetryBackoff is the cooldown between flush attempts during
// Close. Short enough that a fast NATS reconnect is observed promptly,
// long enough that we do not spin on a producer that is permanently
// unavailable.
const closeRetryBackoff = 200 * time.Millisecond

// Close flushes remaining records and stops the background goroutine.
// On a transient MQ failure flush() re-buffers the failed records, so
// Close loops flush() (with backoff) until either the buffer is empty
// or closeShutdownDeadline is reached. Records still in the buffer at
// the deadline are counted on the dropped metric and logged so a
// sustained MQ outage at shutdown surfaces in monitoring instead of
// disappearing silently.
func (w *Writer) Close() {
	close(w.stopCh)
	w.wg.Wait()
	w.drainBuffer(time.Now().Add(closeShutdownDeadline), closeRetryBackoff)
}

// drainBuffer is the deadline-bounded retry loop used by Close. Split
// out so tests can drive it with a small deadline + backoff without
// paying the production wall time. Re-buffered records on flush
// failure get retried until the buffer empties or the deadline trips;
// at the deadline any remaining records are counted on the dropped
// metric so a sustained MQ outage at shutdown is observable instead of
// silently invisible.
func (w *Writer) drainBuffer(deadline time.Time, backoff time.Duration) {
	for {
		w.flush()
		w.mu.Lock()
		remaining := len(w.buf)
		w.mu.Unlock()
		if remaining == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			w.logger.Warn("audit writer Close: buffer not fully drained at deadline; records dropped",
				"remaining", remaining,
			)
			for range remaining {
				w.metrics.incDropped()
			}
			return
		}
		time.Sleep(backoff)
	}
}

func (w *Writer) flushLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(defaultFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.flush()
		case <-w.flushSignal:
			w.flush()
		case <-w.stopCh:
			return
		}
	}
}

func (w *Writer) flush() {
	w.mu.Lock()
	if len(w.buf) == 0 {
		w.mu.Unlock()
		return
	}
	batch := w.buf
	w.buf = make([]*Record, 0, defaultBatchSize)
	w.mu.Unlock()

	if w.producer == nil {
		return
	}

	// Fan the per-record work (normalize + spill + marshal + one
	// JetStream publish-and-ack) across a bounded pool: the per-record
	// cost is the drain ceiling, and these records are independent.
	// flush() is only ever called from the single flush loop (or from
	// Close after that loop has stopped), so there is never more than one
	// flush in flight — the pool here is the only concurrency.
	sem := make(chan struct{}, publishConcurrency)
	var wg sync.WaitGroup
	for _, rec := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(rec *Record) {
			defer wg.Done()
			defer func() { <-sem }()
			w.publishRecord(rec)
		}(rec)
	}
	wg.Wait()
}

// publishRecord normalizes, marshals and publishes one audit record. On a
// transient producer failure the record is re-buffered (bounded by
// maxQueueSize) so the next flush retries it; if the buffer is already full it
// spills to the durable NDJSON sink rather than dropping. On a hard marshal
// failure it is dropped (it can never succeed). Safe for concurrent use across
// the flush worker pool — buffer mutation is under w.mu and the metrics pins +
// producer are concurrency-safe.
func (w *Writer) publishRecord(rec *Record) {
	msg := w.recordToMessage(rec)
	data, err := json.Marshal(msg)
	if err != nil {
		w.logger.Error("audit: marshal failed", "requestId", rec.RequestID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.producer.Enqueue(ctx, w.queue, data); err != nil {
		w.logger.Error("audit: MQ enqueue failed", "requestId", rec.RequestID, "error", err)
		w.metrics.incEnqueueErrors()
		w.mu.Lock()
		reBuffered := len(w.buf) < maxQueueSize
		if reBuffered {
			w.buf = append(w.buf, rec)
		}
		w.mu.Unlock()
		if !reBuffered && !w.spillRecord(rec) {
			w.metrics.incDropped()
		}
		return
	}
	w.metrics.incEnqueueTotal()
}
