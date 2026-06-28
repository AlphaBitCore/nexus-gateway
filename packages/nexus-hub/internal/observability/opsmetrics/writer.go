package opsmetrics

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// CopyPool is the minimum pgx pool surface the bounded-channel writers need
// for COPY-batch inserts. The concrete *pgxpool.Pool satisfies it in
// production; pgxmock's PgxPoolIface satisfies it in unit tests so the
// CopyFrom / Exec branches can be driven without a live PostgreSQL.
//
// Mirrors the PgxPool convention from packages/nexus-hub/internal/observability/siem and
// the cp/store + ai-gw/store + hub/store + hub/scheduler precedents.
type CopyPool interface {
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// Writer is the bounded-channel batcher that sinks metrics_sample payloads
// into metric_ops_raw via pgx CopyFrom. One Writer per Hub instance; share
// across all WS connections.
//
// Spec §7.5: target throughput is 1,000 samples per batch or 200 ms latency
// (whichever first). Channel overflow drops the new payload and increments
// the dropped counter; callers (the WS read pump) MUST NOT block.
type Writer struct {
	pool       CopyPool
	log        *slog.Logger
	maxBatch   int
	maxLatency time.Duration

	in   chan sampleEnvelope
	stop chan struct{}
	done chan struct{}

	// flushReq + flushAck are the FlushNow test seam. A request on flushReq
	// causes the background loop to drain its current buffer and reply on
	// flushAck. The pair lives outside the main `in` queue so a flush
	// request is never re-ordered with payloads racing into the channel.
	flushReq chan struct{}
	flushAck chan error

	dropped uint64

	// droppedCounter mirrors `dropped` to the spec catalog instrument
	// `metrics.dropped_total{reason}` when a registry is wired via
	// SetDropCounter. Optional; nil leaves the local atomic as the only
	// signal (visible via Dropped()) so test harnesses don't pay the
	// registration cost.
	droppedCounter *opsmetrics.Counter

	startOnce sync.Once
	stopOnce  sync.Once
}

// SetDropCounter wires the spec §6.3 `metrics.dropped_total{reason}` counter
// onto the writer so every overflow drop also increments the registered
// instrument. Safe to call once at startup before the writer sees traffic;
// not safe to swap at runtime.
func (w *Writer) SetDropCounter(c *opsmetrics.Counter) {
	if w == nil {
		return
	}
	w.droppedCounter = c
}

// sampleEnvelope is the in-memory queue item; carries the WS-authenticated
// thingID/thingType plus the decoded SampleBatch. Distinct from the wire
// envelope in shared/thingclient (which carries `type` for routing).
type sampleEnvelope struct {
	thingID   string
	thingType string
	batch     opsmetrics.SampleBatch
}

const (
	defaultSampleBatchSize    = 1000
	defaultSampleBatchLatency = 200 * time.Millisecond
)

// NewWriter constructs a Writer and starts its background goroutine. capacity
// is the in-memory queue size (channel cap + buffer cap); maxLatency is the
// idle-flush deadline. Pass 0 for capacity / maxLatency to use spec defaults
// (1000, 200ms).
func NewWriter(pool *pgxpool.Pool, logger *slog.Logger, capacity int, maxLatency time.Duration) *Writer {
	if pool == nil {
		return newWriterWithPool(nil, logger, capacity, maxLatency)
	}
	return newWriterWithPool(pool, logger, capacity, maxLatency)
}

// newWriterWithPool is the internal constructor that accepts the CopyPool
// interface so tests can inject a pgxmock pool via the test-only seam.
// Production callers go through NewWriter with a real *pgxpool.Pool which
// satisfies CopyPool.
func newWriterWithPool(pool CopyPool, logger *slog.Logger, capacity int, maxLatency time.Duration) *Writer {
	if logger == nil {
		logger = slog.Default()
	}
	if capacity <= 0 {
		capacity = defaultSampleBatchSize
	}
	if maxLatency <= 0 {
		maxLatency = defaultSampleBatchLatency
	}
	w := &Writer{
		pool:       pool,
		log:        logger.With("component", "opsmetrics_writer"),
		maxBatch:   defaultSampleBatchSize,
		maxLatency: maxLatency,
		in:         make(chan sampleEnvelope, capacity),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		flushReq:   make(chan struct{}),
		flushAck:   make(chan error, 1),
	}
	w.startOnce.Do(func() { go w.run() })
	return w
}

// Enqueue queues a SampleBatch for COPY-insert. Non-blocking: on channel
// overflow the payload is dropped and the dropped counter is incremented.
// Errors are reserved for hard misuse (writer stopped); callers should
// always treat this as best-effort.
func (w *Writer) Enqueue(_ context.Context, thingID, thingType string, batch opsmetrics.SampleBatch) error {
	if w == nil {
		return fmt.Errorf("opsmetrics writer is nil")
	}
	env := sampleEnvelope{thingID: thingID, thingType: thingType, batch: batch}
	select {
	case w.in <- env:
		return nil
	default:
		atomic.AddUint64(&w.dropped, 1)
		if w.droppedCounter != nil {
			w.droppedCounter.With("queue_overflow").Inc()
		}
		return nil
	}
}

// Dropped returns the cumulative count of overflow drops.
func (w *Writer) Dropped() uint64 {
	return atomic.LoadUint64(&w.dropped)
}

// FlushNow forces the background loop to flush whatever is currently buffered
// (both the queue channel and the in-progress slice) and waits for the COPY
// to complete or the underlying pool to error. Tests use this to assert rows
// land before the test's deadline.
func (w *Writer) FlushNow(ctx context.Context) error {
	select {
	case w.flushReq <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-w.flushAck:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop signals the goroutine to exit, drains anything still queued, and
// returns when the goroutine is gone. Idempotent.
func (w *Writer) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the background loop: pulls envelopes off w.in, accumulates rows up
// to maxBatch or maxLatency, then issues one CopyFrom per batch. Exits on
// w.stop after one final drain.
func (w *Writer) run() {
	defer close(w.done)
	timer := time.NewTimer(w.maxLatency)
	defer timer.Stop()

	buf := make([]sampleEnvelope, 0, w.maxBatch)

	// resetTimer drains the channel and re-arms; safe to call from any
	// branch including the timer-fire path (where the channel is already
	// empty and Stop returns false).
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.maxLatency)
	}

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		err := w.copyBatch(context.Background(), buf)
		buf = buf[:0]
		return err
	}

	for {
		select {
		case env := <-w.in:
			buf = append(buf, env)
			if rowCount(buf) >= w.maxBatch {
				if err := flush(); err != nil {
					w.log.Error("metric_ops_raw COPY failed",
						slog.String("error", err.Error()))
				}
				resetTimer()
			}

		case <-timer.C:
			if err := flush(); err != nil {
				w.log.Error("metric_ops_raw COPY failed (timer)",
					slog.String("error", err.Error()))
			}
			timer.Reset(w.maxLatency)

		case <-w.flushReq:
			// Drain anything currently queued so a test can synchronously
			// flush. The loop is bounded by len(w.in) at this moment.
			for {
				select {
				case env := <-w.in:
					buf = append(buf, env)
				default:
					goto drained
				}
			}
		drained:
			err := flush()
			resetTimer()
			select {
			case w.flushAck <- err:
			default:
			}

		case <-w.stop:
			// Final drain on shutdown.
			for {
				select {
				case env := <-w.in:
					buf = append(buf, env)
				default:
					_ = flush()
					return
				}
			}
		}
	}
}

