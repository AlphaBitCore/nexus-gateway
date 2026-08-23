package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
)

const (
	rollupCorrectionJobID          = "rollup-correction"
	rollupCorrectionJobName        = "Rollup Correction"
	rollupCorrectionJobDescription = "Recomputes rollups for the trailing correction window (default 7 days) to absorb late-arriving events. Re-runs every 5-minute bucket, then re-merges the 1h, 1d, and (for any sealed month touched) 1mo layers, then advances the rollup-correction watermark to the newest day it rebuilt."

	// WatermarkCorrection names the rollup_watermark row this job advances.
	// Exported because the vendor-bill reconcile job imports it rather than
	// repeating the string — the two must never drift apart. Unlike
	// every other watermark here it is not a resume cursor — the correction
	// window is always the trailing lookbackDays regardless of it. It is a
	// published fact: "the 1d tier has been re-aggregated from traffic_event
	// through this UTC day", which is what lets a reader tell an absent series
	// apart from one that simply has not been rebuilt yet.
	WatermarkCorrection = "rollup-correction"

	// correctionLookbackDays is the default trailing window the correction job
	// recomputes. It bounds how late an event can arrive and still be folded
	// into the rollups: an event written more than this many days after its
	// timestamp lands in a sealed bucket outside the window and would otherwise
	// be present in raw traffic_event but in no rollup. Sized to cover
	// an agent that buffered offline for several days before reconnecting.
	correctionLookbackDays = 7

	// retentionSafetyDays is how far inside the 5m retention horizon the
	// correction window is held back. See clampToRetention.
	retentionSafetyDays = 1
)

// correctionRollup is the per-bucket 5m aggregation seam the correction job
// drives. Both the fleet (*Rollup5mJob) and per-Thing (*ThingRollup5mJob)
// pipelines satisfy it. writeWatermark is always false from the correction
// path so backfilling historical buckets never rewinds the live cursor.
type correctionRollup interface {
	processBucket(ctx context.Context, bucketStart time.Time, writeWatermark bool) error
}

// correctionMerge is the per-bucket merge seam the correction job drives.
// Both *RollupMergeJob and *ThingRollupMergeJob satisfy it.
type correctionMerge interface {
	mergeBucket(ctx context.Context, bucketStart, bucketEnd time.Time, writeWatermark bool) error
}

// RollupCorrectionJob recomputes all fleet rollup layers across the trailing
// correction window. Events may land in traffic_event after their bucket has
// already been rolled up; rewinding the window once per tick catches those
// late writes. It delegates to the existing 5m / merge jobs so the aggregation
// logic stays in one place, and never advances the live watermark so
// the daily correction does not force the next live tick to re-scan.
type RollupCorrectionJob struct {
	// pool exists only to publish the watermark; the aggregation itself runs
	// through the sibling jobs, which hold their own pool.
	pool         defs.PgxPool
	r5m          correctionRollup
	merge1h      correctionMerge
	merge1d      correctionMerge
	merge1mo     correctionMerge
	lookbackDays int
	// rollup5mRetentionDays mirrors RollupRetentionConfig.Rollup5mDays so the
	// correction window can be held inside it. 0 means the 5m tier is never
	// purged, so no clamp applies. See clampToRetention.
	rollup5mRetentionDays int
	interval              time.Duration
	logger                *slog.Logger
	// nowFn returns the current time; defaults to time.Now. Seam so tests can pin a
	// date deterministically — the monthly re-merge fires only for a month the
	// lookback window reaches into and that is already sealed.
	nowFn func() time.Time
}

