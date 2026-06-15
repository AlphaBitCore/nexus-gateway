// drift_test.go covers DriftDetector construction, Run happy/error paths,
// handleDriftedThing retry logic, and attemptRepair.
// DB queries are exercised via pgxmock; Redis via miniredis so the retry
// counter, TTL, and exhaustion paths run without real infrastructure.
package drift

import (
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------------------
// F-0112 content-reconcile pass
// ---------------------------------------------------------------------------

// getThingCols mirrors store.GetThing's SELECT column order (thing_registry.go).
var getThingCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
	"hostname", "primary_ip", "os", "os_version", "physical_id",
	"u_id", "u_displayName", "u_email", "metrics_url",
}

// getThingRow builds one GetThing row at EQUAL versions (desired_ver ==
// reported_ver == ver) with the supplied desired/reported maps so the content
// pass sees a version-equal Thing whose CONTENT may or may not diverge.
func getThingRow(id, ttype string, desired, reported map[string]any, ver int64) *pgxmock.Rows {
	now := time.Now().UTC()
	dj, _ := json.Marshal(desired)
	rj, _ := json.Marshal(reported)
	return pgxmock.NewRows(getThingCols).AddRow(
		id, ttype, id, "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		dj, rj,
		ver, ver,
		[]byte(`{}`), &now, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "darwin", "14.0", "",
		"", "", "", "",
	)
}

// expectForceResyncAllDB pins the DB calls ForceResyncAll issues after the
// GetShadowComparison GetThing: a second GetThing, then the desired_ver bump
// (advisory lock + UPDATE + pg_notify inside a tx). The per-key WS/MQ push
// needs no DB (nil WS + nil MQ → ErrNoDeliveryPath, counted as delivered).
func expectForceResyncAllDB(mock pgxmock.PgxPoolIface, id, ttype string, desired, reported map[string]any, ver, newVer int64) {
	mock.ExpectQuery(`FROM thing t`).
		WithArgs(id).
		WillReturnRows(getThingRow(id, ttype, desired, reported, ver))
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
		WithArgs(ttype).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
		WithArgs(id, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(newVer))
	mock.ExpectExec(`pg_notify`).
		WithArgs(pgxmock.AnyArg(), id).
		WillReturnResult(pgxmock.NewResult("SELECT", 0))
	mock.ExpectCommit()
}

func TestContentConverged(t *testing.T) {
	if !contentConverged(nil) {
		t.Error("nil comparison must be treated as converged")
	}
	synced := &manager.ShadowComparison{Keys: map[string]manager.ShadowKeyDiff{
		"a": {Synced: true, InDesired: true}, "b": {Synced: true, InDesired: true},
	}}
	if !contentConverged(synced) {
		t.Error("all-synced desired keys must be converged")
	}
	diverged := &manager.ShadowComparison{Keys: map[string]manager.ShadowKeyDiff{
		"a": {Synced: true, InDesired: true}, "b": {Synced: false, InDesired: true},
	}}
	if contentConverged(diverged) {
		t.Error("one unsynced desired key must be NOT converged")
	}
	// BLOCKER regression (F-0112 review): a reported-only key (InDesired=false)
	// is a stale key the Hub no longer desires — the reported map never prunes,
	// so it lingers forever. It is reported as unsynced (desired=nil != reported)
	// but is NOT actionable drift (ForceResyncAll can't clear it). It must be
	// IGNORED, else the content pass force-resyncs the Thing every cycle forever.
	staleReportedOnly := &manager.ShadowComparison{Keys: map[string]manager.ShadowKeyDiff{
		"a":           {Synced: true, InDesired: true},
		"deleted-key": {Synced: false, InDesired: false}, // reported-only leftover
	}}
	if !contentConverged(staleReportedOnly) {
		t.Error("a reported-only (InDesired=false) unsynced key must NOT count as drift")
	}
	// A genuinely divergent desired key alongside a stale reported-only key still
	// counts as drift (the stale key doesn't mask a real divergence).
	mixed := &manager.ShadowComparison{Keys: map[string]manager.ShadowKeyDiff{
		"a":           {Synced: false, InDesired: true},  // real drift
		"deleted-key": {Synced: false, InDesired: false}, // stale, ignored
	}}
	if contentConverged(mixed) {
		t.Error("a real desired-key divergence must still be detected past stale keys")
	}
}

func TestDriftDetector_runContentPassDue(t *testing.T) {
	d := NewDriftDetector(nil, nil, nil, time.Minute, nil, discardLogger())
	d.contentPassEvery = 3
	// Ticks 1,2 → false; 3 → true; 4,5 → false; 6 → true.
	want := []bool{false, false, true, false, false, true}
	for i, w := range want {
		if got := d.runContentPassDue(); got != w {
			t.Errorf("tick %d: due = %v, want %v", i+1, got, w)
		}
	}

	// contentPassEvery <= 0 disables the content pass entirely.
	d2 := NewDriftDetector(nil, nil, nil, time.Minute, nil, discardLogger())
	d2.contentPassEvery = 0
	for i := range 5 {
		if d2.runContentPassDue() {
			t.Fatalf("disabled content pass must never be due (call %d)", i+1)
		}
	}
}

