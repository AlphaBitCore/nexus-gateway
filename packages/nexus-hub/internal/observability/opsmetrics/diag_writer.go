package opsmetrics

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// DiagWriterImpl is the bounded-channel batch writer for thing_diag_event.
// Mirrors the metrics Writer pattern (channel + background goroutine + COPY
// batch) but with smaller defaults — diag events are lower-rate than metric
// samples (spec §7.5: 100 events / 100 ms).
//
// Diag events are inserted via pgx.CopyFrom on thing_diag_event. The table
// has no UNIQUE constraint beyond the primary-key id, so duplicate-id rows
// (which happen on agent retry of an HTTP-drained crash event after a
// partial WS delivery) collide on the PK and abort the COPY. The HTTP drain
// handler (T12) handles partial-ack via per-event INSERTs to keep
// idempotency intact; the WS path here trusts the agent's client-side dedup
// (60 s LRU per spec §7.2).
type DiagWriterImpl struct {
	pool       CopyPool
	log        *slog.Logger
	maxBatch   int
	maxLatency time.Duration

	in   chan diagEnvelope
	stop chan struct{}
	done chan struct{}

	flushReq chan struct{}
	flushAck chan error

	dropped uint64

	// droppedCounter mirrors `dropped` to the spec catalog instrument
	// `diag.dropped_total{reason}` when a registry is wired via
	// SetDropCounter. Optional; nil leaves the local atomic as the only
	// signal (visible via Dropped()) so test harnesses don't pay the
	// registration cost. Pattern mirrors Writer.droppedCounter above.
	droppedCounter *opsmetrics.Counter

	startOnce sync.Once
	stopOnce  sync.Once
}

// SetDropCounter wires the `diag.dropped_total{reason}` counter onto the
// writer so every overflow drop also increments the registered instrument.
// Safe to call once at startup before the writer sees traffic; not safe to
// swap at runtime. Mirrors Writer.SetDropCounter — both writers feed the
// same operator dashboard.
//
// Background: a 16h prod audit gap was made invisible in part because diag
// drops were only visible via Dropped() (Go-side accessor, never scraped)
// instead of /metrics. Exposing the counter makes overflow drops alertable.
func (w *DiagWriterImpl) SetDropCounter(c *opsmetrics.Counter) {
	if w == nil {
		return
	}
	w.droppedCounter = c
}

type diagEnvelope struct {
	thingID   string
	thingType string
	evt       opsmetrics.DiagEvent
}

const (
	defaultDiagBatchSize    = 100
	defaultDiagBatchLatency = 100 * time.Millisecond
)

// diagEventCols is the column list passed to pgx.CopyFrom on insertBatch.
// MUST stay in sync with the `thing_diag_event` table schema — a column
// rename or new NOT NULL column on the table without an update here will
// silently break every batch insert with `42703 column ... does not exist`.
// TestDiagWriterColsMatchSchema (diag_writer_test.go) reads
// information_schema.columns at test time and asserts this slice matches
// the live table 1:1.
var diagEventCols = []string{
	"id",
	"thing_id",
	"thing_type",
	"occurred_at",
	"received_at",
	"level",
	"event_type",
	"source",
	"message",
	"message_hash",
	"trace_id",
	"attrs",
	"stack_trace",
	"repeat_count",
	"agent_version",
	"os_info",
}

// NewDiagWriter constructs a DiagWriterImpl and starts the background
// goroutine. capacity is the in-memory queue capacity; maxLatency is the
// idle-flush deadline. Pass 0 for either to use spec defaults (100 / 100ms).
// Diag events land in thing_diag_event — the admin Recent Errors page
// (/infrastructure/errors) reads from there directly.
func NewDiagWriter(pool *pgxpool.Pool, logger *slog.Logger, capacity int, maxLatency time.Duration) *DiagWriterImpl {
	if pool == nil {
		return newDiagWriterWithPool(nil, logger, capacity, maxLatency)
	}
	return newDiagWriterWithPool(pool, logger, capacity, maxLatency)
}