// NewRollupCorrection constructs the fleet correction job. interval defaults to
// 24h; lookbackDays defaults to correctionLookbackDays when zero or negative.
// rollup5mRetentionDays must be the same value the retention job purges the 5m
// tier at (0 = never purged); it clamps the window, see clampToRetention.
// The four sibling jobs must have been constructed with the same pool so they
// share a transaction surface and configuration.
func NewRollupCorrection(
	pool defs.PgxPool,
	r5m *Rollup5mJob,
	merge1h, merge1d, merge1mo *RollupMergeJob,
	lookbackDays int,
	rollup5mRetentionDays int,
	interval time.Duration,
	logger *slog.Logger,
) *RollupCorrectionJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if lookbackDays <= 0 {
		lookbackDays = correctionLookbackDays
	}
	return &RollupCorrectionJob{
		pool:                  pool,
		r5m:                   r5m,
		merge1h:               merge1h,
		merge1d:               merge1d,
		merge1mo:              merge1mo,
		lookbackDays:          lookbackDays,
		rollup5mRetentionDays: rollup5mRetentionDays,
		interval:              interval,
		logger:                logger.With("job", rollupCorrectionJobID),
		nowFn:                 time.Now,
	}
}

// clampToRetention caps the correction lookback so the window never reaches
// into the region the retention job is entitled to purge.
//
// The two jobs share a scheduler tick and, at their shipped defaults, an edge:
// retention purges metric_rollup_5m rows older than now-Rollup5mDays (a
// wall-clock instant, e.g. 2026-07-31T02:56Z), while a 7-day correction window
// starts at the UTC midnight 7 days back (2026-07-31T00:00Z). The buckets
// between those two points belong to both jobs at once, and both write them:
// retention with one wide range DELETE, correction with a per-bucket DELETE
// followed by per-row INSERTs. Scanning the same rows in different orders is a
// textbook write-write deadlock, and it is not theoretical — it killed the
// 2026-08-07 correction run on `5m bucket 2026-07-31T01:00:00Z: ... deadlock
// detected (SQLSTATE 40P01)`, which left the correction watermark unadvanced
// and, through it, stalled vendor-bill reconciliation.
//
// Losing the race is the worse outcome of the two. A correction pass that
// rebuilds a partially-purged 5m tier then merges 1h and 1d from it publishes
// an understated day as if it were corrected — silent data loss, where the
// deadlock at least fails loudly. Neither is acceptable, and rebuilding buckets
// that are about to be deleted has no value to begin with, so the window is
// held one whole day inside the horizon rather than merely up to it.
func (j *RollupCorrectionJob) clampToRetention(lookbackDays int) int {
	if j.rollup5mRetentionDays <= 0 {
		// The 5m tier is never purged, so nothing can be racing the window.
		return lookbackDays
	}
	maxDays := j.rollup5mRetentionDays - retentionSafetyDays
	if maxDays < 1 {
		// Retention so short that no window is safe; still rebuild yesterday,
		// which is the day every downstream reader actually depends on.
		maxDays = 1
	}
	if lookbackDays > maxDays {
		j.logger.Info("correction window clamped to stay inside the 5m retention horizon",
			"requestedDays", lookbackDays,
			"effectiveDays", maxDays,
			"rollup5mRetentionDays", j.rollup5mRetentionDays)
		return maxDays
	}
	return lookbackDays
}

func (j *RollupCorrectionJob) ID() string              { return rollupCorrectionJobID }
func (j *RollupCorrectionJob) Name() string            { return rollupCorrectionJobName }
func (j *RollupCorrectionJob) Description() string     { return rollupCorrectionJobDescription }
func (j *RollupCorrectionJob) Interval() time.Duration { return j.interval }

func (j *RollupCorrectionJob) Run(ctx context.Context) error {
	nowFn := j.nowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	if err := runCorrection(ctx, j.r5m, j.merge1h, j.merge1d, j.merge1mo, j.clampToRetention(j.lookbackDays), now, j.logger); err != nil {
		return err
	}
	// Published only after every layer of the window committed. A partial run
	// that failed mid-window leaves the previous watermark standing, so a reader
	// keeps treating the days beyond it as "not rebuilt yet" rather than
	// concluding that an absent series is absent for real.
	return j.advanceWatermark(ctx, correctedThrough(now))
}

