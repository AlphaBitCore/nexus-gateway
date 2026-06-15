package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TrafficEventWriterConfig holds configuration for the traffic event writer.
type TrafficEventWriterConfig struct {
	BatchSize     int           `yaml:"batchSize"`
	FlushInterval time.Duration `yaml:"flushInterval"`
}

type pendingTrafficMessage struct {
	event TrafficEventMessage
	msg   *mq.Message
}

// PgxPool is the minimum pgx pool surface the writers in this package need
// — only flush() touches the pool directly (Begin tx; the rest of the
// insert path operates on the resulting pgx.Tx, which is already an
// interface in pgx and needs no seam of its own). The concrete
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface
// satisfies it in tests, letting flush()'s Begin→SendBatch→Commit chain
// be exercised without a live Postgres. Mirrors the PgxPool convention
// from packages/nexus-hub/internal/observability/siem/bridge.go,
// packages/nexus-hub/internal/alerts/engine/store.go, and
// packages/ai-gateway/internal/cache/layer/layer.go.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	// Exec runs a single statement outside any caller-held transaction.
	// Used by the DLQ insert path which must succeed even when the
	// flush tx itself has rolled back.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// TrafficEventWriter consumes traffic events from 3 MQ queues and batch-inserts
// them into the traffic_event table. Consumer group: "hub-db-writer".
type TrafficEventWriter struct {
	pool   PgxPool // interface seam — *pgxpool.Pool in prod, pgxmock in tests
	mqc    mq.Consumer
	cfg    TrafficEventWriterConfig
	logger *slog.Logger

	// consumed_total / flush_total / traffic_errors_total align with
	// mq.processed_total{stream, status} but the existing label scheme
	// (queue, result, error_type) is more diagnostic for the writer path,
	// so kept verbatim under mq.* dotted names. The error counter is
	// traffic_errors_total (not errors_total) so it doesn't collide with the
	// shared MQ transport layer's unlabeled nexus_mq_errors_total — same
	// per-consumer namespacing the exemption/admin/siem writers use.
	consumedTotal    *opsmetrics.Counter
	flushTotal       *opsmetrics.Counter
	batchSizeHist    *opsmetrics.Histogram
	errorsTotal      *opsmetrics.Counter
	dlqInsertedTotal *opsmetrics.Counter
	diskDLQTotal     *opsmetrics.Counter

	// diskDLQ is the DB-independent, on-disk dead-letter sink used only when
	// the DB-backed insertDLQ itself fails (DB unreachable). Never nil after
	// construction.
	diskDLQ *diskDLQ
}

// TrafficQueues lists the 3 MQ queues this consumer reads from.
var TrafficQueues = []string{
	"nexus.event.ai-traffic",
	"nexus.event.compliance",
	"nexus.event.agent",
}

const dbWriterGroup = "hub-db-writer"

// NewTrafficEventWriter creates a new writer. Call Start(ctx) to begin consuming.
// reg powers both /metrics and the per-tick metrics_sample push; pass nil
// only in test harnesses that do not exercise the metrics path.
func NewTrafficEventWriter(
	pool *pgxpool.Pool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	return newTrafficEventWriter(pool, mqc, cfg, logger, reg)
}

// NewTrafficEventWriterWithPgxPool is the test-only constructor accepting any
// PgxPool — production code goes through NewTrafficEventWriter. Lets the
// flush()'s Begin→SendBatch→Commit chain be driven through pgxmock without a
// live Postgres so the error branches (begin failure, insert failure with
// 22021 poison-pill ack vs nakAll, payload failure, normalized warn-and-
// continue, commit failure, ackAll success) are exercised in unit tests.
func NewTrafficEventWriterWithPgxPool(
	pool PgxPool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	return newTrafficEventWriter(pool, mqc, cfg, logger, reg)
}

func newTrafficEventWriter(
	pool PgxPool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}

	w := &TrafficEventWriter{
		pool:    pool,
		mqc:     mqc,
		cfg:     cfg,
		logger:  logger.With("component", "traffic-event-writer"),
		diskDLQ: newDiskDLQ(""),
	}
	if reg != nil {
		w.consumedTotal = reg.NewCounter("mq.processed_total", []string{"queue"})
		w.flushTotal = reg.NewCounter("mq.batch_flush_total", []string{"result"})
		w.batchSizeHist = reg.NewHistogram("mq.batch_size", nil)
		w.errorsTotal = reg.NewCounter("mq.traffic_errors_total", []string{"error_type"})
		w.dlqInsertedTotal = reg.NewCounter("mq.dlq_inserted_total", []string{"subject"})
		w.diskDLQTotal = reg.NewCounter("mq.disk_dlq_inserted_total", []string{"subject"})
	}
	return w
}

