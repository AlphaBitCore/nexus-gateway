package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)


const (
	dataRetentionJobID          = "data-retention"
	dataRetentionJobName        = "Data Retention Purge"
	dataRetentionJobDescription = "Deletes audit and rollup rows older than the configured retention period each day."
)

// DataRetentionConfig holds per-table retention in days. Zero or negative
// values disable purge for that table so operators can keep data indefinitely.
//
// TrafficEventPayloadDays is typically shorter than TrafficEventDays because
// request/response body blobs are bulky. It purges traffic_event_payload
// rows directly; the ON DELETE CASCADE from traffic_event → traffic_event_payload
// means the longer traffic_event purge also wipes any surviving payload rows.
type DataRetentionConfig struct {
	TrafficEventDays        int
	TrafficEventPayloadDays int
	AdminAuditLogDays       int
	MetricRollupDays        int
}

// DataRetentionJob purges aged audit and rollup rows. Runs once per day.
// Scheduler advisory lock guarantees at-most-one replica executes per tick.
type DataRetentionJob struct {
	// pool is typed against the package-level defs.PgxPool seam so tests can
	// drive the four DELETE statements without sharing real tables.
	pool     defs.PgxPool
	cfg      DataRetentionConfig
	interval time.Duration
	logger   *slog.Logger
}

// NewDataRetention constructs the job. interval defaults to 24h.
func NewDataRetention(pool *pgxpool.Pool, cfg DataRetentionConfig, interval time.Duration, logger *slog.Logger) *DataRetentionJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &DataRetentionJob{
		pool:     pool,
		cfg:      cfg,
		interval: interval,
		logger:   logger.With("job", dataRetentionJobID),
	}
}

func (j *DataRetentionJob) ID() string              { return dataRetentionJobID }
func (j *DataRetentionJob) Name() string            { return dataRetentionJobName }
func (j *DataRetentionJob) Description() string     { return dataRetentionJobDescription }
func (j *DataRetentionJob) Interval() time.Duration { return j.interval }

// Run purges each configured table independently; a failure in one table does
// not prevent the others from attempting.
func (j *DataRetentionJob) Run(ctx context.Context) error {
	now := time.Now().UTC()

	purge := func(name, sql string, days int) (int64, error) {
		if days <= 0 {
			return 0, nil
		}
		cutoff := now.AddDate(0, 0, -days)
		tag, err := j.pool.Exec(ctx, sql, cutoff)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return tag.RowsAffected(), nil
	}

	// Purge payload blobs first: they're the bulky part and usually have a
	// shorter retention window than traffic_event. Any rows still present
	// when the traffic_event purge runs below get cleaned up via
	// ON DELETE CASCADE, so there is no risk of orphaned blobs.
	p, errP := purge("traffic_event_payload", `DELETE FROM traffic_event_payload WHERE created_at < $1`, j.cfg.TrafficEventPayloadDays)
	a, errA := purge("traffic_event", `DELETE FROM traffic_event WHERE timestamp < $1`, j.cfg.TrafficEventDays)
	b, errB := purge("AdminAuditLog", `DELETE FROM "AdminAuditLog" WHERE timestamp < $1`, j.cfg.AdminAuditLogDays)
	c, errC := purge("metric_rollup_1h", `DELETE FROM metric_rollup_1h WHERE "bucketStart" < $1`, j.cfg.MetricRollupDays)

	if a+b+c+p > 0 {
		j.logger.Info("retention purge",
			slog.Int64("traffic_events", a),
			slog.Int64("traffic_event_payloads", p),
			slog.Int64("admin_audit_logs", b),
			slog.Int64("metric_rollups", c),
		)
	}

	return errors.Join(errP, errA, errB, errC)
}
