package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

const (
	metricsRollupJobID          = "metrics-rollup"
	metricsRollupJobName        = "Device Metrics Rollup"
	metricsRollupJobDescription = "Aggregates device fleet status/OS and agent action volume into metric_rollup_1h every hour."
)

// MetricsRollupJob writes the hourly device-fleet rollup row. Idempotent via
// DELETE+INSERT in a single transaction keyed on the current bucket, so a
// replica restarting mid-hour produces the same output.
type MetricsRollupJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// three SELECTs + Begin/Commit cycle are unit-testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewMetricsRollup constructs the job. interval defaults to 1h.
func NewMetricsRollup(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *MetricsRollupJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &MetricsRollupJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", metricsRollupJobID),
	}
}

func (j *MetricsRollupJob) ID() string              { return metricsRollupJobID }
func (j *MetricsRollupJob) Name() string            { return metricsRollupJobName }
func (j *MetricsRollupJob) Description() string     { return metricsRollupJobDescription }
func (j *MetricsRollupJob) Interval() time.Duration { return j.interval }

func (j *MetricsRollupJob) Run(ctx context.Context) error {
	now := time.Now().UTC()
	bucketStart := now.Truncate(time.Hour)
	lastHour := now.Add(-1 * time.Hour)

	rows, errs := j.collectRows(ctx, bucketStart, lastHour)

	if len(rows) == 0 {
		j.logger.Info("no rollup rows for bucket", "bucket", bucketStart.Format(time.RFC3339))
		return errors.Join(errs...)
	}

	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `
		DELETE FROM metric_rollup_1h
		WHERE "bucketStart" = $1
		AND "metricName" IN ($2, $3, $4)
	`, bucketStart, metrics.MetricDeviceFleetStatus, metrics.MetricDeviceFleetOS, metrics.MetricAgentActionVolume); err != nil {
		return fmt.Errorf("delete existing bucket: %w", err)
	}

	if err := rollupstore.InsertRollupRows(ctx, tx, "metric_rollup_1h", rows); err != nil {
		return fmt.Errorf("insert rollup rows: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	j.logger.Info("rollup completed",
		"bucket", bucketStart.Format(time.RFC3339),
		"rows", len(rows),
	)
	return errors.Join(errs...)
}

// collectRows runs the three source queries, tolerating per-query failures so
// a transient error on one query does not starve the others. Any errors are
// returned alongside the rows collected from the successful queries.
func (j *MetricsRollupJob) collectRows(ctx context.Context, bucketStart, since time.Time) ([]metrics.RollupRow, []error) {
	var out []metrics.RollupRow
	var errs []error

	// Fleet status: counts of agent Things grouped by status.
	fleetStatus, err := j.pool.Query(ctx, `SELECT status, COUNT(*) FROM thing WHERE type = 'agent' GROUP BY status`)
	if err != nil {
		errs = append(errs, fmt.Errorf("fleet_status query: %w", err))
	} else {
		for fleetStatus.Next() {
			var status string
			var n int
			if err := fleetStatus.Scan(&status, &n); err != nil {
				errs = append(errs, fmt.Errorf("fleet_status scan: %w", err))
				continue
			}
			out = append(out, metrics.RollupRow{
				BucketStart:  bucketStart,
				MetricName:   metrics.MetricDeviceFleetStatus,
				DimensionKey: "status=" + status,
				Value:        float64(n),
			})
		}
		fleetStatus.Close()
	}

	// Fleet OS: agent Things grouped by OS, with non-major OSes folded to "other".
	fleetOS, err := j.pool.Query(ctx, `SELECT COALESCE(t.os, '') AS os, COUNT(*) FROM thing t WHERE t.type = 'agent' GROUP BY COALESCE(t.os, '')`)
	if err != nil {
		errs = append(errs, fmt.Errorf("fleet_os query: %w", err))
	} else {
		for fleetOS.Next() {
			var osName string
			var n int
			if err := fleetOS.Scan(&osName, &n); err != nil {
				errs = append(errs, fmt.Errorf("fleet_os scan: %w", err))
				continue
			}
			if osName != "darwin" && osName != "windows" {
				osName = "other"
			}
			out = append(out, metrics.RollupRow{
				BucketStart:  bucketStart,
				MetricName:   metrics.MetricDeviceFleetOS,
				DimensionKey: "os=" + osName,
				Value:        float64(n),
			})
		}
		fleetOS.Close()
	}

	// Agent action volume over the trailing hour.
	actions, err := j.pool.Query(ctx, `
		SELECT action, COUNT(*) FROM traffic_event
		WHERE source = 'agent' AND timestamp >= $1 AND action IS NOT NULL GROUP BY action
	`, since)
	if err != nil {
		errs = append(errs, fmt.Errorf("agent_actions query: %w", err))
	} else {
		for actions.Next() {
			var action string
			var n int
			if err := actions.Scan(&action, &n); err != nil {
				errs = append(errs, fmt.Errorf("agent_actions scan: %w", err))
				continue
			}
			out = append(out, metrics.RollupRow{
				BucketStart:  bucketStart,
				MetricName:   metrics.MetricAgentActionVolume,
				DimensionKey: "action=" + action,
				Value:        float64(n),
			})
		}
		actions.Close()
	}

	return out, errs
}