// Start begins consuming from all 3 event queues in parallel goroutines.
// Blocks until ctx is cancelled.
func (w *TrafficEventWriter) Start(ctx context.Context) error {
	for _, queue := range TrafficQueues {
		q := queue
		batch := NewBatchAccumulator[pendingTrafficMessage](w.cfg.BatchSize, w.cfg.FlushInterval, func(items []pendingTrafficMessage) error {
			return w.flush(ctx, items)
		})

		go func() {
			defer batch.Stop() //nolint:errcheck

			err := w.mqc.Consume(ctx, q, dbWriterGroup, func(_ context.Context, msg *mq.Message) error {
				return w.handleMessage(q, batch, msg)
			})

			if err != nil && ctx.Err() == nil {
				w.logger.Error("consumer exited with error", "queue", q, "error", err)
			}
		}()
	}

	<-ctx.Done()
	return nil
}

// handleMessage is the per-message handler passed to mq.Consumer.Consume.
// Returns nil if the message is a poison pill (already acked inline); returns
// mq.ErrDeferAck if the message is buffered and will be acked after the batch
// flush; returns a non-sentinel error to trigger auto-nak by the MQ driver.
func (w *TrafficEventWriter) handleMessage(queue string, batch *BatchAccumulator[pendingTrafficMessage], msg *mq.Message) error {
	if w.consumedTotal != nil {
		w.consumedTotal.With(queue).Inc()
	}

	var evt TrafficEventMessage
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		w.logger.Error("deserialize failed, dropping message",
			"queue", queue, "error", err)
		if w.errorsTotal != nil {
			w.errorsTotal.With("deserialize").Inc()
		}
		return msg.Ack()
	}

	if err := batch.Add(pendingTrafficMessage{event: evt, msg: msg}); err != nil {
		// Synchronous flush failure (batch hit maxSize and flush errored).
		// flush already invoked nakAll on this item; returning the error lets
		// the driver log it. The driver's auto-nak is idempotent on NATS/Redis.
		return err
	}
	// Hand ack/nak off to the batch flush path (ackAll / nakAll).
	return mq.ErrDeferAck
}

// flush attempts the whole batch in one tx (the fast path); if that tx fails as
// a unit it falls back to per-item reprocessing so a single bad row cannot drop
// up to 99 healthy events. flush itself returns nil because every item
// is fully resolved (acked / nak'd / dead-lettered) by one of the two paths —
// returning a non-nil error would make the MQ driver redundantly nak a message
// the per-item path already handled.
func (w *TrafficEventWriter) flush(ctx context.Context, items []pendingTrafficMessage) error {
	if w.batchSizeHist != nil {
		w.batchSizeHist.With().Observe(float64(len(items)))
	}

	if err := w.flushBatch(ctx, items); err != nil {
		// One poison/oversize row aborts the whole pgx.Batch tx, so the batch is
		// rolled back un-acked. Re-run each item in its own tx: healthy rows
		// commit + ack, the offending row is isolated (poison → ack-to-skip;
		// transient → nak/DLQ) instead of taking the batch down with it.
		w.logger.Warn("flush: batch insert failed, isolating per-item",
			"error", err, "count", len(items))
		for i := range items {
			w.flushItem(ctx, items[i])
		}
	}
	return nil
}

