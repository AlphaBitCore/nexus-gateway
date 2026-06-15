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

const (
	watermarkMerge1h  = "merge-1h"
	watermarkMerge1d  = "merge-1d"
	watermarkMerge1mo = "merge-1mo"
)

// rollupMergeConfig describes one merge layer. A zero bucketDuration means
// calendar-month mode (variable bucket lengths); any non-zero value uses
// fixed-duration buckets (e.g. 1h, 24h).
type rollupMergeConfig struct {
	id             string
	name           string
	description    string
	watermarkName  string
	sourceTable    string
	targetTable    string
	bucketDuration time.Duration
	initLookback   time.Duration
}

// RollupMergeJob merges rows from a finer-grained rollup table into a
// coarser one. The same type drives all three merge layers (1h, 1d, 1mo);
// the calendar-month layer uses variable bucket boundaries while the others
// use fixed durations. Each bucket is written in a single transaction that
// also advances the watermark, so a replica restarting mid-bucket produces
// the same output.
type RollupMergeJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-bucket merge transaction is testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
	cfg      rollupMergeConfig
}

// NewRollupMerge1h constructs the 5m→1h merge job. interval defaults to 5m.
func NewRollupMerge1h(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *RollupMergeJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	cfg := rollupMergeConfig{
		id:             "merge-1h",
		name:           "Rollup Merge (5 minute → 1 hour)",
		description:    "Merges metric_rollup_5m into metric_rollup_1h every 5 minutes, catching up from the persisted watermark to the most recent sealed 1-hour bucket.",
		watermarkName:  watermarkMerge1h,
		sourceTable:    "metric_rollup_5m",
		targetTable:    "metric_rollup_1h",
		bucketDuration: time.Hour,
		initLookback:   6 * time.Hour,
	}
	return &RollupMergeJob{pool: pool, interval: interval, logger: logger.With("job", cfg.id), cfg: cfg}
}

// NewRollupMerge1d constructs the 1h→1d merge job. interval defaults to 1h.
func NewRollupMerge1d(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *RollupMergeJob {
	if interval <= 0 {
		interval = time.Hour
	}
	cfg := rollupMergeConfig{
		id:             "merge-1d",
		name:           "Rollup Merge (1 hour → 1 day)",
		description:    "Merges metric_rollup_1h into metric_rollup_1d every hour, catching up from the persisted watermark to the most recent sealed 1-day bucket.",
		watermarkName:  watermarkMerge1d,
		sourceTable:    "metric_rollup_1h",
		targetTable:    "metric_rollup_1d",
		bucketDuration: 24 * time.Hour,
		initLookback:   48 * time.Hour,
	}
	return &RollupMergeJob{pool: pool, interval: interval, logger: logger.With("job", cfg.id), cfg: cfg}
}

// NewRollupMerge1mo constructs the 1d→1mo merge job with calendar-month
// buckets (variable length). interval defaults to 24h.
func NewRollupMerge1mo(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *RollupMergeJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	cfg := rollupMergeConfig{
		id:             "merge-1mo",
		name:           "Rollup Merge (1 day → 1 month)",
		description:    "Merges metric_rollup_1d into metric_rollup_1mo once per day, using calendar-month bucket boundaries.",
		watermarkName:  watermarkMerge1mo,
		sourceTable:    "metric_rollup_1d",
		targetTable:    "metric_rollup_1mo",
		bucketDuration: 0, // calendar mode
		initLookback:   0,
	}
	return &RollupMergeJob{pool: pool, interval: interval, logger: logger.With("job", cfg.id), cfg: cfg}
}

func (j *RollupMergeJob) ID() string              { return j.cfg.id }
func (j *RollupMergeJob) Name() string            { return j.cfg.name }
func (j *RollupMergeJob) Description() string     { return j.cfg.description }
func (j *RollupMergeJob) Interval() time.Duration { return j.interval }

func (j *RollupMergeJob) Run(ctx context.Context) error {
	if j.cfg.bucketDuration == 0 {
		return j.runCalendarMonth(ctx)
	}
	return j.runFixed(ctx)
}

// runFixed handles fixed-duration bucket layers (1h, 1d).
func (j *RollupMergeJob) runFixed(ctx context.Context) error {
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
		j.logger.Info("merge completed", "buckets", count)
	}
	return nil
}

// coldStartWatermark picks the fixed-duration initial watermark. It returns
// the earlier of (now - initLookback) and (earliestSourceBucket - bucketDuration),
// so that when the source table already holds data older than the default
// lookback (e.g. after a restart with retained rollups), the merge covers it
// instead of skipping straight to the recent window.
func (j *RollupMergeJob) coldStartWatermark(ctx context.Context) time.Time {
	earliest, ok, err := rollupstore.EarliestBucketStart(ctx, j.pool, j.cfg.sourceTable)
	if err != nil {
		j.logger.Warn("earliest source bucket lookup failed, using default lookback", "error", err)
		return pickColdStartWatermark(time.Now().UTC(), j.cfg.initLookback, j.cfg.bucketDuration, time.Time{}, false)
	}
	return pickColdStartWatermark(time.Now().UTC(), j.cfg.initLookback, j.cfg.bucketDuration, earliest, ok)
}

