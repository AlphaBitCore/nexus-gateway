package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/chain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// AdminAuditWriterConfig holds configuration for the admin audit writer.
type AdminAuditWriterConfig struct {
	BatchSize     int           `yaml:"batchSize"`
	FlushInterval time.Duration `yaml:"flushInterval"`
}

type pendingAdminMessage struct {
	event mq.AdminAuditMessage
	msg   *mq.Message
}

const adminAuditQueue = "nexus.event.admin-audit"

// AdminAuditWriter consumes admin audit events from MQ and batch-inserts them
// into the AdminAuditLog table. Consumer group: "hub-db-writer".
type AdminAuditWriter struct {
	pool   PgxPool // interface seam — *pgxpool.Pool in prod, pgxmock in tests
	mqc    mq.Consumer
	cfg    AdminAuditWriterConfig
	logger *slog.Logger

	consumedTotal *opsmetrics.Counter
	flushTotal    *opsmetrics.Counter
	batchSizeHist *opsmetrics.Histogram
	errorsTotal   *opsmetrics.Counter
}

// NewAdminAuditWriter creates a new admin audit writer. Call Start(ctx) to begin.
// reg powers both /metrics and the per-tick metrics_sample push; pass nil
// only in test harnesses.
func NewAdminAuditWriter(
	pool *pgxpool.Pool,
	mqc mq.Consumer,
	cfg AdminAuditWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *AdminAuditWriter {
	return newAdminAuditWriter(pool, mqc, cfg, logger, reg)
}

// NewAdminAuditWriterWithPgxPool is the test-only constructor accepting any
// PgxPool — production code goes through NewAdminAuditWriter. Mirrors the
// traffic writer's seam so flush()'s Begin→insertAdminEvents→Commit chain
// is exercisable under pgxmock without a live Postgres.
func NewAdminAuditWriterWithPgxPool(
	pool PgxPool,
	mqc mq.Consumer,
	cfg AdminAuditWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *AdminAuditWriter {
	return newAdminAuditWriter(pool, mqc, cfg, logger, reg)
}

func newAdminAuditWriter(
	pool PgxPool,
	mqc mq.Consumer,
	cfg AdminAuditWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *AdminAuditWriter {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}

	w := &AdminAuditWriter{
		pool:   pool,
		mqc:    mqc,
		cfg:    cfg,
		logger: logger.With("component", "admin-audit-writer"),
	}
	if reg != nil {
		w.consumedTotal = reg.NewCounter("mq.admin_consumed_total", nil)
		w.flushTotal = reg.NewCounter("mq.admin_batch_flush_total", []string{"result"})
		w.batchSizeHist = reg.NewHistogram("mq.admin_batch_size", nil)
		w.errorsTotal = reg.NewCounter("mq.admin_errors_total", []string{"error_type"})
	}
	return w
}

// Start begins consuming from nexus.event.admin-audit. Blocks until ctx is cancelled.
func (w *AdminAuditWriter) Start(ctx context.Context) error {
	batch := NewBatchAccumulator[pendingAdminMessage](w.cfg.BatchSize, w.cfg.FlushInterval, func(items []pendingAdminMessage) error {
		return w.flush(ctx, items)
	})

	go func() {
		defer batch.Stop() //nolint:errcheck

		err := w.mqc.Consume(ctx, adminAuditQueue, dbWriterGroup, func(_ context.Context, msg *mq.Message) error {
			if w.consumedTotal != nil {
				w.consumedTotal.With().Inc()
			}

			var evt mq.AdminAuditMessage
			if err := json.Unmarshal(msg.Data, &evt); err != nil {
				w.logger.Error("deserialize failed, dropping admin audit message", "error", err)
				if w.errorsTotal != nil {
					w.errorsTotal.With("deserialize").Inc()
				}
				return msg.Ack()
			}

			if err := batch.Add(pendingAdminMessage{event: evt, msg: msg}); err != nil {
				// Synchronous flush failure (batch hit maxSize and flush errored).
				// flush already invoked nakAll on this item.
				return err
			}
			// Hand ack/nak off to the batch flush path (ackAll / nakAll).
			return mq.ErrDeferAck
		})

		if err != nil && ctx.Err() == nil {
			w.logger.Error("admin audit consumer exited with error", "error", err)
		}
	}()

	<-ctx.Done()
	return nil
}

func (w *AdminAuditWriter) flush(ctx context.Context, items []pendingAdminMessage) error {
	if w.batchSizeHist != nil {
		w.batchSizeHist.With().Observe(float64(len(items)))
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.nakAll(items)
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_begin").Inc()
		}
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := w.insertAdminEvents(ctx, tx, items); err != nil {
		w.nakAll(items)
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert").Inc()
		}
		return fmt.Errorf("insert AdminAuditLog: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		w.nakAll(items)
		if w.flushTotal != nil {
			w.flushTotal.With("error").Inc()
		}
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_commit").Inc()
		}
		return fmt.Errorf("commit tx: %w", err)
	}

	w.ackAll(items)
	if w.flushTotal != nil {
		w.flushTotal.With("success").Inc()
	}

	w.logger.Debug("flushed admin audit events", "count", len(items))
	return nil
}

// insertAdminEvents writes one batch of MQ-consumed admin audit events into
// AdminAuditLog. Each row is hashed via chain.NextHash inside the same
// transaction so the chain advisory lock serialises us against the Hub
// in-tx writer (thingmgr/override.go). pgx.Batch is intentionally not used
// here: the chain is sequence-dependent (each row's hash needs the prior
// row's integrityHash committed-or-staged), so we run inserts one at a time
// inside the tx — pgx batch pipelining cannot interleave the SELECT-then-
// INSERT pattern NextHash needs.
func (w *AdminAuditWriter) insertAdminEvents(ctx context.Context, tx pgx.Tx, items []pendingAdminMessage) error {
	for _, pm := range items {
		e := pm.event

		var beforeRaw, afterRaw json.RawMessage
		if e.BeforeState != nil {
			bs, err := json.Marshal(e.BeforeState)
			if err == nil {
				beforeRaw = bs
			} else {
				// Marshal failure here means an upstream caller stuffed an
				// un-JSONable value (channel, function, recursive struct)
				// into BeforeState. We still insert the row (with NULL
				// beforeState) so the chain doesn't lose this entry, but
				// surface the visibility gap loudly so dashboards alert
				// rather than silently corrupting the audit trail.
				w.logger.Warn("admin audit beforeState marshal failed; inserting row with NULL beforeState",
					"event", "admin_audit_marshal_failed",
					"field", "beforeState",
					"thing_id", e.EntityID,
					"action", e.Action,
					"error", err,
				)
				if w.errorsTotal != nil {
					w.errorsTotal.With("marshal_before").Inc()
				}
			}
		}
		if e.AfterState != nil {
			as, err := json.Marshal(e.AfterState)
			if err == nil {
				afterRaw = as
			} else {
				w.logger.Warn("admin audit afterState marshal failed; inserting row with NULL afterState",
					"event", "admin_audit_marshal_failed",
					"field", "afterState",
					"thing_id", e.EntityID,
					"action", e.Action,
					"error", err,
				)
				if w.errorsTotal != nil {
					w.errorsTotal.With("marshal_after").Inc()
				}
			}
		}

		payload, err := chain.NewHashPayload(e.Action, e.ActorID, e.EntityType, e.EntityID)
		if err != nil {
			return fmt.Errorf("build hash payload for %s: %w", e.ID, err)
		}
		payload.TimestampMs = e.Timestamp.UTC().UnixMilli()
		payload.BeforeState = beforeRaw
		payload.AfterState = afterRaw
		payload.NexusRequestID = e.NexusRequestID
		prevHash, integrityHash, hashInput, err := chain.NextHash(ctx, tx, payload)
		if err != nil {
			return fmt.Errorf("compute chain hash: %w", err)
		}

		var prevArg any
		if prevHash != "" {
			prevArg = prevHash
		}
		var beforeArg, afterArg any
		if len(beforeRaw) > 0 {
			beforeArg = []byte(beforeRaw)
		}
		if len(afterRaw) > 0 {
			afterArg = []byte(afterRaw)
		}

		if _, err := tx.Exec(ctx, insertAdminAuditSQL,
			e.ID, e.Timestamp,
			e.ActorID, e.ActorLabel, nilIfEmpty(e.ActorRole),
			nilIfEmpty(e.SourceIP),
			e.Action, e.EntityType, nilIfEmpty(e.EntityID),
			beforeArg, afterArg,
			nilIfEmpty(e.NexusRequestID),
			prevArg, integrityHash, hashInput,
		); err != nil {
			return fmt.Errorf("insert admin audit row: %w", err)
		}
	}
	return nil
}

const insertAdminAuditSQL = `
INSERT INTO "AdminAuditLog" (
    id, timestamp,
    "actorId", "actorLabel", "actorRole",
    "sourceIp",
    action, "entityType", "entityId",
    "beforeState", "afterState",
    "nexusRequestId",
    "previousHash", "integrityHash", "hashInput"
) VALUES (
    $1, $2,
    $3, $4, $5,
    $6,
    $7, $8, $9,
    $10, $11,
    $12,
    $13, $14, $15
) ON CONFLICT (id) DO NOTHING
`

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (w *AdminAuditWriter) ackAll(items []pendingAdminMessage) {
	for _, pm := range items {
		if err := pm.msg.Ack(); err != nil {
			w.logger.Warn("ack failed", "error", err)
		}
	}
}

func (w *AdminAuditWriter) nakAll(items []pendingAdminMessage) {
	for _, pm := range items {
		if err := pm.msg.Nak(); err != nil {
			w.logger.Warn("nak failed", "error", err)
		}
	}
}