// correctedThrough is the newest UTC day a run starting at now has fully
// re-aggregated. The correction loop walks d = lookbackDays…1 days back, so the
// last complete day it rebuilds is yesterday; today is deliberately excluded
// because it is still accumulating events.
func correctedThrough(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
}

// advanceWatermark publishes how far the re-aggregation has reached. A failure
// here fails the job: the rollup rows are already committed and correct, but a
// watermark left behind would keep readers deferring days that are in fact
// rebuilt, and the run status is the only place that is visible.
func (j *RollupCorrectionJob) advanceWatermark(ctx context.Context, through time.Time) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("correction watermark: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := rollupstore.SetWatermark(ctx, tx, WatermarkCorrection, through); err != nil {
		return fmt.Errorf("correction watermark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("correction watermark: commit: %w", err)
	}
	j.logger.Info("correction watermark advanced", "through", through.Format("2006-01-02"))
	return nil
}

// runCorrection re-aggregates the trailing [now-lookbackDays, now) window across
// the 5m, 1h, 1d layers, then re-merges the 1mo layer for any fully-sealed month
// the window reached into. Every bucket write skips the watermark. The
// current (unsealed) month is never re-merged. Buckets are processed from oldest
// to newest, and months in deterministic chronological order.
func runCorrection(
	ctx context.Context,
	r5m correctionRollup,
	merge1h, merge1d, merge1mo correctionMerge,
	lookbackDays int,
	now time.Time,
	logger *slog.Logger,
) error {
	now = now.UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var count5m, count1h, countDays int
	sealedMonths := make(map[time.Time]struct{})

	for d := lookbackDays; d >= 1; d-- {
		dayStart := todayStart.AddDate(0, 0, -d)
		dayEnd := dayStart.AddDate(0, 0, 1)

		for bucket := dayStart; bucket.Before(dayEnd); bucket = bucket.Add(bucketDuration5m) {
			if err := r5m.processBucket(ctx, bucket, false); err != nil {
				return fmt.Errorf("5m bucket %s: %w", bucket.Format(time.RFC3339), err)
			}
			count5m++
		}

		for hour := dayStart; hour.Before(dayEnd); hour = hour.Add(time.Hour) {
			if err := merge1h.mergeBucket(ctx, hour, hour.Add(time.Hour), false); err != nil {
				return fmt.Errorf("1h bucket %s: %w", hour.Format(time.RFC3339), err)
			}
			count1h++
		}

		if err := merge1d.mergeBucket(ctx, dayStart, dayEnd, false); err != nil {
			return fmt.Errorf("1d bucket %s: %w", dayStart.Format(time.RFC3339), err)
		}
		countDays++

		monthStart := time.Date(dayStart.Year(), dayStart.Month(), 1, 0, 0, 0, 0, time.UTC)
		if monthStart.Before(currentMonthStart) {
			sealedMonths[monthStart] = struct{}{}
		}
	}

	// Re-merge each fully-sealed month the window touched, in chronological
	// order. A month merges from metric_rollup_1d, so the 1d re-merges above
	// must already be committed (they are — same loop, earlier).
	months := make([]time.Time, 0, len(sealedMonths))
	for m := range sealedMonths {
		months = append(months, m)
	}
	sort.Slice(months, func(i, k int) bool { return months[i].Before(months[k]) })
	for _, monthStart := range months {
		monthEnd := nextMonth(monthStart)
		if err := merge1mo.mergeBucket(ctx, monthStart, monthEnd, false); err != nil {
			return fmt.Errorf("1mo bucket %s: %w", monthStart.Format("2006-01"), err)
		}
		logger.Info("monthly bucket re-merged", "month", monthStart.Format("2006-01"))
	}

	logger.Info("correction completed",
		"days", countDays,
		"buckets_5m", count5m,
		"buckets_1h", count1h,
	)
	return nil
}