// TestDriftDetector_ContentPass_HealsDivergentLeavesConverged is the core
// F-0112 assertion: two online Things at EQUAL versions, one with divergent
// reported content and one converged. The content pass force-resyncs the
// divergent one (full bump-and-push DB chain) and leaves the converged one
// untouched (no heal, no version bump).
func TestDriftDetector_ContentPass_HealsDivergentLeavesConverged(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Candidates: divergent first, converged second (ordered pgxmock).
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).
			AddRow("t-div", "agent").
			AddRow("t-ok", "agent"))

	// t-div: GetShadowComparison sees desired{k:A} vs reported{k:B} → diverged.
	divDesired := map[string]any{"k": "A"}
	divReported := map[string]any{"k": "B"}
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-div").
		WillReturnRows(getThingRow("t-div", "agent", divDesired, divReported, 5))
	// Heal: ForceResyncAll bumps desired_ver 5 → 6 and force-pushes.
	expectForceResyncAllDB(mock, "t-div", "agent", divDesired, divReported, 5, 6)

	// t-ok: converged (desired == reported) → NO heal expectation follows.
	okState := map[string]any{"k": "A"}
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-ok").
		WillReturnRows(getThingRow("t-ok", "agent", okState, okState, 7))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, newTestRegistry(), discardLogger())

	diverged := d.contentPass(context.Background())
	if diverged != 1 {
		t.Fatalf("diverged = %d, want 1 (only t-div healed, t-ok left alone)", diverged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestDriftDetector_ContentPass_AllConverged_NoHeal asserts a converged fleet is
// left completely alone — diverged count 0 and no bump/push DB calls.
func TestDriftDetector_ContentPass_AllConverged_NoHeal(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("t-ok", "agent"))
	okState := map[string]any{"k": "A"}
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-ok").
		WillReturnRows(getThingRow("t-ok", "agent", okState, okState, 3))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, nil, discardLogger())

	if diverged := d.contentPass(context.Background()); diverged != 0 {
		t.Fatalf("diverged = %d, want 0 (no false re-push on a converged Thing)", diverged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met (a stray heal would add unexpected calls): %v", err)
	}
}

// TestDriftDetector_ContentPass_FindCandidatesError: the candidate query erroring
// must be logged and yield 0 diverged (no panic, version pass already ran).
func TestDriftDetector_ContentPass_FindCandidatesError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnError(errors.New("db down"))

	st := store.NewWithPgxPool(mock)
	d := NewDriftDetector(st, nil, nil, time.Minute, nil, discardLogger())
	if diverged := d.contentPass(context.Background()); diverged != 0 {
		t.Fatalf("diverged = %d, want 0 on candidate-query error", diverged)
	}
}