// pickColdStartWatermark is the pure decision logic behind coldStartWatermark.
// It returns the earlier of (now - initLookback) truncated, and
// (earliestSourceBucket - bucketDuration) truncated, when a source bucket is
// present. Loop semantics in runFixed mean the first processed bucket is
// watermark + bucketDuration, so subtracting one bucket from the earliest
// source row makes that row the first one processed.
func pickColdStartWatermark(now time.Time, initLookback, bucketDuration time.Duration, earliestSource time.Time, haveSource bool) time.Time {
	defaultWM := now.Add(-initLookback).Truncate(bucketDuration)
	if !haveSource {
		return defaultWM
	}
	fromSource := earliestSource.UTC().Truncate(bucketDuration).Add(-bucketDuration)
	if fromSource.Before(defaultWM) {
		return fromSource
	}
	return defaultWM
}

// runCalendarMonth handles the 1d→1mo layer with variable month lengths.
// The current (unsealed) month is always excluded.
func (j *RollupMergeJob) runCalendarMonth(ctx context.Context) error {
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
		j.logger.Info("merge completed", "buckets", count)
	}
	return nil
}

// mergeOneBucket reads source rows for [bucketStart, bucketEnd), merges rows
// sharing (MetricName, DimensionKey, SubDimension), and writes the result
// + watermark in one transaction. Used by the live catch-up loop, which must
// advance the watermark.
func (j *RollupMergeJob) mergeOneBucket(ctx context.Context, bucketStart, bucketEnd time.Time) error {
	return j.mergeBucket(ctx, bucketStart, bucketEnd, true)
}

// mergeBucket merges one coarse bucket inside a single transaction. When
// writeWatermark is false (the correction backfill path) the live merge
// watermark is left untouched so re-merging historical buckets does not rewind
// the live cursor. Empty buckets are skipped (no-op).
//
// Gauge-snapshot metrics (device_fleet_status / device_fleet_os) are excluded
// from the merge so the SUM cascade never folds N hourly snapshots into one
// coarse bucket (~24x at 1d, ~720x at 1mo). These metrics are written directly
// into metric_rollup_1h by the metrics-rollup job and read only from that tier;
// they have no meaningful coarser aggregate.
func (j *RollupMergeJob) mergeBucket(ctx context.Context, bucketStart, bucketEnd time.Time, writeWatermark bool) error {
	sourceRows, err := rollupstore.QueryRollupMergeSource(ctx, j.pool, j.cfg.sourceTable, bucketStart, bucketEnd)
	if err != nil {
		return fmt.Errorf("read source %s [%v, %v): %w", j.cfg.sourceTable, bucketStart, bucketEnd, err)
	}

	sourceRows = excludeGaugeRows(sourceRows)
	if len(sourceRows) == 0 {
		return nil
	}

	merged := metrics.MergeRollupRows(sourceRows)
	merged = deduplicateRows5m(merged)

	for i := range merged {
		merged[i].BucketStart = bucketStart
	}

	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := rollupstore.DeleteRollupBucket(ctx, tx, j.cfg.targetTable, bucketStart); err != nil {
		return err
	}
	if err := rollupstore.InsertRollupRows(ctx, tx, j.cfg.targetTable, merged); err != nil {
		return err
	}
	if writeWatermark {
		if err := rollupstore.SetWatermark(ctx, tx, j.cfg.watermarkName, bucketStart); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// gaugeMergeMetrics is the set of current-state GAUGE snapshot metrics that
// must NOT propagate through the SUM merge cascade. They are point-in-time
// counts (agent Things grouped by status / OS) written directly into
// metric_rollup_1h; summing successive hourly snapshots into a coarser bucket
// over-counts by the number of snapshots folded.
var gaugeMergeMetrics = map[string]struct{}{
	metrics.MetricDeviceFleetStatus: {},
	metrics.MetricDeviceFleetOS:     {},
}

// excludeGaugeRows drops gauge-snapshot rows from a merge source slice so they
// never enter a coarser rollup tier. Returns the input unchanged when no gauge
// rows are present (the common case) to avoid an allocation.
func excludeGaugeRows(rows []metrics.RollupRow) []metrics.RollupRow {
	hasGauge := false
	for i := range rows {
		if _, ok := gaugeMergeMetrics[rows[i].MetricName]; ok {
			hasGauge = true
			break
		}
	}
	if !hasGauge {
		return rows
	}
	out := rows[:0:0]
	for i := range rows {
		if _, ok := gaugeMergeMetrics[rows[i].MetricName]; ok {
			continue
		}
		out = append(out, rows[i])
	}
	return out
}

// nextMonth returns the first day of the month following t.
func nextMonth(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}
