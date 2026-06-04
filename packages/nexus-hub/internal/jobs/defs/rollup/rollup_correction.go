package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	rollupCorrectionJobID          = "rollup-correction"
	rollupCorrectionJobName        = "Rollup Correction"
	rollupCorrectionJobDescription = "Recomputes rollups for T-1 (yesterday) to absorb late-arriving events. Re-runs every 5-minute bucket, then re-merges the 1h, 1d, and (on month boundary) 1mo layers."
)

// RollupCorrectionJob recomputes all rollup layers for yesterday's buckets.
// Events may land in traffic_event after their bucket has already been
// rolled up — rewinding T-1 once per day catches those late writes without
// churning the live watermark path. Delegates to the existing 5m / merge
// jobs so the aggregation logic stays in one place.
type RollupCorrectionJob struct {
	r5m      *Rollup5mJob
	merge1h  *RollupMergeJob
	merge1d  *RollupMergeJob
	merge1mo *RollupMergeJob
	interval time.Duration
	logger   *slog.Logger
	// nowFn returns the current time; defaults to time.Now. Seam so tests can pin a
	// date deterministically — the monthly re-merge fires only on the 1st of a month
	// (when yesterday was a month-end), a branch that is otherwise reachable in tests
	// only on that calendar day.
	nowFn func() time.Time
}

// NewRollupCorrection constructs the job. interval defaults to 24h.
// The four sibling jobs must have been constructed with the same pool so
// they share a transaction surface and configuration.
func NewRollupCorrection(
	r5m *Rollup5mJob,
	merge1h, merge1d, merge1mo *RollupMergeJob,
	interval time.Duration,
	logger *slog.Logger,
) *RollupCorrectionJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &RollupCorrectionJob{
		r5m:      r5m,
		merge1h:  merge1h,
		merge1d:  merge1d,
		merge1mo: merge1mo,
		interval: interval,
		logger:   logger.With("job", rollupCorrectionJobID),
		nowFn:    time.Now,
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
	now := nowFn().UTC()
	yesterdayStart := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	var count5m int
	for bucket := yesterdayStart; bucket.Before(yesterdayEnd); bucket = bucket.Add(bucketDuration5m) {
		if err := j.r5m.processOneBucket(ctx, bucket); err != nil {
			return fmt.Errorf("5m bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
		count5m++
	}

	var count1h int
	for hour := yesterdayStart; hour.Before(yesterdayEnd); hour = hour.Add(time.Hour) {
		if err := j.merge1h.mergeOneBucket(ctx, hour, hour.Add(time.Hour)); err != nil {
			return fmt.Errorf("1h bucket %s: %w", hour.Format(time.RFC3339), err)
		}
		count1h++
	}

	if err := j.merge1d.mergeOneBucket(ctx, yesterdayStart, yesterdayEnd); err != nil {
		return fmt.Errorf("1d bucket %s: %w", yesterdayStart.Format(time.RFC3339), err)
	}

	// If yesterday was the last day of its month, re-merge the monthly bucket.
	if yesterdayEnd.Day() == 1 {
		monthStart := time.Date(yesterdayStart.Year(), yesterdayStart.Month(), 1, 0, 0, 0, 0, time.UTC)
		if err := j.merge1mo.mergeOneBucket(ctx, monthStart, yesterdayEnd); err != nil {
			return fmt.Errorf("1mo bucket %s: %w", monthStart.Format("2006-01"), err)
		}
		j.logger.Info("monthly bucket re-merged", "month", monthStart.Format("2006-01"))
	}

	j.logger.Info("correction completed",
		"date", yesterdayStart.Format("2006-01-02"),
		"buckets_5m", count5m,
		"buckets_1h", count1h,
	)
	return nil
}