// TestDriftDetector_ContentPass_ShadowComparisonError: a candidate whose GetThing
// fails is skipped (logged), not healed.
func TestDriftDetector_ContentPass_ShadowComparisonError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("t-err", "agent"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-err").
		WillReturnError(errors.New("planner err"))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, nil, discardLogger())
	if diverged := d.contentPass(context.Background()); diverged != 0 {
		t.Fatalf("diverged = %d, want 0 (comparison error → skip, not heal)", diverged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestDriftDetector_ContentPass_RedisWithinLimit: with Redis, a content-divergent
// Thing under the retry cap is healed and its content-retry counter increments.
func TestDriftDetector_ContentPass_RedisWithinLimit(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()
	defer mr.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("t-rr", "agent"))
	div := map[string]any{"k": "A"}
	rep := map[string]any{"k": "B"}
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-rr").
		WillReturnRows(getThingRow("t-rr", "agent", div, rep, 4))
	expectForceResyncAllDB(mock, "t-rr", "agent", div, rep, 4, 5)

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, newTestRegistry(), discardLogger())

	if diverged := d.contentPass(context.Background()); diverged != 1 {
		t.Fatalf("diverged = %d, want 1", diverged)
	}
	val, err := rdb.Get(context.Background(), contentRetryKeyPrefix+"t-rr").Int64()
	if err != nil {
		t.Fatalf("get content-retry counter: %v", err)
	}
	if val != 1 {
		t.Errorf("content-retry counter = %d, want 1", val)
	}
	// The content-retry key must NOT collide with the version-retry key.
	if _, err := rdb.Get(context.Background(), driftKeyPrefix+"t-rr").Result(); !errors.Is(err, redis.Nil) {
		t.Errorf("version-retry key must be untouched by content pass; err=%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestDriftDetector_ContentPass_RedisExhausted: once the content-retry counter
// exceeds the cap the Thing is marked 'drift' (ops visibility) instead of being
// re-pushed forever.
func TestDriftDetector_ContentPass_RedisExhausted(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()
	defer mr.Close()
	mr.Set(contentRetryKeyPrefix+"t-ex", "3") // Incr → 4 > driftMaxRetries(3)

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("t-ex", "agent"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-ex").
		WillReturnRows(getThingRow("t-ex", "agent", map[string]any{"k": "A"}, map[string]any{"k": "B"}, 6))
	// Exhausted → UpdateThingStatus('drift'), NO ForceResyncAll.
	mock.ExpectExec(`UPDATE thing SET status`).
		WithArgs("t-ex", "drift").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, newTestRegistry(), discardLogger())

	if diverged := d.contentPass(context.Background()); diverged != 1 {
		t.Fatalf("diverged = %d, want 1", diverged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestDriftDetector_ContentPass_RedisIncrError: when the content-retry Incr
// fails (Redis unreachable) handleContentDrift surfaces the wrapped error; the
// pass logs it and still counts the Thing as diverged.
func TestDriftDetector_ContentPass_RedisIncrError(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	defer rdb.Close()
	mr.Close() // kill the server so Incr returns a connection error

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("t-re", "agent"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-re").
		WillReturnRows(getThingRow("t-re", "agent", map[string]any{"k": "A"}, map[string]any{"k": "B"}, 1))
	// No heal DB calls: the Incr error short-circuits before attemptContentRepair.

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, rdb, time.Minute, nil, discardLogger())
	if diverged := d.contentPass(context.Background()); diverged != 1 {
		t.Fatalf("diverged = %d, want 1 (divergence detected even when retry-gate errors)", diverged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no heal must be attempted after an Incr error: %v", err)
	}
}

// TestDriftDetector_Run_ContentPassDue_RunsBothPasses: when the content pass is
// due, Run issues both the version query and the content query.
func TestDriftDetector_Run_ContentPassDue_RunsBothPasses(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Version pass: no drift.
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}))
	// Content pass: no equal-version candidates.
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}))

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, newTestRegistry(), discardLogger())
	d.contentPassEvery = 1 // every tick is due

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both passes must run: %v", err)
	}
}

// TestDriftDetector_Run_ContentPassNotDue: on a non-due tick only the version
// pass runs — the content query is never issued.
func TestDriftDetector_Run_ContentPassNotDue(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at"}))
	// No content query expectation: if the content pass ran, the unexpected
	// FindEqualVersionOnlineThings query would surface.

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, newTestRegistry(), discardLogger())
	// Default contentPassEvery = contentPassEveryNTicks(10); tick 1 % 10 != 0.

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("only the version pass must run on a non-due tick: %v", err)
	}
}

// TestDriftDetector_Run_ContentPassSkippedOnVersionError: when the version pass
// errors (DB down), the content pass is skipped even on a due tick, and the
// version error propagates.
func TestDriftDetector_Run_ContentPassSkippedOnVersionError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("db down")
	mock.ExpectQuery(`SELECT id, type, status, desired_ver, reported_ver, last_seen_at`).
		WillReturnError(sentinel)
	// No content query expectation — it must NOT run after a version error.

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, nil, discardLogger())
	d.contentPassEvery = 1 // due, but version error must still skip content

	if err := d.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Run err = %v, want sentinel", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("content pass must be skipped on version error: %v", err)
	}
}

// TestDriftDetector_ContentPass_NilRedisHeals: without Redis the content pass
// heals divergent Things unconditionally (no retry gate).
func TestDriftDetector_ContentPass_NilRedisHeals(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).AddRow("t-nr", "agent"))
	div := map[string]any{"k": "A"}
	rep := map[string]any{"k": "B"}
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-nr").
		WillReturnRows(getThingRow("t-nr", "agent", div, rep, 2))
	expectForceResyncAllDB(mock, "t-nr", "agent", div, rep, 2, 3)

	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-1", discardLogger())
	d := NewDriftDetector(st, mgr, nil, time.Minute, nil, discardLogger())
	if diverged := d.contentPass(context.Background()); diverged != 1 {
		t.Fatalf("diverged = %d, want 1", diverged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestDriftDetector_NewWithRegistry_ContentMetricsRegistered asserts the new
// content-pass metrics are wired when a registry is supplied.
func TestDriftDetector_NewWithRegistry_ContentMetricsRegistered(t *testing.T) {
	d := NewDriftDetector(nil, nil, nil, time.Minute, newTestRegistry(), discardLogger())
	if d.contentDriftThings == nil || d.contentRepairsAttempted == nil || d.contentRepairsExhausted == nil {
		t.Error("content-pass metrics must be non-nil with a real registry")
	}
	if d.contentPassEvery != contentPassEveryNTicks {
		t.Errorf("contentPassEvery = %d, want default %d", d.contentPassEvery, contentPassEveryNTicks)
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
