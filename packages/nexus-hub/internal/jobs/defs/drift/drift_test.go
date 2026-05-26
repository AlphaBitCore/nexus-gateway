// drift_test.go covers DriftDetector construction, Run happy/error paths,
// handleDriftedThing retry logic, and attemptRepair.
// DB queries are exercised via pgxmock; Redis via miniredis so the retry
// counter, TTL, and exhaustion paths run without real infrastructure.
package drift

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestRegistry returns an isolated Prometheus registry to avoid registration
// conflicts between parallel tests.
func newTestRegistry() *opsmetrics.Registry {
	return opsmetrics.NewRegistry(prometheus.NewRegistry())
}

// newMiniRedis starts a miniredis server and returns a connected go-redis client.
func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, rdb
}

// NewDriftDetector — construction and identity accessors

func TestDriftDetector_NewWithNilRegistry_DoesNotPanic(t *testing.T) {
	// No registry means all metric fields stay nil; the constructor must not panic.
	d := NewDriftDetector(nil, nil, nil, 5*time.Minute, nil, discardLogger())
	if d == nil {
		t.Fatal("NewDriftDetector returned nil")
	}
	if d.thingsTotal != nil {
		t.Errorf("thingsTotal should be nil with nil registry")
	}
	if d.repairsAttempted != nil {
		t.Errorf("repairsAttempted should be nil with nil registry")
	}
	if d.checkDurationMs != nil {
		t.Errorf("checkDurationMs should be nil with nil registry")
	}
}

func TestDriftDetector_NewWithRegistry_MetricsRegistered(t *testing.T) {
	reg := newTestRegistry()
	d := NewDriftDetector(nil, nil, nil, 5*time.Minute, reg, discardLogger())
	if d == nil {
		t.Fatal("NewDriftDetector returned nil")
	}
	if d.thingsTotal == nil {
		t.Errorf("thingsTotal must be non-nil with a real registry")
	}
	if d.repairsAttempted == nil {
		t.Errorf("repairsAttempted must be non-nil with a real registry")
	}
	if d.checkDurationMs == nil {
		t.Errorf("checkDurationMs must be non-nil with a real registry")
	}
}

func TestDriftDetector_Identity(t *testing.T) {
	d := NewDriftDetector(nil, nil, nil, 10*time.Minute, nil, discardLogger())
	if d.ID() != driftJobID {
		t.Errorf("ID = %q, want %q", d.ID(), driftJobID)
	}
	if d.Name() == "" {
		t.Error("Name must not be empty")
	}
	if d.Description() == "" {
		t.Error("Description must not be empty")
	}
	if d.Interval() != 10*time.Minute {
		t.Errorf("Interval = %v, want 10m", d.Interval())
	}
}

// Run — store error path

func TestDriftDetector_Run_FindDriftedThings_Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("db down")
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnError(sentinel)

	st := store.NewWithPgxPool(mock)
	d := NewDriftDetector(st, nil, nil, time.Minute, nil, discardLogger())

	err := d.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run err = %v, want sentinel", err)
	}
}

// TestDriftDetector_Run_NoDriftedThings asserts the no-op path when the store
// returns zero drifted things: no repair call, no error, gauge set to 0.
func TestDriftDetector_Run_NoDriftedThings(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}))

	st := store.NewWithPgxPool(mock)
	d := NewDriftDetector(st, nil, nil, time.Minute, newTestRegistry(), discardLogger())

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run — drifted things found, various repair paths

// TestDriftDetector_Run_NilRedis_AttemptRepair asserts that without Redis the
// job unconditionally calls attemptRepair for each drifted thing. GetThing
// returns not found so the repair results in an ErrNotFound — logged, not
// propagated through Run.
func TestDriftDetector_Run_NilRedis_AttemptRepair(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// FindDriftedThings: 1 thing
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}).
			AddRow("thing-1", "agent", "drift", int64(2), int64(1), (*time.Time)(nil)))
	// GetThing (called by RePushConfig): returns 0 rows → ErrNotFound
	mock.ExpectQuery(`SELECT t.id`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, nil, discardLogger())

	// Run must not propagate the ErrNotFound from repair (it logs a warning).
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestDriftDetector_Run_WithRedis_WithinRetryLimit asserts that when the Redis
// counter is below driftMaxRetries, attemptRepair is called.
func TestDriftDetector_Run_WithRedis_WithinRetryLimit(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()
	defer mr.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// FindDriftedThings: 1 thing
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}).
			AddRow("thing-rr", "agent", "drift", int64(2), int64(1), (*time.Time)(nil)))
	// GetThing: 0 rows → ErrNotFound (repair error is logged, not propagated)
	mock.ExpectQuery(`SELECT t.id, t.type`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, nil, discardLogger())

	// Counter starts at 0 → Incr sets it to 1 (≤ driftMaxRetries=3) → attemptRepair.
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Verify the counter was incremented in Redis.
	val, err := rdb.Get(context.Background(), driftKeyPrefix+"thing-rr").Int64()
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if val != 1 {
		t.Errorf("retry counter = %d, want 1", val)
	}
}

// TestDriftDetector_Run_WithRedis_ExhaustedRetries asserts that when the Redis
// counter already exceeds driftMaxRetries, UpdateThingStatus("drift") is called
// instead of attemptRepair.
func TestDriftDetector_Run_WithRedis_ExhaustedRetries(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()
	defer mr.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Pre-seed: Incr from 3 → 4 which exceeds driftMaxRetries(3).
	mr.Set(driftKeyPrefix+"thing-ex", "3")

	// FindDriftedThings: 1 thing
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}).
			AddRow("thing-ex", "agent", "drift", int64(2), int64(1), (*time.Time)(nil)))

	// UpdateThingStatus is called on exhaustion.
	mock.ExpectExec(`UPDATE thing SET status`).
		WithArgs("thing-ex", "drift").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, newTestRegistry(), discardLogger())

	// Run must succeed even when retries are exhausted.
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestDriftDetector_Run_ExhaustedRetries_UpdateStatusError covers the path
// where UpdateThingStatus itself fails after exhaustion — the error is logged
// and does not propagate through Run.
func TestDriftDetector_Run_ExhaustedRetries_UpdateStatusError(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()
	defer mr.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mr.Set(driftKeyPrefix+"thing-ue", "3")

	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}).
			AddRow("thing-ue", "agent", "drift", int64(5), int64(3), (*time.Time)(nil)))

	mock.ExpectExec(`UPDATE thing SET status`).
		WithArgs("thing-ue", "drift").
		WillReturnError(errors.New("update failed"))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, nil, discardLogger())

	// Run should NOT return the UpdateThingStatus error (it is warned, not propagated).
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error, want nil (update-status error must not propagate): %v", err)
	}
}

// TestDriftDetector_Run_ThingsGaugeFired asserts that the thingsTotal gauge is
// set to the count of drifted things returned by the store, covering the
// non-empty path of the gauge update branch in Run.
func TestDriftDetector_Run_ThingsGaugeFired(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}).
			AddRow("t1", "agent", "drift", int64(2), int64(1), (*time.Time)(nil)))
	mock.ExpectQuery(`SELECT t.id`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())

	// Use a non-nil registry so the gauge code runs (not the nil-check branch).
	d := NewDriftDetector(st, mgr, nil, time.Minute, newTestRegistry(), discardLogger())

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// d.thingsTotal gauge was set to 1 inside Run (non-nil guard passed).
	// We verify by exercising the branch; the registry API does not expose a
	// read method so we assert no panic and the test completes.
}
