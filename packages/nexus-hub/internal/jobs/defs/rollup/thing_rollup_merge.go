package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// ThingRollupMergeJob is the per-Thing twin of RollupMergeJob: it merges
// thing-keyed rollup rows from a finer-grained thing rollup table into a
// coarser one. Mirrors the fleet merge pattern (watermark + idempotent
// DELETE+INSERT + single transaction) so per-Thing recovery is isolated.
type ThingRollupMergeJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-bucket merge transaction is testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
	cfg      thingRollupMergeConfig
}

const (
	watermarkThingMerge1h  = "thing-merge-1h"
	watermarkThingMerge1d  = "thing-merge-1d"
	watermarkThingMerge1mo = "thing-merge-1mo"
)

type thingRollupMergeConfig struct {
	id             string
	name           string
	description    string
	watermarkName  string
	sourceTable    string
	targetTable    string
	bucketDuration time.Duration
	initLookback   time.Duration
}

func NewThingRollupMerge1h(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *ThingRollupMergeJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	cfg := thingRollupMergeConfig{
		id:             "thing-merge-1h",
		name:           "Per-Thing Rollup Merge (5 minute → 1 hour)",
		description:    "Merges thing_metric_rollup_5m into thing_metric_rollup_1h every 5 minutes, mirroring merge-1h.",
		watermarkName:  watermarkThingMerge1h,
		sourceTable:    "thing_metric_rollup_5m",
		targetTable:    "thing_metric_rollup_1h",
		bucketDuration: time.Hour,
		initLookback:   6 * time.Hour,
	}
	return &ThingRollupMergeJob{pool: pool, interval: interval, logger: logger.With("job", cfg.id), cfg: cfg}
}

func NewThingRollupMerge1d(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *ThingRollupMergeJob {
	if interval <= 0 {
		interval = time.Hour
	}
	cfg := thingRollupMergeConfig{
		id:             "thing-merge-1d",
		name:           "Per-Thing Rollup Merge (1 hour → 1 day)",
		description:    "Merges thing_metric_rollup_1h into thing_metric_rollup_1d every hour, mirroring merge-1d.",
		watermarkName:  watermarkThingMerge1d,
		sourceTable:    "thing_metric_rollup_1h",
		targetTable:    "thing_metric_rollup_1d",
		bucketDuration: 24 * time.Hour,
		initLookback:   48 * time.Hour,
	}
	return &ThingRollupMergeJob{pool: pool, interval: interval, logger: logger.With("job", cfg.id), cfg: cfg}
}

func NewThingRollupMerge1mo(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *ThingRollupMergeJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	cfg := thingRollupMergeConfig{
		id:             "thing-merge-1mo",
		name:           "Per-Thing Rollup Merge (1 day → 1 month)",
		description:    "Merges thing_metric_rollup_1d into thing_metric_rollup_1mo daily with calendar-month bucket boundaries.",
		watermarkName:  watermarkThingMerge1mo,
		sourceTable:    "thing_metric_rollup_1d",
		targetTable:    "thing_metric_rollup_1mo",
		bucketDuration: 0,
		initLookback:   0,
	}
	return &ThingRollupMergeJob{pool: pool, interval: interval, logger: logger.With("job", cfg.id), cfg: cfg}
}

func (j *ThingRollupMergeJob) ID() string              { return j.cfg.id }
func (j *ThingRollupMergeJob) Name() string            { return j.cfg.name }
func (j *ThingRollupMergeJob) Description() string     { return j.cfg.description }
func (j *ThingRollupMergeJob) Interval() time.Duration { return j.interval }

func (j *ThingRollupMergeJob) Run(ctx context.Context) error {
	if j.cfg.bucketDuration == 0 {
		return j.runCalendarMonth(ctx)
	}
	return j.runFixed(ctx)
}

func (j *ThingRollupMergeJob) runFixed(ctx context.Context) error {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, j.cfg.watermarkName)
	if err != nil || watermark.IsZero() {
		watermark = j.coldStartWatermark(ctx)
		j.logger.Info("initializing watermark", "watermark", watermark.Format(time.RFC3339))
	}

	latestSealed := time.Now().UTC().Add(-j.cfg.bucketDuration).Truncate(j.cfg.bucketDuration)
	if !watermark.Before(latestSealed) {
		return nil
	}

	var count int
	for bucket := watermark.Add(j.cfg.bucketDuration); !bucket.After(latestSealed); bucket = bucket.Add(j.cfg.bucketDuration) {
		bucketEnd := bucket.Add(j.cfg.bucketDuration)
		if err := j.mergeOneBucket(ctx, bucket, bucketEnd); err != nil {
			return fmt.Errorf("bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
		count++
	}
	if count > 0 {
		j.logger.Info("thing merge completed", "buckets", count)
	}
	return nil
}

func (j *ThingRollupMergeJob) coldStartWatermark(ctx context.Context) time.Time {
	earliest, ok, err := rollupstore.EarliestThingBucketStart(ctx, j.pool, j.cfg.sourceTable)
	if err != nil {
		j.logger.Warn("earliest thing source bucket lookup failed, using default lookback", "error", err)
		return pickColdStartWatermark(time.Now().UTC(), j.cfg.initLookback, j.cfg.bucketDuration, time.Time{}, false)
	}
	return pickColdStartWatermark(time.Now().UTC(), j.cfg.initLookback, j.cfg.bucketDuration, earliest, ok)
}

func (j *ThingRollupMergeJob) runCalendarMonth(ctx context.Context) error {
	watermark, err := rollupstore.GetWatermark(ctx, j.pool, j.cfg.watermarkName)
	if err != nil || watermark.IsZero() {
		now := time.Now().UTC()
		watermark = time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
		j.logger.Info("initializing watermark", "watermark", watermark.Format(time.RFC3339))
	}

	now := time.Now().UTC()
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var count int
	for monthStart := nextMonth(watermark); monthStart.Before(currentMonthStart); monthStart = nextMonth(monthStart) {
		monthEnd := nextMonth(monthStart)
		if err := j.mergeOneBucket(ctx, monthStart, monthEnd); err != nil {
			return fmt.Errorf("bucket %s: %w", monthStart.Format("2006-01"), err)
		}
		count++
	}
	if count > 0 {
		j.logger.Info("thing merge completed", "buckets", count)
	}
	return nil
}

func (j *ThingRollupMergeJob) mergeOneBucket(ctx context.Context, bucketStart, bucketEnd time.Time) error {
	sourceRows, err := rollupstore.QueryThingRollupMergeSource(ctx, j.pool, j.cfg.sourceTable, bucketStart, bucketEnd)
	if err != nil {
		return fmt.Errorf("read thing source %s [%v, %v): %w", j.cfg.sourceTable, bucketStart, bucketEnd, err)
	}
	if len(sourceRows) == 0 {
		return nil
	}

	merged := metrics.MergeThingRollupRows(sourceRows)
	for i := range merged {
		merged[i].BucketStart = bucketStart
	}

	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := rollupstore.DeleteThingRollupBucket(ctx, tx, j.cfg.targetTable, bucketStart); err != nil {
		return err
	}
	if err := rollupstore.InsertThingRollupRows(ctx, tx, j.cfg.targetTable, merged); err != nil {
		return err
	}
	if err := rollupstore.SetWatermark(ctx, tx, j.cfg.watermarkName, bucketStart); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
