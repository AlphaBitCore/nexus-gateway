package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// ExemptionConsumerConfig holds configuration for the agent auto-exemption
// consumer. Batches mirror the AdminAuditWriter shape for consistency.
type ExemptionConsumerConfig struct {
	BatchSize     int           `yaml:"batchSize"`
	FlushInterval time.Duration `yaml:"flushInterval"`
}

// exemptionQueue is the MQ subject the agent uploads auto-exemptions to.
// See packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go
// (ExemptionUpload handler) for the producer side.
const exemptionQueue = "nexus.event.exemption"

// exemptionUpload mirrors the JSON payload internal_things.go enqueues.
// Keep field tags in lockstep with the producer's payload map.
type exemptionUpload struct {
	Kind      string `json:"kind"`
	ThingID   string `json:"thingId"`
	Host      string `json:"host"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expiresAt"` // RFC3339; producer marshals via time.Time.Format
}

type pendingExemptionMessage struct {
	event exemptionUpload
	msg   *mq.Message
}

// ExemptionConsumer reads agent-uploaded auto-exemption events from
// nexus.event.exemption and INSERTs them into exemption_request as
// status='PENDING' rows, closing the Cert-Pin Auto-Exemption loop:
//
//	agent runtime TLS-bump failure
//	    → exemption.Store local Add (Source=auto)
//	    → cmd_run.go uploads via POST /api/internal/things/exemption
//	    → Hub internal_things.go Enqueue("nexus.event.exemption", …)
//	    → THIS CONSUMER inserts into exemption_request
//	    → admin sees in /compliance/exemptions list (pending tab)
//	    → admin approves → ApproveExemptionRequestWithGrant creates
//	      compliance_exemption_grant row
//	    → Hub catbagent loader picks the grant up
//	    → Cat B "exemptions" pushed to agent + compliance-proxy
//	    → agent flips SourceAuto → SourceAdmin
//
// Consumer group: "hub-db-writer" (shared with TrafficEventWriter +
// AdminAuditWriter — InterestPolicy fan-out gives each group its own copy
// of every NEXUS_EVENTS subject).
//
// Dedup: a partial unique index `(target_host, requested_by) WHERE
// status='PENDING'` lives in migration
// 20260609000002_exemption_request_agent_auto_dedup_uniq. The consumer
// INSERTs with ON CONFLICT DO UPDATE so agent re-uploads (process restart,
// reported map cleared) refresh the row instead of piling duplicates.
//
// Write-failure visibility: every flush failure logs via slog.Error so the
// Hub's SlogSink → thing_diag_event pipeline surfaces it on the admin
// Recent Errors page (/infrastructure/errors).
type ExemptionConsumer struct {
	pool   PgxPool
	mqc    mq.Consumer
	cfg    ExemptionConsumerConfig
	logger *slog.Logger

	consumedTotal *opsmetrics.Counter
	flushTotal    *opsmetrics.Counter
	batchSizeHist *opsmetrics.Histogram
	errorsTotal   *opsmetrics.Counter
}

