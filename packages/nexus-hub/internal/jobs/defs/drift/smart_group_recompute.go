package drift

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

const (
	smartGroupRecomputeJobID   = "smart-group-recompute"
	smartGroupRecomputeJobName = "Smart Group Membership Recompute"
	smartGroupRecomputeJobDesc = "Re-evaluates every smart DeviceGroup's membership_query against the current device fleet and replaces the device_group_membership_cache rows. Runs every 60s as a safety net; heartbeat-driven recomputes handle the steady-state."
)

// smartGroupStore is the narrow subset SmartGroupRecomputeJob depends on.
// Declared as an interface so tests can wire an in-memory fake without
// standing up Postgres.
type smartGroupStore interface {
	ListSmartGroups(ctx context.Context) ([]store.SmartGroupSnapshot, error)
	LoadDevicesForSmartGroupEval(ctx context.Context) ([]struct {
		ID  string
		Dev device.Device
	}, error)
	ReplaceSmartGroupCache(ctx context.Context, groupID string, deviceIDs []string) error
	// EvictExpiredMemberships removes static-group rows whose expires_at has
	// passed. Returns the count of rows removed for observability. Runs on the
	// same 60s tick as smart-group recompute so expired memberships are cleaned
	// within one tick of the deadline.
	EvictExpiredMemberships(ctx context.Context) (int, error)
}

// SmartGroupRecomputeJob re-evaluates every smart-group predicate and
// rebuilds device_group_membership_cache. Class 1 (state-table) — the
// CP IAM middleware reads the cache on the request path, so this
// keeps it within ~60s of truth even when the heartbeat-driven
// recompute path misses (e.g. devices that haven't heartbeated since
// the predicate changed).
//
// Failures are recorded per-group: one bad predicate doesn't take out
// the recompute for the rest of the fleet. The job logs and returns
// the joined error so the scheduler's standard alerting fires.
type SmartGroupRecomputeJob struct {
	store    smartGroupStore
	interval time.Duration
	logger   *slog.Logger
}

// NewSmartGroupRecompute constructs the job. interval defaults to 60s.
func NewSmartGroupRecompute(s smartGroupStore, interval time.Duration, logger *slog.Logger) *SmartGroupRecomputeJob {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &SmartGroupRecomputeJob{
		store:    s,
		interval: interval,
		logger:   logger.With("job", smartGroupRecomputeJobID),
	}
}

func (j *SmartGroupRecomputeJob) ID() string              { return smartGroupRecomputeJobID }
func (j *SmartGroupRecomputeJob) Name() string            { return smartGroupRecomputeJobName }
func (j *SmartGroupRecomputeJob) Description() string     { return smartGroupRecomputeJobDesc }
func (j *SmartGroupRecomputeJob) Interval() time.Duration { return j.interval }

func (j *SmartGroupRecomputeJob) Run(ctx context.Context) error {
	// Evict expired static-group memberships before the smart-group recompute
	// so the re-resolution path sees clean state immediately. A failure here
	// is logged but does not abort the recompute — expired rows are filtered
	// at read time in GroupsOfDevice / MembersOfGroup anyway, so leaving them
	// on disk is safe; this eviction is hygiene, not correctness.
	if evicted, evErr := j.store.EvictExpiredMemberships(ctx); evErr != nil {
		j.logger.Warn("evict expired memberships failed", "error", evErr)
	} else if evicted > 0 {
		j.logger.Info("evicted expired memberships", "count", evicted)
	}

	groups, err := j.store.ListSmartGroups(ctx)
	if err != nil {
		// Continue if the error was per-group; ListSmartGroups
		// returns successfully-parsed groups even when one is bad.
		if len(groups) == 0 {
			return fmt.Errorf("list smart groups: %w", err)
		}
		j.logger.Warn("partial smart-group list", "error", err, "loaded", len(groups))
	}
	if len(groups) == 0 {
		// Empty fleet of smart groups is the common case until
		// operators start creating them. No work, no log noise.
		return nil
	}

	devices, err := j.store.LoadDevicesForSmartGroupEval(ctx)
	if err != nil {
		return fmt.Errorf("load devices: %w", err)
	}

	nowSec := time.Now().UTC().Unix()
	var errs []error
	for _, g := range groups {
		matched := make([]string, 0, len(devices))
		for i := range devices {
			ok, evalErr := device.Evaluate(g.Predicate, &devices[i].Dev, nowSec)
			if evalErr != nil {
				// Predicate shape error — record once per group and
				// skip evaluating the rest of the devices against
				// this group. The cache for this group is left
				// untouched (stale > wrong).
				errs = append(errs, fmt.Errorf("group %s: %w", g.ID, evalErr))
				j.logger.Warn("smart-group predicate error", "group_id", g.ID, "error", evalErr)
				matched = nil
				break
			}
			if ok {
				matched = append(matched, devices[i].ID)
			}
		}
		if matched == nil {
			continue // skip cache write on per-group error
		}
		if err := j.store.ReplaceSmartGroupCache(ctx, g.ID, matched); err != nil {
			errs = append(errs, fmt.Errorf("write cache for %s: %w", g.ID, err))
			j.logger.Warn("smart-group cache write failed", "group_id", g.ID, "error", err)
			continue
		}
		j.logger.Info("smart-group recomputed",
			"group_id", g.ID,
			"matched", len(matched),
			"total_devices", len(devices),
		)
	}
	return errors.Join(errs...)
}
