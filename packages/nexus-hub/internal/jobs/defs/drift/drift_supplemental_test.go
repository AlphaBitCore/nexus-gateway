// drift_supplemental_test.go covers the remaining statement gaps in the drift
// package not addressed by the primary per-file test files:
//   - SmartGroupRecomputeJob.ID/Name/Description (0%)
//   - SmartGroupRecomputeJob.Run partial-list path (listErr != nil, groups > 0)
//   - SmartGroupRecomputeJob.Run EvictExpiredMemberships error (warn path)
//   - SmartGroupRecomputeJob.Run LoadDevicesForSmartGroupEval error
//   - SmartGroupRecomputeJob.Run ReplaceSmartGroupCache error (continue path)
//   - ExemptionGCJob.Run UpdateConfig error path
//   - DriftDetector.handleDriftedThing Redis INCR error
package drift

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

// SmartGroupRecomputeJob — identity accessors (0% in baseline)

func TestSmartGroupRecompute_IDNameDescription(t *testing.T) {
	j := NewSmartGroupRecompute(&fakeSmartGroupStore{}, time.Second, testLogger())
	if j.ID() != smartGroupRecomputeJobID {
		t.Errorf("ID = %q, want %q", j.ID(), smartGroupRecomputeJobID)
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
}

// SmartGroupRecomputeJob — partial list (listErr != nil, len(groups) > 0)

// TestSmartGroupRecompute_PartialListContinues covers the branch in Run where
// ListSmartGroups returns both an error AND a non-empty slice: the job logs a
// warning and processes the groups it received.
func TestSmartGroupRecompute_PartialListContinues(t *testing.T) {
	fake := &fakeSmartGroupStore{
		groups: []store.SmartGroupSnapshot{
			{ID: "g-ok", Predicate: device.Predicate{All: []device.Leaf{
				{Field: "os", Op: "eq", Value: "linux"},
			}}},
		},
		listErr: errors.New("partial read"),
		devices: []struct {
			ID  string
			Dev device.Device
		}{
			{ID: "dev-linux", Dev: device.Device{OS: "linux"}},
		},
	}
	j := NewSmartGroupRecompute(fake, time.Second, testLogger())

	// Run should not return an error (list warning only; no per-group errors).
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// g-ok must have been recomputed despite the list error.
	if got := fake.writes["g-ok"]; len(got) != 1 || got[0] != "dev-linux" {
		t.Errorf("g-ok members = %v, want [dev-linux]", got)
	}
}

// SmartGroupRecomputeJob — EvictExpiredMemberships error (warn, not abort)

func TestSmartGroupRecompute_EvictError_DoesNotAbort(t *testing.T) {
	fake := &fakeSmartGroupStore{
		evictionsErr: errors.New("evict failed"),
		// no smart groups → Run exits after eviction; the eviction error is warned only
	}
	j := NewSmartGroupRecompute(fake, time.Second, testLogger())

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.evictCalls != 1 {
		t.Errorf("EvictExpiredMemberships called %d times, want 1", fake.evictCalls)
	}
}

// SmartGroupRecomputeJob — LoadDevicesForSmartGroupEval error propagates

func TestSmartGroupRecompute_LoadDevicesError(t *testing.T) {
	sentinel := errors.New("devices unavailable")
	fake := &fakeSmartGroupStore{
		groups: []store.SmartGroupSnapshot{
			{ID: "g-x", Predicate: device.Predicate{}},
		},
		devicesErr: sentinel,
	}
	j := NewSmartGroupRecompute(fake, time.Second, testLogger())

	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
}

// SmartGroupRecomputeJob — ReplaceSmartGroupCache error (per-group, continues)

func TestSmartGroupRecompute_CacheWriteError_Continues(t *testing.T) {
	fake := &fakeSmartGroupStore{
		groups: []store.SmartGroupSnapshot{
			{ID: "g-fail", Predicate: device.Predicate{}},
			{ID: "g-fail2", Predicate: device.Predicate{}},
		},
		devices: []struct {
			ID  string
			Dev device.Device
		}{
			{ID: "dev-1", Dev: device.Device{}},
		},
		writeErr: errors.New("write failed"),
	}
	j := NewSmartGroupRecompute(fake, time.Second, testLogger())

	// Both groups fail the cache write — errors are joined and returned.
	err := j.Run(context.Background())
	if err == nil {
		t.Error("expected joined error from cache write failures")
	}
}

// ExemptionGCJob updater-error propagation is already covered by
// exemption_gc_test.go's existing TestExemptionGC_* suite using the
// fakeExemptionQuerier + fakeExemptionUpdater stubs. A stale draft of
// this test referenced an out-of-date ConfigTemplate-based API and was
// removed during the post-merge cleanup.

// DriftDetector — handleDriftedThing Redis INCR error

// TestDriftDetector_Run_RedisIncrError asserts that when the Redis INCR call
// fails the error is warned per-thing and does NOT propagate through Run.
func TestDriftDetector_Run_RedisIncrError(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()

	// Kill miniredis so INCR will fail.
	mr.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}).
			AddRow("thing-rerr", "agent", "drift", int64(2), int64(1), (*time.Time)(nil)))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, nil, discardLogger())

	// Run must not propagate the Redis error — warned per-thing.
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error, want nil (Redis error must be warned, not propagated): %v", err)
	}
}
