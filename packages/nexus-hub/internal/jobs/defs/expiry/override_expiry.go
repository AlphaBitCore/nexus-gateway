package expiry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const (
	overrideExpiryJobID = "override-expiry"

	// overrideExpiryEscalateAfter is the consecutive-failure threshold at
	// which the per-row warn log is escalated to error so it shows up in
	// alert filters. 5 was picked so a transient blip (one or two ticks
	// at the 60s default) doesn't wake on-call, but a poisoned row that
	// keeps failing tick-after-tick stops being silent within ~5 min.
	overrideExpiryEscalateAfter = 5
)

// overrideExpiryClearer is the narrow surface OverrideExpiry uses from
// manager.Manager. Defining the interface here keeps the job package
// in charge of its own dependency contract and lets the unit tests
// inject a failing implementation without standing up a real Manager
// (which needs Postgres + a WS pool to construct).
type overrideExpiryClearer interface {
	ClearOverride(ctx context.Context, thingID, configKey, actor string) error
}

// overrideExpiryLister is the read seam: the job needs only
// ListExpiredOverrides off store.Store. Same rationale as the clearer
// interface — narrow surface keeps the test double trivial.
type overrideExpiryLister interface {
	ListExpiredOverrides(ctx context.Context, before time.Time) ([]store.ThingConfigOverride, error)
}

// OverrideExpiry sweeps thing_config_override for rows past their
// expires_at and clears them through the Manager so the audit row + desired
// recompute + WS push paths are identical to admin-initiated clears.
//
// The actor written to admin_audit_log is the constant string
// "system:override-expiry-job" so the trail distinguishes automatic clears
// from admin clears without bolting on a new actor type.
//
// Per-row failure tracking:
//   - rowFailureCounter increments on every ClearOverride error so /metrics
//     surfaces a single global "this job is unhappy" signal.
//   - failures{thingID:configKey → consecutive-fail count} drives the
//     log-level escalation. The 5th consecutive failure for the same
//     row escalates the structured warn to error so on-call alert
//     filters trip. Reset on success or when the row leaves the
//     expired-list (admin extended the TTL, deleted the row directly).
type OverrideExpiry struct {
	st       overrideExpiryLister
	mgr      overrideExpiryClearer
	interval time.Duration
	logger   *slog.Logger

	rowFailureCounter *opsmetrics.Counter

	failuresMu sync.Mutex
	failures   map[string]int
}

// NewOverrideExpiry creates the override-expiry job with the supplied tick.
// 60s is the planned default; ops can tune via cfg.Scheduler.OverrideExpiryInterval.
//
// `reg` may be nil — when no opsmetrics registry is wired (test harness,
// scheduler-disabled mode) the counter calls become safe no-ops.
func NewOverrideExpiry(
	st *store.Store,
	mgr *manager.Manager,
	interval time.Duration,
	reg *opsmetrics.Registry,
	logger *slog.Logger,
) *OverrideExpiry {
	j := &OverrideExpiry{
		interval: interval,
		logger:   logger.With("job", overrideExpiryJobID),
		failures: make(map[string]int),
	}
	// Wire concrete deps through the narrow interfaces so the same struct
	// shape is reused by tests via newOverrideExpiryWithDeps below.
	if st != nil {
		j.st = st.OverrideStore()
	}
	if mgr != nil {
		j.mgr = mgr
	}
	if reg != nil {
		j.rowFailureCounter = reg.NewCounter("override_expiry.row_failures_total", nil)
	}
	return j
}

func (j *OverrideExpiry) ID() string   { return overrideExpiryJobID }
func (j *OverrideExpiry) Name() string { return "Override Expiry" }
func (j *OverrideExpiry) Description() string {
	return "Clears thing_config_override rows past their expires_at."
}
func (j *OverrideExpiry) Interval() time.Duration { return j.interval }
func (j *OverrideExpiry) RunOnStart() bool        { return true }

// Run lists every override with expires_at < now and clears each one. A
// failure on any single row is logged + counted but does not abort the
// loop; the next tick will retry the row that failed. After 5
// consecutive failures for the same (thing_id, config_key) the per-row
// log is escalated to Error so alert filters trip.
func (j *OverrideExpiry) Run(ctx context.Context) error {
	expired, err := j.st.ListExpiredOverrides(ctx, time.Now().UTC())
	if err != nil {
		return err
	}

	// Track which keys we saw this tick so we can reap failure-map
	// entries whose row is no longer expired (admin extended TTL,
	// directly deleted, etc.). Without reaping, an entry could keep
	// the "consecutive failures" count forever even though the row is
	// long gone — defeating the reset-on-success semantics.
	seenKeys := make(map[string]struct{}, len(expired))

	for _, o := range expired {
		key := o.ThingID + ":" + o.ConfigKey
		seenKeys[key] = struct{}{}

		if err := j.mgr.ClearOverride(ctx, o.ThingID, o.ConfigKey, "system:override-expiry-job"); err != nil {
			fails := j.recordFailure(key)
			if j.rowFailureCounter != nil {
				j.rowFailureCounter.With().Inc()
			}
			level := slog.LevelWarn
			if fails >= overrideExpiryEscalateAfter {
				level = slog.LevelError
			}
			j.logger.Log(ctx, level, "override expiry: clear failed",
				slog.String("event", "override_expiry_clear_failed"),
				slog.String("thing_id", o.ThingID),
				slog.String("config_key", o.ConfigKey),
				slog.Int("consecutive_failures", fails),
				slog.String("error", err.Error()),
			)
			continue
		}

		// Success path: reset the consecutive-failure count for this row.
		j.resetFailure(key)
		j.logger.Info("override expiry: cleared",
			slog.String("event", "override_expiry_cleared"),
			slog.String("thing_id", o.ThingID),
			slog.String("config_key", o.ConfigKey),
		)
	}

	j.reapStaleFailures(seenKeys)
	return nil
}

// recordFailure increments the consecutive-failure count for `key` and
// returns the new value. Holds failuresMu for the duration so concurrent
// ticks (the scheduler runs jobs sequentially today, but defence-in-depth
// keeps us safe if that changes) cannot interleave reads and writes.
func (j *OverrideExpiry) recordFailure(key string) int {
	j.failuresMu.Lock()
	defer j.failuresMu.Unlock()
	j.failures[key]++
	return j.failures[key]
}

// resetFailure deletes the failure-map entry for `key`. Called on
// success so the next failure for the same row starts the count at 1.
func (j *OverrideExpiry) resetFailure(key string) {
	j.failuresMu.Lock()
	defer j.failuresMu.Unlock()
	delete(j.failures, key)
}

// reapStaleFailures removes failure-map entries whose row no longer
// appears in the current expired-list. Called once per Run after every
// row has been processed.
func (j *OverrideExpiry) reapStaleFailures(seen map[string]struct{}) {
	j.failuresMu.Lock()
	defer j.failuresMu.Unlock()
	for k := range j.failures {
		if _, ok := seen[k]; !ok {
			delete(j.failures, k)
		}
	}
}
