// Package jobs contains Hub scheduler jobs: drift detection, identity enrichment, enrollment token cleanup.
package drift

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const (
	driftJobID          = "config-drift-check"
	driftJobName        = "Config Drift Detection"
	driftJobDescription = "Detects Things whose reported config version differs from desired and triggers repair."
	driftMaxRetries     = 3
	driftRetryTTL       = 5 * time.Minute
	driftKeyPrefix      = "nexus:drift:retry:"

	// contentRetryKeyPrefix namespaces the content-drift retry counter so it
	// never collides with the version-drift counter (driftKeyPrefix): a Thing
	// can legitimately be retried for both a version mismatch and, separately,
	// equal-version content divergence.
	contentRetryKeyPrefix = "nexus:drift:content-retry:"

	// contentPassEveryNTicks throttles the content-reconcile pass to
	// every Nth Run tick. The version pass runs every tick because its query is
	// a single indexed scan (desired_ver != reported_ver). The content pass is
	// far heavier: it issues one GetThing join per equal-version online Thing
	// and marshals every desired/reported key to compare them, so a fleet-wide
	// content diff every tick (60s default) would be wasteful. Content
	// divergence only arises from rare events — a manual DB edit, or a
	// dropped/partial apply that still re-stamped the version — none of which
	// need sub-10-minute detection. At the 60s default this is one content
	// sweep every ~10 minutes; it is strictly defense-in-depth layered on top
	// of the (unchanged, every-tick) version pass, never a replacement for it.
	contentPassEveryNTicks = 10
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

	// Content-reconcile pass metrics + cadence state.
	contentDriftThings      *opsmetrics.Gauge
	contentRepairsAttempted *opsmetrics.Counter
	contentRepairsExhausted *opsmetrics.Counter

	// tickCount counts Run invocations; the content pass fires when
	// tickCount % contentPassEvery == 0. Atomic because a manual scheduler
	// trigger can overlap a cron tick (scheduler.runOne is re-entrant).
	tickCount atomic.Int64
	// contentPassEvery is the tick stride for the content pass; defaults to
	// contentPassEveryNTicks. A field (not the const directly) so tests can
	// force every-tick execution without waiting 10 ticks.
	contentPassEvery int64
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
		store:            st,
		mgr:              mgr,
		redis:            rdb,
		interval:         interval,
		logger:           logger.With("job", driftJobID),
		contentPassEvery: contentPassEveryNTicks,
	}
	if reg != nil {
		d.thingsTotal = reg.NewGauge("shadow.drift_things", nil)
		d.repairsAttempted = reg.NewCounter("drift.repairs_attempted_total", nil)
		d.repairsExhausted = reg.NewCounter("drift.repairs_exhausted_total", nil)
		d.checkDurationMs = reg.NewHistogram("drift.check_duration_ms", nil)
		d.contentDriftThings = reg.NewGauge("shadow.content_drift_things", nil)
		d.contentRepairsAttempted = reg.NewCounter("drift.content_repairs_attempted_total", nil)
		d.contentRepairsExhausted = reg.NewCounter("drift.content_repairs_exhausted_total", nil)
	}
	return d
}

func (d *DriftDetector) ID() string              { return driftJobID }
func (d *DriftDetector) Name() string            { return driftJobName }
func (d *DriftDetector) Description() string     { return driftJobDescription }
func (d *DriftDetector) Interval() time.Duration { return d.interval }

// Run runs the version pass every tick and, every Nth tick, the slower content
// pass. The tick counter advances once per Run. The content pass is
// gated on tick stride AND always runs after the version pass so a busy version
// pass never starves it; its failures are isolated inside contentPass and never
// fail the job.
func (d *DriftDetector) Run(ctx context.Context) error {
	start := time.Now()
	defer func() {
		if d.checkDurationMs != nil {
			d.checkDurationMs.With().Observe(float64(time.Since(start).Milliseconds()))
		}
	}()

	contentDue := d.runContentPassDue()

	verErr := d.versionPass(ctx)

	// Content pass runs even when the version pass found nothing (the whole
	// point of it is equal-version divergence the version pass can't see)
	// but is skipped when the version pass errored, because that error is
	// almost always the shared DB being unreachable — re-issuing a fleet-wide
	// content scan against a down DB just amplifies the failure.
	if contentDue && verErr == nil {
		d.contentPass(ctx)
	}
	return verErr
}

