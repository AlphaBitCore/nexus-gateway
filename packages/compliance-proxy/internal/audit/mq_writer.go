package audit

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// MQBatchWriter publishes compliance-proxy audit events to MQ in batches.
// The Hub db-writer consumer ingests them into the traffic_event table; the
// proxy itself never writes to the database. Events that can't make it onto
// the bus (MQ outage, channel overflow) spill to the NDJSON fallback on disk
// so no local work is lost.
type MQBatchWriter struct {
	producer      mq.Producer
	queue         string
	batchSize     int
	flushInterval time.Duration
	ndjson        *NDJSONWriter
	logger        *slog.Logger

	// thingID / thingName identify the emitting compliance-proxy instance;
	// stamped onto every TrafficEventMessage so traffic_event.thing_id and
	// .thing_name reflect which proxy processed the request. Set via
	// WithThingIdentity at startup; empty in unit tests.
	thingID   string
	thingName string

	// normalize is the closure wired by cmd/compliance-proxy/main.go from
	// shared/normalize. When non-nil, toMessage invokes it to populate the
	// TrafficEventMessage.RequestNormalized / ResponseNormalized columns so
	// cp traffic appears in the traffic_event_normalized sidecar table.
	// Nil keeps the wire fields empty.
	normalize NormalizeFn

	ch        chan AuditEvent
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	// flushReqs carries explicit Flush requests into the loop goroutine.
	// Flush() can't drain the channel itself — the loop holds events in
	// a private buf between batch fills / timer ticks, and those events
	// would be invisible to a direct drain. The loop handles a request
	// by draining the channel into its buf, flushing, and acking.
	flushReqs chan flushRequest
}

// flushRequest is a one-shot Flush coordination message handed from
// Flush() to the loop goroutine. ctx carries the caller's deadline so
// the underlying flushBatch honors it; ack receives the per-batch error
// (or nil) when the loop completes the flush.
type flushRequest struct {
	ctx context.Context
	ack chan error
}

// NormalizeFn matches shared/normalize.AuditFn but is declared here so
// this package keeps building without a shared/normalize dependency.
// Wired by cp main via WithNormalizer.
type NormalizeFn func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (raw json.RawMessage, status, errReason string)

// WithNormalizer wires the normalize closure. Returns the receiver for
// chaining. Safe to call before NewMQBatchWriter's goroutine begins
// dequeuing events.
func (w *MQBatchWriter) WithNormalizer(fn NormalizeFn) *MQBatchWriter {
	w.normalize = fn
	return w
}

// WithThingIdentity stamps the proxy's Thing ID and human-readable name onto
// every emitted TrafficEventMessage. Returns the receiver for chaining.
//
// Must be called during startup before NewMQBatchWriter's goroutine begins
// dequeuing events; mutates w.thingID / w.thingName without a lock.
func (w *MQBatchWriter) WithThingIdentity(id, name string) *MQBatchWriter {
	w.thingID = id
	w.thingName = name
	return w
}

