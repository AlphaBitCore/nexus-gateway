package manager

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// version_lock_test.go covers the F-0109 fix: the per-type advisory lock that
// UpdateConfig / SetOverride / ClearOverride take as the FIRST statement of
// their transaction so concurrent same-type desired_ver allocations cannot
// collide.
//
// Two layers:
//   - pgxmock tests (always run) assert the lock is taken first and that a lock
//     failure rolls back with a clear error — covering the new branch in each
//     of the three flows.
//   - TestManager_UpdateConfig_DistinctVersionsUnderConcurrency (real Postgres,
//     skipped without TEST_DATABASE_URL) proves the actual outcome: two
//     concurrent same-type updates receive distinct, consecutive, strictly
//     increasing versions and both keys land in thing.desired. pgxmock cannot
//     model Postgres' lock serialization + EvalPlanQual, so the distinct-version
//     property is verified against a live database.

// TestManager_UpdateConfig_AcquireLockErr exercises the new Step 0 branch: when
// the advisory lock cannot be taken, UpdateConfig rolls back and returns a
// wrapped error rather than proceeding to allocate a version unsynchronized.
func TestManager_UpdateConfig_AcquireLockErr(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	mock.ExpectBegin()
	// The lock is the first in-tx statement — before the template upsert.
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
		WithArgs("agent").
		WillReturnError(errors.New("lock timeout"))
	mock.ExpectRollback()

	_, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "h", State: map[string]any{"x": 1}, ActorID: "actor",
	})
	if err == nil || !strings.Contains(err.Error(), "acquire config version lock") {
		t.Errorf("err = %v, want acquire-config-version-lock wrap", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestManager_SetOverride_AcquireLockErr covers the same Step 0 branch on the
// single-Thing override write path.
func TestManager_SetOverride_AcquireLockErr(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":false}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
		WithArgs("agent").
		WillReturnError(errors.New("lock timeout"))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks", State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "acquire config version lock") {
		t.Errorf("err = %v, want acquire-config-version-lock wrap", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestManager_ClearOverride_AcquireLockErr covers the Step 0 branch on the
// override clear path.
func TestManager_ClearOverride_AcquireLockErr(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(4),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
		WithArgs("agent").
		WillReturnError(errors.New("lock timeout"))
	mock.ExpectRollback()

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "bob")
	if err == nil || !strings.Contains(err.Error(), "acquire config version lock") {
		t.Errorf("err = %v, want acquire-config-version-lock wrap", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestManager_UpdateConfig_AdvisoryLockOrderedFirst pins the in-tx statement
// ORDER: the advisory lock fires immediately after Begin and strictly before
// the version-allocating CTE (`WITH next AS`). pgxmock enforces ordered
// expectations, so a regression that moved the lock after the allocation — or
// dropped it — would fail this test.
func TestManager_UpdateConfig_AdvisoryLockOrderedFirst(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{broadcastCount: 1}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
		WithArgs("agent").
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "hooks", pgxmock.AnyArg(), "actor-1").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(5)))
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "hooks", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).AddRow("t-1", int64(6)))
	// F-0110: agent type emits no pg_notify in UpdateDesiredForType. Dropping
	// the stale ExpectExec(pg_notify) keeps ExpectationsWereMet strict — if the
	// gate regressed and a notify fired, the unmatched Exec would fail here.
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("agent", "hooks", "update", "actor-1", "", pgxmock.AnyArg(), int64(5), "", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	resp, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "hooks", State: map[string]any{"e": true},
		Action: "update", ActorID: "actor-1",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.ThingDesiredVer != 6 {
		t.Errorf("ThingDesiredVer = %d, want 6", resp.ThingDesiredVer)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (lock not ordered first?): %v", err)
	}
}

// TestManager_UpdateConfig_DistinctVersionsUnderConcurrency is the faithful
// F-0109 regression test. It fires two concurrent UpdateConfig calls for the
// SAME Thing type but DIFFERENT keys against a live Postgres and asserts:
//
//   - the two calls receive DISTINCT, consecutive versions (6 and 7), never a
//     collision (6 and 6) — the bug the advisory lock fixes;
//   - both keys land in every Thing's desired (the jsonb_set merge survives);
//   - the final desired_ver (7) is strictly greater than the reported_ver (5),
//     so the second config_changed frame is applicable on the client rather
//     than being short-circuited as a stale no-op.
//
// Uses a throwaway Thing type so MAX(desired_ver) for the type counts only the
// rows this test seeds. Skipped without TEST_DATABASE_URL (matches the package
// integration-test convention).
func TestManager_UpdateConfig_DistinctVersionsUnderConcurrency(t *testing.T) {
	pool := overrideMgrTestPool(t) // t.Skip when TEST_DATABASE_URL is unset
	defer pool.Close()
	ctx := context.Background()

	const seededVer = 5
	prefix := overrideMgrTestPrefix + "f0109-"
	thingType := prefix + "agent"
	keyA := prefix + "alpha"
	keyB := prefix + "bravo"

	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE type = $1`, thingType)
		_, _ = pool.Exec(ctx, `DELETE FROM thing_config_template WHERE type = $1`, thingType)
		_, _ = pool.Exec(ctx, `DELETE FROM config_change_event WHERE thing_type = $1`, thingType)
	}
	cleanup()
	defer cleanup()

	thingIDs := []string{prefix + "n1", prefix + "n2"}
	for _, id := range thingIDs {
		if _, err := pool.Exec(ctx, `
			INSERT INTO thing (id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
			                   status, metadata, desired, reported, desired_ver, reported_ver,
			                   last_seen_at, enrolled_at, updated_at)
			VALUES ($1, $2, $1, '1.0.0', '127.0.0.1', 'tester', 'bearer', 'http',
			        'online', '{}', '{}', '{}', $3, $3, NOW(), NOW(), NOW())
		`, id, thingType, int64(seededVer)); err != nil {
			t.Fatalf("seed thing %s: %v", id, err)
		}
	}

	mgr, _ := newTestManager(t, pool)

	type result struct {
		ver int64
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, key := range []string{keyA, keyB} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			resp, err := mgr.UpdateConfig(ctx, UpdateConfigRequest{
				ThingType: thingType, ConfigKey: k,
				State: map[string]any{"k": k}, Action: "update", ActorID: "tester",
			})
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{ver: resp.ThingDesiredVer}
		}(key)
	}
	wg.Wait()
	close(results)

	var vers []int64
	for r := range results {
		if r.err != nil {
			t.Fatalf("UpdateConfig: %v", r.err)
		}
		vers = append(vers, r.ver)
	}
	sort.Slice(vers, func(i, j int) bool { return vers[i] < vers[j] })
	if len(vers) != 2 || vers[0] != seededVer+1 || vers[1] != seededVer+2 {
		t.Fatalf("allocated desired_ver = %v, want [%d %d] (distinct consecutive, no collision)",
			vers, seededVer+1, seededVer+2)
	}

	rows, err := pool.Query(ctx, `SELECT desired, desired_ver FROM thing WHERE type = $1 ORDER BY id`, thingType)
	if err != nil {
		t.Fatalf("query things: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var desiredRaw []byte
		var dv int64
		if err := rows.Scan(&desiredRaw, &dv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var desired map[string]any
		if err := json.Unmarshal(desiredRaw, &desired); err != nil {
			t.Fatalf("unmarshal desired: %v", err)
		}
		if _, ok := desired[keyA]; !ok {
			t.Errorf("thing desired missing key %q: %v", keyA, desired)
		}
		if _, ok := desired[keyB]; !ok {
			t.Errorf("thing desired missing key %q: %v", keyB, desired)
		}
		if dv != seededVer+2 {
			t.Errorf("final desired_ver = %d, want %d (the higher of the two)", dv, seededVer+2)
		}
		if dv <= seededVer {
			t.Errorf("final desired_ver %d must exceed reported_ver %d so the push is applicable", dv, seededVer)
		}
		n++
	}
	if n != len(thingIDs) {
		t.Errorf("expected %d things, got %d", len(thingIDs), n)
	}
}