// versionPass is the original version-only drift detection (desired_ver !=
// reported_ver). It is unchanged behavior, extracted so Run can layer the
// content pass on top without altering the version path.
func (d *DriftDetector) versionPass(ctx context.Context) error {
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

// runContentPassDue reports whether this tick should also run the content
// pass and advances the tick counter exactly once per Run.
func (d *DriftDetector) runContentPassDue() bool {
	if d.contentPassEvery <= 0 {
		return false
	}
	return d.tickCount.Add(1)%d.contentPassEvery == 0
}

// contentPass is the defense-in-depth companion to the version pass.
// The version pass (FindDriftedThings) only sees Things where
// desired_ver != reported_ver, so a Thing whose reported CONTENT diverged from
// desired while the versions stayed EQUAL — a manual DB edit, or a
// dropped/partial apply that still re-stamped the reported version — is
// invisible to it. contentPass closes that gap: for online, equal-version
// Things it runs a per-key desired-vs-reported diff via the existing
// Manager.GetShadowComparison helper and heals any Thing whose keys disagree.
//
// It runs on a slower cadence than the version pass (see contentPassEveryNTicks)
// because the per-Thing GetShadowComparison join is far heavier than the single
// indexed scan the version pass uses. Failures are logged, never propagated:
// the content pass is strictly additive and must not regress the version pass,
// which already ran (and was accounted) before this is invoked.
// contentPass returns the number of Things found content-divergent (and thus
// handed to handleContentDrift) this pass — used by the gauge and by tests to
// assert that converged Things are left untouched.
func (d *DriftDetector) contentPass(ctx context.Context) int {
	candidates, err := d.store.RegistryStore().FindEqualVersionOnlineThings(ctx)
	if err != nil {
		d.logger.Warn("content drift: find candidates failed", "error", err)
		return 0
	}

	diverged := 0
	for _, c := range candidates {
		cmp, err := d.mgr.GetShadowComparison(ctx, c.ID)
		if err != nil {
			d.logger.Warn("content drift: shadow comparison failed", "thing_id", c.ID, "error", err)
			continue
		}
		if contentConverged(cmp) {
			// All keys agree — leave the Thing alone. A blanket re-push of a
			// converged Thing would be a false repair (and a needless
			// desired_ver bump), so the content pass MUST be diff-gated.
			continue
		}
		diverged++
		if err := d.handleContentDrift(ctx, c); err != nil {
			d.logger.Warn("content drift handle failed", "thing_id", c.ID, "error", err)
		}
	}

	if d.contentDriftThings != nil {
		d.contentDriftThings.With().Set(float64(diverged))
	}
	if diverged > 0 {
		d.logger.Info("content drift pass found divergent things",
			"candidates", len(candidates), "diverged", diverged)
	}
	return diverged
}

// contentConverged reports whether every DESIRED key agrees between desired and
// reported. A nil comparison is treated as converged (nothing actionable). Note
// this checks per-key content equality, NOT cmp.Synced — cmp.Synced is the
// version-level (reportedVer >= desiredVer) signal, which is always true for the
// equal-version population the content pass scans; the whole point is
// that version-equal can still be content-divergent.
//
// CRITICAL: only keys present in DESIRED count. A reported-only key (kd.InDesired
// == false) is one the Thing still reports but the Hub no longer desires — the
// reported map is merge-only and never prunes (UpdateShadowReport in fleet/store),
// so such keys linger indefinitely. They are NOT actionable drift: ForceResyncAll
// pushes only desired keys, so it can never clear a reported-only key. Treating
// one as drift would force-resync every content pass forever (the retry budget
// never exhausts because its TTL < the content cadence) — fleet-wide churn that
// inflates desired_ver. Skip them.
func contentConverged(cmp *manager.ShadowComparison) bool {
	if cmp == nil {
		return true
	}
	for _, kd := range cmp.Keys {
		if !kd.InDesired {
			continue
		}
		if !kd.Synced {
			return false
		}
	}
	return true
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

// handleContentDrift heals one Thing flagged by the content pass, mirroring
// handleDriftedThing's retry/exhaustion machinery but with its own Redis key
// namespace. After driftMaxRetries failed content heals it marks the Thing
// 'drift' for ops visibility (it won't self-converge) and stops re-pushing.
func (d *DriftDetector) handleContentDrift(ctx context.Context, c store.ContentCheckCandidate) error {
	if d.redis == nil {
		// Without Redis, always attempt the heal.
		return d.attemptContentRepair(ctx, c)
	}

	key := contentRetryKeyPrefix + c.ID
	retries, err := d.redis.Incr(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("redis incr: %w", err)
	}
	d.redis.Expire(ctx, key, driftRetryTTL)

	if retries > driftMaxRetries {
		if d.contentRepairsExhausted != nil {
			d.contentRepairsExhausted.With().Inc()
		}
		d.logger.Warn("content drift retries exhausted, marking drift", "thing_id", c.ID, "retries", retries)
		return d.store.RegistryStore().UpdateThingStatus(ctx, c.ID, "drift")
	}

	return d.attemptContentRepair(ctx, c)
}

// attemptContentRepair heals an equal-version content divergence. It uses
// ForceResyncAll, NOT RePushConfig: the version pass's RePushConfig replays
// each key at the CURRENT desired_ver without Force, which a Thing at
// desired_ver == reported_ver short-circuits as a no-op (thingclient applies a
// config_changed only when DesiredVer > reportedVer, unless Force is set).
// ForceResyncAll instead bumps desired_ver once and force-pushes every key, so
// the heal reaches both WS-connected Things (Force bypasses the equality
// short-circuit) and HTTP-fallback Things (the version bump drives the next
// heartbeat pull) — exactly the reliable-delivery guarantee. After the
// bump the Thing is momentarily version-unequal and converges through the
// normal version path on subsequent ticks.
func (d *DriftDetector) attemptContentRepair(ctx context.Context, c store.ContentCheckCandidate) error {
	if d.contentRepairsAttempted != nil {
		d.contentRepairsAttempted.With().Inc()
	}
	d.logger.Info("attempting content drift repair", "thing_id", c.ID, "type", c.Type)
	_, err := d.mgr.ForceResyncAll(ctx, c.ID)
	return err
}