// rowCount returns the total number of metric rows across all queued
// envelopes — used to decide when to flush against maxBatch.
func rowCount(envs []sampleEnvelope) int {
	n := 0
	for _, e := range envs {
		n += len(e.batch.Samples)
	}
	return n
}

// copyBatch issues a single pgx.CopyFrom into metric_ops_raw, generating a
// fresh UUID per row and marshalling Sample.Metadata to JSONB bytes.
func (w *Writer) copyBatch(ctx context.Context, envs []sampleEnvelope) error {
	if w.pool == nil {
		// Test seams may construct a Writer without a pool; treat as no-op
		// rather than panic so the dropped counter still advances and Stop
		// flows work.
		return nil
	}

	rows := make([][]any, 0, rowCount(envs))
	for _, env := range envs {
		for _, s := range env.batch.Samples {
			id := uuid.NewString()

			var metaBytes []byte
			if s.Metadata != nil {
				b, err := json.Marshal(s.Metadata)
				if err != nil {
					w.log.Warn("drop sample with unmarshallable metadata",
						slog.String("metric", s.Name),
						slog.String("error", err.Error()),
					)
					if w.droppedCounter != nil {
						w.droppedCounter.With("bad_metadata").Inc()
					}
					continue
				}
				metaBytes = b
			}

			// metric_ops_raw.value is nullable (DOUBLE PRECISION); for
			// histograms the value is conventionally zero with buckets in
			// metadata, so we still write 0.0 — matches spec §6.4.
			value := s.Value

			rows = append(rows, []any{
				id,
				env.batch.SampledAt,
				env.thingID,
				env.thingType,
				s.Name,
				string(s.Kind),
				s.DimensionKey,
				value,
				metaBytes,
			})
		}
	}
	if len(rows) == 0 {
		return nil
	}

	cols := []string{
		"id",
		"sampled_at",
		"thing_id",
		"thing_type",
		"metric_name",
		"metric_kind",
		"dimension_key",
		"value",
		"metadata",
	}
	_, err := w.pool.CopyFrom(ctx, pgx.Identifier{"metric_ops_raw"}, cols, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("copy metric_ops_raw: %w", err)
	}
	return nil
}
