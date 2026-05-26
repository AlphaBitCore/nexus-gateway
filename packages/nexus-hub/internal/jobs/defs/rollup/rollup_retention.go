package rollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
)

const (
	rollupRetentionJobID          = "rollup-retention"
	rollupRetentionJobName        = "Rollup Retention Purge"
	rollupRetentionJobDescription = "Purges aged rows from metric_rollup_5m/1h/1d/1mo tables based on per-tier retention days."
)

// RollupRetentionConfig holds per-rollup-tier retention in days. Zero disables
// the tier so operators can preserve the finest-grained data indefinitely.
type RollupRetentionConfig struct {
	Rollup5mDays  int
	Rollup1hDays  int
	Rollup1dDays  int
	Rollup1moDays int
}

// DefaultRollupRetention matches the values CP used prior to the consolidation.
func DefaultRollupRetention() RollupRetentionConfig {
	return RollupRetentionConfig{
		Rollup5mDays:  7,
		Rollup1hDays:  90,
		Rollup1dDays:  365,
		Rollup1moDays: 1825,
	}
}

// RollupRetentionJob purges aged rows per rollup tier. Tiers are processed
// independently; a failure on one never prevents the others from attempting.
type RollupRetentionJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the per-
	// tier DELETE loop can be driven by pgxmock.
	pool     defs.PgxPool
	cfg      RollupRetentionConfig
	interval time.Duration
	logger   *slog.Logger
}

// NewRollupRetention constructs the job. interval defaults to 24h.
func NewRollupRetention(pool *pgxpool.Pool, cfg RollupRetentionConfig, interval time.Duration, logger *slog.Logger) *RollupRetentionJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &RollupRetentionJob{
		pool:     pool,
		cfg:      cfg,
		interval: interval,
		logger:   logger.With("job", rollupRetentionJobID),
	}
}

func (j *RollupRetentionJob) ID() string              { return rollupRetentionJobID }
func (j *RollupRetentionJob) Name() string            { return rollupRetentionJobName }
func (j *RollupRetentionJob) Description() string     { return rollupRetentionJobDescription }
func (j *RollupRetentionJob) Interval() time.Duration { return j.interval }

func (j *RollupRetentionJob) Run(ctx context.Context) error {
	now := time.Now().UTC()
	tiers := []struct {
		table string
		days  int
	}{
		{"metric_rollup_5m", j.cfg.Rollup5mDays},
		{"metric_rollup_1h", j.cfg.Rollup1hDays},
		{"metric_rollup_1d", j.cfg.Rollup1dDays},
		{"metric_rollup_1mo", j.cfg.Rollup1moDays},
	}

	var total int64
	var errs []error
	for _, t := range tiers {
		if t.days <= 0 {
			continue
		}
		cutoff := now.AddDate(0, 0, -t.days)
		purged, err := rollupstore.PurgeRollupBefore(ctx, j.pool, t.table, cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.table, err))
			continue
		}
		if purged > 0 {
			j.logger.Info("purged rollup tier",
				slog.String("table", t.table),
				slog.Int64("rows", purged),
				slog.String("cutoff", cutoff.Format(time.RFC3339)),
			)
		}
		total += purged
	}

	if total > 0 {
		j.logger.Info("rollup-retention completed", "totalPurged", total)
	}
	return errors.Join(errs...)
}
