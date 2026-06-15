package rollup

import (
	"context"
	"log/slog"
	"time"
)

const (
	thingRollupCorrectionJobID          = "thing-rollup-correction"
	thingRollupCorrectionJobName        = "Per-Thing Rollup Correction"
	thingRollupCorrectionJobDescription = "Recomputes per-Thing rollups for the trailing correction window (default 7 days) to absorb late-arriving events. The per-Thing twin of rollup-correction; without it a late event whose per-Thing 5m bucket already sealed would never be re-aggregated and per-Thing dashboards would permanently under-count."
)

// ThingRollupCorrectionJob is the per-Thing twin of RollupCorrectionJob. The
// per-Thing pipeline (thing-rollup-5m → thing-merge-1h/1d/1mo) seals buckets
// behind a live watermark exactly like the fleet pipeline, so it needs the
// same trailing-window re-aggregation to absorb late writes. It shares the
// fleet correction's runCorrection logic via the correctionRollup /
// correctionMerge seams, and never advances the live watermark.
type ThingRollupCorrectionJob struct {
	r5m          correctionRollup
	merge1h      correctionMerge
	merge1d      correctionMerge
	merge1mo     correctionMerge
	lookbackDays int
	interval     time.Duration
	logger       *slog.Logger
	// nowFn returns the current time; defaults to time.Now. Test seam mirroring
	// RollupCorrectionJob.nowFn.
	nowFn func() time.Time
}

// NewThingRollupCorrection constructs the per-Thing correction job. interval
// defaults to 24h; lookbackDays defaults to correctionLookbackDays when zero or
// negative. The four sibling jobs must share the same pool.
func NewThingRollupCorrection(
	r5m *ThingRollup5mJob,
	merge1h, merge1d, merge1mo *ThingRollupMergeJob,
	lookbackDays int,
	interval time.Duration,
	logger *slog.Logger,
) *ThingRollupCorrectionJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if lookbackDays <= 0 {
		lookbackDays = correctionLookbackDays
	}
	return &ThingRollupCorrectionJob{
		r5m:          r5m,
		merge1h:      merge1h,
		merge1d:      merge1d,
		merge1mo:     merge1mo,
		lookbackDays: lookbackDays,
		interval:     interval,
		logger:       logger.With("job", thingRollupCorrectionJobID),
		nowFn:        time.Now,
	}
}

func (j *ThingRollupCorrectionJob) ID() string              { return thingRollupCorrectionJobID }
func (j *ThingRollupCorrectionJob) Name() string            { return thingRollupCorrectionJobName }
func (j *ThingRollupCorrectionJob) Description() string     { return thingRollupCorrectionJobDescription }
func (j *ThingRollupCorrectionJob) Interval() time.Duration { return j.interval }

func (j *ThingRollupCorrectionJob) Run(ctx context.Context) error {
	nowFn := j.nowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return runCorrection(ctx, j.r5m, j.merge1h, j.merge1d, j.merge1mo, j.lookbackDays, nowFn().UTC(), j.logger)
}
