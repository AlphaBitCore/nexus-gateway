package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

const (
	rollupCorrectionJobID          = "rollup-correction"
	rollupCorrectionJobName        = "Rollup Correction"
	rollupCorrectionJobDescription = "Recomputes rollups for the trailing correction window (default 7 days) to absorb late-arriving events. Re-runs every 5-minute bucket, then re-merges the 1h, 1d, and (for any sealed month touched) 1mo layers."

	// correctionLookbackDays is the default trailing window the correction job
	// recomputes. It bounds how late an event can arrive and still be folded
	// into the rollups: an event written more than this many days after its
	// timestamp lands in a sealed bucket outside the window and would otherwise
	// be present in raw traffic_event but in no rollup. Sized to cover
	// an agent that buffered offline for several days before reconnecting.
	correctionLookbackDays = 7
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
	r5m          correctionRollup
	merge1h      correctionMerge
	merge1d      correctionMerge
	merge1mo     correctionMerge
	lookbackDays int
	interval     time.Duration
	logger       *slog.Logger
	// nowFn returns the current time; defaults to time.Now. Seam so tests can pin a
	// date deterministically — the monthly re-merge fires only for a month the
	// lookback window reaches into and that is already sealed.
	nowFn func() time.Time
}

// NewRollupCorrection constructs the fleet correction job. interval defaults to
// 24h; lookbackDays defaults to correctionLookbackDays when zero or negative.
// The four sibling jobs must have been constructed with the same pool so they
// share a transaction surface and configuration.
func NewRollupCorrection(
	r5m *Rollup5mJob,
	merge1h, merge1d, merge1mo *RollupMergeJob,
	lookbackDays int,
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
		r5m:          r5m,
		merge1h:      merge1h,
		merge1d:      merge1d,
		merge1mo:     merge1mo,
		lookbackDays: lookbackDays,
		interval:     interval,
		logger:       logger.With("job", rollupCorrectionJobID),
		nowFn:        time.Now,
	}
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
	return runCorrection(ctx, j.r5m, j.merge1h, j.merge1d, j.merge1mo, j.lookbackDays, nowFn().UTC(), j.logger)
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