// NewMQBatchWriter creates an MQBatchWriter that publishes to the given queue.
func NewMQBatchWriter(
	producer mq.Producer,
	queue string,
	batchSize int,
	flushInterval time.Duration,
	chanCapacity int,
	ndjsonFallback *NDJSONWriter,
	logger *slog.Logger,
) *MQBatchWriter {
	if chanCapacity <= 0 {
		chanCapacity = 1000
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	w := &MQBatchWriter{
		producer:      producer,
		queue:         queue,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		ndjson:        ndjsonFallback,
		logger:        logger,
		ch:            make(chan AuditEvent, chanCapacity),
		done:          make(chan struct{}),
		flushReqs:     make(chan flushRequest),
	}

	w.wg.Add(1)
	go w.loop()

	return w
}

// Enqueue adds an event to the write queue. Non-blocking; overflows to NDJSON.
func (w *MQBatchWriter) Enqueue(event AuditEvent) {
	if BumpStatusTotal != nil {
		BumpStatusTotal.With(event.BumpStatus).Inc()
	}

	select {
	case w.ch <- event:
		if QueueDepth != nil {
			QueueDepth.With().Set(float64(len(w.ch)))
		}
		if EnqueueTotal != nil {
			EnqueueTotal.With("mq").Inc()
		}
	default:
		if EnqueueTotal != nil {
			EnqueueTotal.With("ndjson").Inc()
		}
		if NDJSONActive != nil {
			NDJSONActive.With().Set(1)
		}
		w.logger.Warn("audit/mq_writer: channel full, writing to NDJSON fallback")
		if w.ndjson != nil {
			if err := w.ndjson.Write(event); err != nil {
				w.logger.Error("audit/mq_writer: NDJSON fallback write failed", "error", err)
			}
		}
	}
}

// Flush writes all currently buffered events immediately — both events
// still queued in w.ch and events already pulled into the loop's private
// buf. Coordinates with the loop goroutine so there's no race against
// the asynchronous batch fill / timer tick. After Close has returned the
// loop is gone, so Flush falls back to a direct channel drain.
func (w *MQBatchWriter) Flush(ctx context.Context) error {
	req := flushRequest{ctx: ctx, ack: make(chan error, 1)}
	select {
	case w.flushReqs <- req:
	case <-w.done:
		// Loop already exited (Close in flight or done) — events still in
		// the channel are nobody's responsibility now. Drain directly.
		events := w.drain()
		if len(events) == 0 {
			return nil
		}
		return w.flushBatch(ctx, events)
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// QueueLen returns the current number of events buffered.
func (w *MQBatchWriter) QueueLen() int { return len(w.ch) }

// QueueCap returns the capacity of the write channel.
func (w *MQBatchWriter) QueueCap() int { return cap(w.ch) }

// Close signals the background goroutine to stop and flushes remaining events.
func (w *MQBatchWriter) Close(ctx context.Context) error {
	var closeErr error
	w.closeOnce.Do(func() {
		close(w.done)
		w.wg.Wait()

		remaining := w.drain()
		if len(remaining) > 0 {
			if err := w.flushBatch(ctx, remaining); err != nil {
				w.logger.Error("audit/mq_writer: final flush failed", "error", err)
			}
		}
	})
	return closeErr
}

func (w *MQBatchWriter) loop() {
	defer w.wg.Done()

	timer := time.NewTimer(w.flushInterval)
	defer timer.Stop()

	buf := make([]AuditEvent, 0, w.batchSize)

	// flushBuf flushes buf via the given context, applying NDJSON fallback
	// + metrics on failure. Returns the flushBatch error (or nil) so the
	// caller can ack a Flush request with the underlying error.
	flushBuf := func(ctx context.Context) error {
		if len(buf) == 0 {
			return nil
		}
		batch := make([]AuditEvent, len(buf))
		copy(batch, buf)
		buf = buf[:0]

		err := w.flushBatch(ctx, batch)
		if err != nil {
			w.logger.Error("audit/mq_writer: batch flush failed, falling back to NDJSON",
				"count", len(batch), "error", err)
			if WriteErrors != nil {
				WriteErrors.With().Inc()
			}
			if NDJSONActive != nil {
				NDJSONActive.With().Set(1)
			}
			w.fallbackToNDJSON(batch)
		} else if NDJSONActive != nil {
			NDJSONActive.With().Set(0)
		}
		return err
	}

	// flushOwned wraps flushBuf with the loop's own 30s background context
	// for the timer / batch-full / shutdown paths that have no caller ctx.
	flushOwned := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = flushBuf(ctx)
	}

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.flushInterval)
	}

	for {
		select {
		case event, ok := <-w.ch:
			if !ok {
				flushOwned()
				return
			}
			if QueueDepth != nil {
				QueueDepth.With().Set(float64(len(w.ch)))
			}
			buf = append(buf, event)
			if len(buf) >= w.batchSize {
				flushOwned()
				resetTimer()
			}
		case <-timer.C:
			flushOwned()
			timer.Reset(w.flushInterval)
		case req := <-w.flushReqs:
			// Pull anything still queued in w.ch into buf so a single
			// flushBatch covers both already-buffered and just-arrived
			// events. Non-blocking — only events present right now.
		drain:
			for {
				select {
				case event := <-w.ch:
					buf = append(buf, event)
				default:
					break drain
				}
			}
			req.ack <- flushBuf(req.ctx)
			resetTimer()
		case <-w.done:
			for {
				select {
				case event := <-w.ch:
					buf = append(buf, event)
				default:
					flushOwned()
					return
				}
			}
		}
	}
}

func (w *MQBatchWriter) flushBatch(ctx context.Context, events []AuditEvent) error {
	start := time.Now()
	defer func() {
		if BatchLatency != nil {
			BatchLatency.With().Observe(float64(time.Since(start).Milliseconds()))
		}
		if BatchSize != nil {
			BatchSize.With().Observe(float64(len(events)))
		}
	}()

	for _, e := range events {
		msg := toMessage(e, w.thingID, w.thingName)
		applyNormalize(&msg, e, w.normalize)
		data, err := json.Marshal(msg)
		if err != nil {
			w.logger.Error("audit/mq_writer: marshal failed", "eventId", e.ID, "error", err)
			continue
		}
		if err := w.producer.Enqueue(ctx, w.queue, data); err != nil {
			return fmt.Errorf("mq enqueue failed after %d events: %w",
				len(events), err)
		}
	}
	return nil
}

func (w *MQBatchWriter) drain() []AuditEvent {
	var events []AuditEvent
	for {
		select {
		case e := <-w.ch:
			events = append(events, e)
		default:
			return events
		}
	}
}

func (w *MQBatchWriter) fallbackToNDJSON(events []AuditEvent) {
	if w.ndjson == nil {
		w.logger.Error("audit/mq_writer: no NDJSON fallback, dropping events",
			"count", len(events))
		return
	}
	for _, e := range events {
		if err := w.ndjson.Write(e); err != nil {
			w.logger.Error("audit/mq_writer: NDJSON write failed", "error", err)
		}
	}
}