// newDiagWriterWithPool is the internal constructor that accepts the
// CopyPool interface so tests can inject a pgxmock pool via the test-only
// seam. Production callers go through NewDiagWriter with a real
// *pgxpool.Pool which satisfies CopyPool.
func newDiagWriterWithPool(pool CopyPool, logger *slog.Logger, capacity int, maxLatency time.Duration) *DiagWriterImpl {
	if logger == nil {
		logger = slog.Default()
	}
	if capacity <= 0 {
		capacity = defaultDiagBatchSize
	}
	if maxLatency <= 0 {
		maxLatency = defaultDiagBatchLatency
	}
	w := &DiagWriterImpl{
		pool:       pool,
		log:        logger.With("component", "opsmetrics_diag_writer"),
		maxBatch:   defaultDiagBatchSize,
		maxLatency: maxLatency,
		in:         make(chan diagEnvelope, capacity),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		flushReq:   make(chan struct{}),
		flushAck:   make(chan error, 1),
	}
	w.startOnce.Do(func() { go w.run() })
	return w
}

// Enqueue queues a DiagEvent. Non-blocking; on overflow the event is dropped
// and the dropped counter is incremented.
func (w *DiagWriterImpl) Enqueue(_ context.Context, thingID, thingType string, evt opsmetrics.DiagEvent) error {
	if w == nil {
		return fmt.Errorf("opsmetrics diag writer is nil")
	}
	env := diagEnvelope{thingID: thingID, thingType: thingType, evt: evt}
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
func (w *DiagWriterImpl) Dropped() uint64 {
	return atomic.LoadUint64(&w.dropped)
}

// FlushNow forces the goroutine to drain the current queue and waits for the
// resulting INSERT to complete.
func (w *DiagWriterImpl) FlushNow(ctx context.Context) error {
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

// Stop signals the goroutine to exit, drains any remaining queue, and
// returns once the goroutine is gone. Idempotent.
func (w *DiagWriterImpl) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *DiagWriterImpl) run() {
	defer close(w.done)
	timer := time.NewTimer(w.maxLatency)
	defer timer.Stop()

	buf := make([]diagEnvelope, 0, w.maxBatch)

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
		err := w.insertBatch(context.Background(), buf)
		buf = buf[:0]
		return err
	}

	for {
		select {
		case env := <-w.in:
			buf = append(buf, env)
			if len(buf) >= w.maxBatch {
				if err := flush(); err != nil {
					w.log.Error("thing_diag_event INSERT failed",
						slog.String("error", err.Error()))
				}
				resetTimer()
			}

		case <-timer.C:
			if err := flush(); err != nil {
				w.log.Error("thing_diag_event INSERT failed (timer)",
					slog.String("error", err.Error()))
			}
			timer.Reset(w.maxLatency)

		case <-w.flushReq:
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

// insertBatch issues a single pgx.CopyFrom into thing_diag_event. The
// message_hash column is NOT NULL: clients that emit a hash (the agent's
// slog sink does, per spec §7.2) keep it; clients that omit it (legacy or
// HTTP-drain crash payloads built before the hash convention was pinned)
// have it computed server-side via ComputeMessageHash.
func (w *DiagWriterImpl) insertBatch(ctx context.Context, envs []diagEnvelope) error {
	if w.pool == nil {
		return nil
	}

	rows := make([][]any, 0, len(envs))
	for _, env := range envs {
		evt := env.evt
		if evt.MessageHash == "" {
			evt.MessageHash = ComputeMessageHash(evt)
		}

		var attrsBytes []byte
		if evt.Attrs != nil {
			b, err := json.Marshal(evt.Attrs)
			if err != nil {
				w.log.Warn("drop diag event with unmarshallable attrs",
					slog.String("source", evt.Source),
					slog.String("error", err.Error()))
				continue
			}
			attrsBytes = b
		}

		var osInfoBytes []byte
		if evt.OSInfo != nil {
			b, err := json.Marshal(evt.OSInfo)
			if err != nil {
				w.log.Warn("drop diag event with unmarshallable osInfo",
					slog.String("source", evt.Source),
					slog.String("error", err.Error()))
				continue
			}
			osInfoBytes = b
		}

		// stack_trace, agent_version, and trace_id are nullable TEXT columns.
		// pgx maps a Go *string to a SQL NULL when nil, so we forward "" →
		// NULL via pointer indirection. Empty trace_id (events emitted off
		// any request scope — e.g. boot-time fatals) lands NULL rather than
		// "" so admin queries can filter `WHERE trace_id IS NULL` cleanly.
		var stackPtr *string
		if evt.StackTrace != "" {
			s := evt.StackTrace
			stackPtr = &s
		}
		var agentVerPtr *string
		if evt.AgentVersion != "" {
			s := evt.AgentVersion
			agentVerPtr = &s
		}
		var tracePtr *string
		if evt.TraceID != "" {
			s := evt.TraceID
			tracePtr = &s
		}

		// Defensive zero-time fallback for the WS path. Mirrors the
		// HTTP-drain path's same guard at insertDiagDrainEvent. Without
		// this, callers that emit a DiagEvent literal without setting
		// OccurredAt land Go's time.Time zero value (0001-01-01) in
		// the column — silently breaking CP UI's time-DESC sort. Use
		// ingest time as a best-effort proxy; service-side callers
		// should still populate OccurredAt at the emit site for the
		// real wall-clock truth.
		occurredAt := evt.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now().UTC()
		}
		rows = append(rows, []any{
			uuid.NewString(),
			env.thingID,
			env.thingType,
			occurredAt,
			time.Now().UTC(), // received_at — the table default is also CURRENT_TIMESTAMP, but COPY skips defaults so set explicitly.
			evt.Level,
			evt.EventType,
			evt.Source,
			evt.Message,
			evt.MessageHash,
			tracePtr,
			attrsBytes,
			stackPtr,
			int32(evt.RepeatCount),
			agentVerPtr,
			osInfoBytes,
		})
	}
	if len(rows) == 0 {
		return nil
	}

	_, err := w.pool.CopyFrom(ctx, pgx.Identifier{"thing_diag_event"}, diagEventCols, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("copy thing_diag_event: %w", err)
	}
	return nil
}

// ComputeMessageHash mirrors the agent's slog-sink dedup-key algorithm so a
// server-backfilled hash collides with a client-computed hash for the same
// logical event. Inputs:
//
//	level + "|" + source + "|" + (firstStackFrame OR message)
//
// where firstStackFrame is the line after the first '\n' in StackTrace, or
// the first 80 chars if there is no newline; falls back to Message when
// StackTrace is empty.
//
// md5 is chosen for speed — this is a non-cryptographic dedup key, not a
// security primitive.
//
// Exported (was computeMessageHash) so the handler.DiagDrainAPI in
// internal/handler/ can backfill hashes on the synchronous HTTP-drain path
// without duplicating the algorithm.
func ComputeMessageHash(evt opsmetrics.DiagEvent) string {
	var third string
	if evt.StackTrace != "" {
		if i := strings.IndexByte(evt.StackTrace, '\n'); i >= 0 {
			rest := evt.StackTrace[i+1:]
			if j := strings.IndexByte(rest, '\n'); j >= 0 {
				third = strings.TrimSpace(rest[:j])
			} else {
				third = strings.TrimSpace(rest)
			}
		} else {
			s := evt.StackTrace
			if len(s) > 80 {
				s = s[:80]
			}
			third = strings.TrimSpace(s)
		}
	}
	if third == "" {
		third = evt.Message
	}
	sum := md5.Sum([]byte(evt.Level + "|" + evt.Source + "|" + third))
	return hex.EncodeToString(sum[:])
}