// flushBatch runs the batched fast path in a single transaction. On any fatal
// failure it returns the wrapped error WITHOUT acking or naking — the caller
// (flush) falls back to per-item reprocessing which owns the ack/nak decision.
// On success it acks the whole batch.
func (w *TrafficEventWriter) flushBatch(ctx context.Context, items []pendingTrafficMessage) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.countFlushErr("db_begin")
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := w.insertTrafficEvents(ctx, tx, items); err != nil {
		w.countFlushErr("db_insert")
		return fmt.Errorf("insert traffic_event: %w", err)
	}

	if err := w.insertPayloads(ctx, tx, items); err != nil {
		w.countFlushErr("db_insert_payload")
		return fmt.Errorf("insert traffic_event_payload: %w", err)
	}

	// Normalized payloads are an independent sidecar. Each sidecar row runs
	// inside its OWN savepoint (see insertNormalizedPayloads), so a failure
	// here — including a jsonb encoding error (22P05) — rolls back only that
	// savepoint and leaves the outer tx committable. The raw traffic_event +
	// traffic_event_payload rows therefore survive even when normalization
	// fails. A returned error is a non-poison sidecar failure: it is logged +
	// counted (the normalize-backfill job heals the gap) but never rolls the
	// raw batch.
	if err := w.insertNormalizedPayloads(ctx, tx, items); err != nil {
		w.logger.Warn("flush: insert traffic_event_normalized failed (raw rows still committed)",
			"error", err, "count", len(items))
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_normalized").Inc()
		}
	}

	if err := tx.Commit(ctx); err != nil {
		w.countFlushErr("db_commit")
		return fmt.Errorf("commit tx: %w", err)
	}

	w.ackAll(items)
	if w.flushTotal != nil {
		w.flushTotal.With("success").Inc()
	}

	w.logger.Debug("flushed traffic events", "count", len(items))
	return nil
}

// flushItem reprocesses a single message in its own transaction, used only when
// the batched fast path failed. It guarantees the message is resolved exactly
// once: a permanent encoding poison (typed SQLSTATE 22021 / 22P05) is acked to
// skip; any other failure is nak'd / dead-lettered; success commits + acks.
func (w *TrafficEventWriter) flushItem(ctx context.Context, pm pendingTrafficMessage) {
	single := []pendingTrafficMessage{pm}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.countFlushErr("db_begin")
		w.nakOrDLQ(ctx, single, fmt.Errorf("begin tx: %w", err))
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := w.insertTrafficEvents(ctx, tx, single); err != nil {
		w.countFlushErr("db_insert")
		w.resolveItemInsertErr(ctx, single, fmt.Errorf("insert traffic_event: %w", err))
		return
	}

	if err := w.insertPayloads(ctx, tx, single); err != nil {
		w.countFlushErr("db_insert_payload")
		w.resolveItemInsertErr(ctx, single, fmt.Errorf("insert traffic_event_payload: %w", err))
		return
	}

	if err := w.insertNormalizedPayloads(ctx, tx, single); err != nil {
		w.logger.Warn("flush: insert traffic_event_normalized failed (raw row still committed)",
			"error", err, "id", pm.event.ID)
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_normalized").Inc()
		}
	}

	if err := tx.Commit(ctx); err != nil {
		w.countFlushErr("db_commit")
		w.nakOrDLQ(ctx, single, fmt.Errorf("commit tx: %w", err))
		return
	}

	w.ackAll(single)
	if w.flushTotal != nil {
		w.flushTotal.With("success").Inc()
	}
}

// resolveItemInsertErr decides the fate of a single row whose insert failed in
// the per-item path. A permanent NUL/encoding error (typed 22021 / 22P05) can
// never succeed on retry, so the row is acked to skip (the error log is the
// audit trail); every other error is nak'd / dead-lettered for redelivery.
func (w *TrafficEventWriter) resolveItemInsertErr(ctx context.Context, single []pendingTrafficMessage, err error) {
	if isJSONNulPoison(err) {
		w.logger.Warn("flush: permanent encoding error, acking to skip poison row",
			"id", single[0].event.ID, "error", err)
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_poison").Inc()
		}
		w.ackAll(single)
		return
	}
	w.nakOrDLQ(ctx, single, err)
}

// countFlushErr increments the flush error counters for the given error_type.
func (w *TrafficEventWriter) countFlushErr(errorType string) {
	if w.flushTotal != nil {
		w.flushTotal.With("error").Inc()
	}
	if w.errorsTotal != nil {
		w.errorsTotal.With(errorType).Inc()
	}
}

func (w *TrafficEventWriter) ackAll(items []pendingTrafficMessage) {
	for _, pm := range items {
		if err := pm.msg.Ack(); err != nil {
			w.logger.Warn("ack failed", "error", err)
		}
	}
}