// NewExemptionConsumer wires production *pgxpool.Pool.
func NewExemptionConsumer(
	pool *pgxpool.Pool,
	mqc mq.Consumer,
	cfg ExemptionConsumerConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *ExemptionConsumer {
	return newExemptionConsumer(pool, mqc, cfg, logger, reg)
}

// NewExemptionConsumerWithPgxPool is the test-only seam matching the
// admin_audit / traffic writers; production goes through NewExemptionConsumer.
func NewExemptionConsumerWithPgxPool(
	pool PgxPool,
	mqc mq.Consumer,
	cfg ExemptionConsumerConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *ExemptionConsumer {
	return newExemptionConsumer(pool, mqc, cfg, logger, reg)
}

func newExemptionConsumer(
	pool PgxPool,
	mqc mq.Consumer,
	cfg ExemptionConsumerConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *ExemptionConsumer {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	c := &ExemptionConsumer{
		pool:   pool,
		mqc:    mqc,
		cfg:    cfg,
		logger: logger.With("component", "exemption-consumer"),
	}
	if reg != nil {
		c.consumedTotal = reg.NewCounter("mq.exemption_consumed_total", nil)
		c.flushTotal = reg.NewCounter("mq.exemption_batch_flush_total", []string{"result"})
		c.batchSizeHist = reg.NewHistogram("mq.exemption_batch_size", nil)
		c.errorsTotal = reg.NewCounter("mq.exemption_errors_total", []string{"error_type"})
	}
	return c
}

// Start begins consuming from nexus.event.exemption. Blocks until ctx
// is cancelled.
func (c *ExemptionConsumer) Start(ctx context.Context) error {
	batch := NewBatchAccumulator[pendingExemptionMessage](c.cfg.BatchSize, c.cfg.FlushInterval, func(items []pendingExemptionMessage) error {
		return c.flush(ctx, items)
	})

	go func() {
		defer batch.Stop() //nolint:errcheck

		err := c.mqc.Consume(ctx, exemptionQueue, dbWriterGroup, func(_ context.Context, msg *mq.Message) error {
			if c.consumedTotal != nil {
				c.consumedTotal.With().Inc()
			}

			var up exemptionUpload
			if err := json.Unmarshal(msg.Data, &up); err != nil {
				c.logger.Error("deserialize failed, dropping exemption upload",
					"event", "exemption_consumer_deserialize_failed",
					"error", err)
				if c.errorsTotal != nil {
					c.errorsTotal.With("deserialize").Inc()
				}
				return msg.Ack()
			}

			if err := batch.Add(pendingExemptionMessage{event: up, msg: msg}); err != nil {
				return err
			}
			return mq.ErrDeferAck
		})

		if err != nil && ctx.Err() == nil {
			c.logger.Error("exemption consumer exited with error",
				"event", "exemption_consumer_exit_error",
				"error", err)
		}
	}()

	<-ctx.Done()
	return nil
}

func (c *ExemptionConsumer) flush(ctx context.Context, items []pendingExemptionMessage) error {
	if c.batchSizeHist != nil {
		c.batchSizeHist.With().Observe(float64(len(items)))
	}

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		c.nakAll(items)
		if c.flushTotal != nil {
			c.flushTotal.With("error").Inc()
		}
		if c.errorsTotal != nil {
			c.errorsTotal.With("db_begin").Inc()
		}
		// slog.Error so SlogSink → thing_diag_event → /infrastructure/errors
		// makes the data-loss-risk visible to operators.
		c.logger.Error("exemption consumer begin tx failed",
			"event", "exemption_consumer_begin_failed",
			"batch_size", len(items),
			"error", err)
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := c.insertExemptions(ctx, tx, items); err != nil {
		c.nakAll(items)
		if c.flushTotal != nil {
			c.flushTotal.With("error").Inc()
		}
		if c.errorsTotal != nil {
			c.errorsTotal.With("db_insert").Inc()
		}
		c.logger.Error("exemption consumer insert failed",
			"event", "exemption_consumer_insert_failed",
			"batch_size", len(items),
			"error", err)
		return fmt.Errorf("insert exemption_request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		c.nakAll(items)
		if c.flushTotal != nil {
			c.flushTotal.With("error").Inc()
		}
		if c.errorsTotal != nil {
			c.errorsTotal.With("db_commit").Inc()
		}
		c.logger.Error("exemption consumer commit failed",
			"event", "exemption_consumer_commit_failed",
			"batch_size", len(items),
			"error", err)
		return fmt.Errorf("commit tx: %w", err)
	}

	c.ackAll(items)
	if c.flushTotal != nil {
		c.flushTotal.With("success").Inc()
	}
	c.logger.Debug("flushed exemption events", "count", len(items))
	return nil
}

// insertExemptions writes each agent-uploaded exemption as a PENDING row
// in exemption_request. Re-uploads (e.g. agent restart clearing its
// in-memory `reported` map) collide on the partial unique index
// `(target_host, requested_by) WHERE status='PENDING'` and refresh
// created_at + reason instead of duplicating.
func (c *ExemptionConsumer) insertExemptions(ctx context.Context, tx pgx.Tx, items []pendingExemptionMessage) error {
	for _, pm := range items {
		e := pm.event

		// derive durationMinutes from expiresAt; clamp negatives to 0 so
		// the DB CHECK / app filter sees a sensible value.
		var durationMinutes int
		if e.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, e.ExpiresAt); err == nil {
				d := time.Until(t)
				if d > 0 {
					durationMinutes = int(math.Ceil(d.Minutes()))
				}
			}
		}

		transactionID := buildExemptionTransactionID(e.ThingID, e.Host)
		requestedBy := "agent:" + e.ThingID
		reason := "auto-detected: " + e.Reason

		if _, err := tx.Exec(ctx, insertExemptionRequestSQL,
			transactionID,   // $1
			"",              // $2 source_ip (agent uploads no IP; left empty)
			e.Host,          // $3 target_host
			reason,          // $4 reason
			durationMinutes, // $5 duration_minutes
			requestedBy,     // $6 requested_by
		); err != nil {
			return fmt.Errorf("insert exemption_request row (thing=%s host=%s): %w", e.ThingID, e.Host, err)
		}
	}
	return nil
}

// insertExemptionRequestSQL inserts a PENDING auto-exemption request,
// dedup-merging into the same row on (target_host, requested_by) collision.
// The partial unique index ensures one PENDING row per (host, requester);
// approve/reject lifecycle removes the row from the index so future
// auto-detections after approval can re-INSERT independently.
const insertExemptionRequestSQL = `
INSERT INTO exemption_request (
    id, transaction_id, source_ip, target_host, reason,
    duration_minutes, requested_by, status, "createdAt"
) VALUES (
    gen_random_uuid(), $1, $2, $3, $4,
    $5, $6, 'PENDING', NOW()
) ON CONFLICT (target_host, requested_by) WHERE status = 'PENDING' DO UPDATE SET
    reason          = EXCLUDED.reason,
    duration_minutes = EXCLUDED.duration_minutes,
    "createdAt"     = NOW()
`

// buildExemptionTransactionID computes a stable transactionId for an
// agent-auto-detected exemption so the existing exemption_request UI can
// correlate / dedupe across re-uploads. The "agent-auto-" prefix
// distinguishes these from employee-submitted reject-page requests; the
// short hash (16 hex chars of sha256) collides only when (thingID, host)
// match, which is precisely the dedup invariant.
func buildExemptionTransactionID(thingID, host string) string {
	sum := sha256.Sum256([]byte(thingID + "\x00" + host))
	return "agent-auto-" + hex.EncodeToString(sum[:])[:16]
}

func (c *ExemptionConsumer) ackAll(items []pendingExemptionMessage) {
	for _, pm := range items {
		if err := pm.msg.Ack(); err != nil {
			c.logger.Warn("ack failed", "error", err)
		}
	}
}

func (c *ExemptionConsumer) nakAll(items []pendingExemptionMessage) {
	for _, pm := range items {
		if err := pm.msg.Nak(); err != nil {
			c.logger.Warn("nak failed", "error", err)
		}
	}
}
