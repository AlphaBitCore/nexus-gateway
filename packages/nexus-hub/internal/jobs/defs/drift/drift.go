// Package jobs contains Hub scheduler jobs: drift detection, identity enrichment, enrollment token cleanup.
package drift

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const (
	driftJobID          = "config-drift-check"
	driftJobName        = "Config Drift Detection"
	driftJobDescription = "Detects Things whose reported config version differs from desired and triggers repair."
	driftMaxRetries     = 3
	driftRetryTTL       = 5 * time.Minute
	driftKeyPrefix      = "nexus:drift:retry:"
)

// DriftDetector detects config drift and attempts auto-repair.
type DriftDetector struct {
	store    *store.Store
	mgr      *manager.Manager
	redis    redis.UniversalClient
	interval time.Duration
	logger   *slog.Logger

	thingsTotal      *opsmetrics.Gauge
	repairsAttempted *opsmetrics.Counter
	repairsExhausted *opsmetrics.Counter
	checkDurationMs  *opsmetrics.Histogram
}

// NewDriftDetector creates a drift detection job. The opsmetrics registry
// powers both the /metrics scrape surface and the per-tick metrics_sample
// push to Hub's own thing row (so Hub-side dashboards see drift counts).
//
// Spec catalog mapping (§6.3):
//   - shadow.drift_things                — gauge
//   - jobs.runs_total{name, status}      — handled by scheduler, not here
//   - jobs.duration_ms{name}             — handled by scheduler, not here
//
// repairsAttempted/repairsExhausted are drift-specific and not in the
// spec catalog; they are kept as job-internal counters with names that
// avoid collisions on /metrics.
func NewDriftDetector(
	st *store.Store,
	mgr *manager.Manager,
	rdb redis.UniversalClient,
	interval time.Duration,
	reg *opsmetrics.Registry,
	logger *slog.Logger,
) *DriftDetector {
	d := &DriftDetector{
		store:    st,
		mgr:      mgr,
		redis:    rdb,
		interval: interval,
		logger:   logger.With("job", driftJobID),
	}
	if reg != nil {
		d.thingsTotal = reg.NewGauge("shadow.drift_things", nil)
		d.repairsAttempted = reg.NewCounter("drift.repairs_attempted_total", nil)
		d.repairsExhausted = reg.NewCounter("drift.repairs_exhausted_total", nil)
		d.checkDurationMs = reg.NewHistogram("drift.check_duration_ms", nil)
	}
	return d
}

func (d *DriftDetector) ID() string              { return driftJobID }
func (d *DriftDetector) Name() string            { return driftJobName }
func (d *DriftDetector) Description() string     { return driftJobDescription }
func (d *DriftDetector) Interval() time.Duration { return d.interval }

// Run finds drifted Things and attempts repair.
func (d *DriftDetector) Run(ctx context.Context) error {
	start := time.Now()
	defer func() {
		if d.checkDurationMs != nil {
			d.checkDurationMs.With().Observe(float64(time.Since(start).Milliseconds()))
		}
	}()

	drifted, err := d.store.RegistryStore().FindDriftedThings(ctx)
	if err != nil {
		return fmt.Errorf("find drifted things: %w", err)
	}

	if d.thingsTotal != nil {
		d.thingsTotal.With().Set(float64(len(drifted)))
	}
	if len(drifted) == 0 {
		return nil
	}

	d.logger.Info("drift check found mismatched things", "count", len(drifted))

	for _, dt := range drifted {
		if err := d.handleDriftedThing(ctx, dt); err != nil {
			d.logger.Warn("drift handle failed", "thing_id", dt.ID, "error", err)
		}
	}
	return nil
}

func (d *DriftDetector) handleDriftedThing(ctx context.Context, dt store.DriftedThing) error {
	if d.redis == nil {
		// Without Redis, always attempt repair
		return d.attemptRepair(ctx, dt)
	}

	key := driftKeyPrefix + dt.ID
	retries, err := d.redis.Incr(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("redis incr: %w", err)
	}
	d.redis.Expire(ctx, key, driftRetryTTL)

	if retries > driftMaxRetries {
		if d.repairsExhausted != nil {
			d.repairsExhausted.With().Inc()
		}
		d.logger.Warn("drift retries exhausted, marking drift", "thing_id", dt.ID, "retries", retries)
		return d.store.RegistryStore().UpdateThingStatus(ctx, dt.ID, "drift")
	}

	return d.attemptRepair(ctx, dt)
}

func (d *DriftDetector) attemptRepair(ctx context.Context, dt store.DriftedThing) error {
	if d.repairsAttempted != nil {
		d.repairsAttempted.With().Inc()
	}
	d.logger.Info("attempting drift repair", "thing_id", dt.ID, "desired_ver", dt.DesiredVer, "reported_ver", dt.ReportedVer)
	return d.mgr.RePushConfig(ctx, dt.ID, dt.Type)
}
