package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// findDriftCols mirrors FindDriftedThings' 6-column SELECT (no out_of_sync_keys).
var findDriftCols = []string{
	"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at",
}

// listDriftCols mirrors ListDriftedThings' 7-column SELECT (includes out_of_sync_keys).
var listDriftCols = []string{
	"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at", "out_of_sync_keys",
}

// TestFindDriftedThings covers happy + query err wrap.
func TestFindDriftedThings(t *testing.T) {
	t.Run("happy multi-row", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM thing\s+WHERE status IN \('online', 'drift'\)\s+AND desired_ver != reported_ver`).
			WillReturnRows(pgxmock.NewRows(findDriftCols).
				AddRow("dev-1", "agent", "drift", int64(3), int64(2), &now).
				AddRow("dev-2", "ai-gateway", "online", int64(5), int64(4), &now),
			)
		store := New(mock)
		got, err := store.FindDriftedThings(context.Background())
		if err != nil {
			t.Fatalf("FindDriftedThings: %v", err)
		}
		if len(got) != 2 || got[0].ID != "dev-1" || got[1].Status != "online" {
			t.Errorf("drifted: %+v", got)
		}
	})
	t.Run("query err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM thing\s+WHERE status IN \('online', 'drift'\)`).
			WillReturnError(want)
		store := New(mock)
		_, err := store.FindDriftedThings(context.Background())
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "find drifted") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

// TestFindEqualVersionOnlineThings covers the content-pass candidate
// query: online Things at desired_ver == reported_ver, id/type only.
func TestFindEqualVersionOnlineThings(t *testing.T) {
	t.Run("happy multi-row", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'\s+AND desired_ver = reported_ver`).
			WillReturnRows(pgxmock.NewRows([]string{"id", "type"}).
				AddRow("dev-1", "agent").
				AddRow("dev-2", "ai-gateway"),
			)
		store := New(mock)
		got, err := store.FindEqualVersionOnlineThings(context.Background())
		if err != nil {
			t.Fatalf("FindEqualVersionOnlineThings: %v", err)
		}
		if len(got) != 2 || got[0].ID != "dev-1" || got[0].Type != "agent" || got[1].ID != "dev-2" {
			t.Errorf("candidates: %+v", got)
		}
	})
	t.Run("empty result", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
			WillReturnRows(pgxmock.NewRows([]string{"id", "type"}))
		store := New(mock)
		got, err := store.FindEqualVersionOnlineThings(context.Background())
		if err != nil {
			t.Fatalf("FindEqualVersionOnlineThings: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("want empty, got %+v", got)
		}
	})
	t.Run("query err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
			WillReturnError(want)
		store := New(mock)
		_, err := store.FindEqualVersionOnlineThings(context.Background())
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "find equal-version online things") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("scan err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// One column instead of two → Scan(&id, &type) fails.
		mock.ExpectQuery(`SELECT id, type\s+FROM thing\s+WHERE status = 'online'`).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("dev-1"))
		store := New(mock)
		_, err := store.FindEqualVersionOnlineThings(context.Background())
		if err == nil || !strings.Contains(err.Error(), "scan content candidate") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})
}

// TestListDriftedThings covers happy + query err wrap (uses
// status='drift' OR online+mismatch — distinct SQL from
// FindDriftedThings; verify the same row shape threads through).
func TestListDriftedThings(t *testing.T) {
	t.Run("happy: out_of_sync_keys populated", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		// Simulate the DB returning two diverging config keys.
		keys := []string{"hooks", "killswitch"}
		mock.ExpectQuery(`FROM thing\s+WHERE status = 'drift'\s+OR \(status = 'online' AND desired_ver != reported_ver\)`).
			WillReturnRows(pgxmock.NewRows(listDriftCols).
				AddRow("dev-1", "agent", "drift", int64(1), int64(0), &now, keys),
			)
		store := New(mock)
		got, err := store.ListDriftedThings(context.Background())
		if err != nil {
			t.Fatalf("ListDriftedThings: %v", err)
		}
		if len(got) != 1 || got[0].ID != "dev-1" {
			t.Fatalf("got=%+v", got)
		}
		if len(got[0].OutOfSyncKeys) != 2 || got[0].OutOfSyncKeys[0] != "hooks" || got[0].OutOfSyncKeys[1] != "killswitch" {
			t.Errorf("OutOfSyncKeys = %v, want [hooks killswitch]", got[0].OutOfSyncKeys)
		}
	})
	t.Run("happy: out_of_sync_keys nil from DB normalises to empty slice", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		// When desired == reported for all keys the subquery returns an empty array;
		// pgxmock represents this as nil for []string.
		mock.ExpectQuery(`FROM thing\s+WHERE status = 'drift'\s+OR \(status = 'online' AND desired_ver != reported_ver\)`).
			WillReturnRows(pgxmock.NewRows(listDriftCols).
				AddRow("dev-2", "agent", "drift", int64(3), int64(2), &now, []string(nil)),
			)
		store := New(mock)
		got, err := store.ListDriftedThings(context.Background())
		if err != nil {
			t.Fatalf("ListDriftedThings: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 row; got=%+v", got)
		}
		if got[0].OutOfSyncKeys == nil {
			t.Error("OutOfSyncKeys must not be nil (JSON must be [] not null)")
		}
		if len(got[0].OutOfSyncKeys) != 0 {
			t.Errorf("OutOfSyncKeys = %v, want empty", got[0].OutOfSyncKeys)
		}
	})
	t.Run("query err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("timeout")
		mock.ExpectQuery(`FROM thing\s+WHERE status = 'drift'`).WillReturnError(want)
		store := New(mock)
		_, err := store.ListDriftedThings(context.Background())
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "list drifted") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

// TestUpdateShadowReport covers all 5 branches: marshal err (reported)
// / marshal err (outcomes) — chan triggers / empty outcomes →
// "{}" normalisation / not found / exec err.
func TestUpdateShadowReport(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(7), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		now := time.Now().UTC()
		ver := int64(7)
		err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"x": "y"}, 7,
			map[string]ReportedKeyOutcome{"x": {AppliedAt: &now, AppliedVersion: &ver}})
		if err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
	t.Run("marshal reported err", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		store := New(mock)
		err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"bad": make(chan int)}, 1, nil)
		if err == nil {
			t.Fatal("expected marshal err")
		}
		if !strings.Contains(err.Error(), "marshal reported") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("nil outcomes normalises to {}", func(t *testing.T) {
		// json.Marshal(nil map) → "null"; the code rewrites to "{}" so
		// downstream readers see an empty map, not a nil-typed jsonb.
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(7), []byte("{}")).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"x": "y"}, 7, nil)
		if err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("missing", pgxmock.AnyArg(), int64(0), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))
		store := New(mock)
		err := store.UpdateShadowReport(context.Background(), "missing",
			map[string]any{}, 0, nil)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(1), pgxmock.AnyArg()).
			WillReturnError(want)
		store := New(mock)
		err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{}, 1, nil)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
	// the UPDATE must carry the monotonic guard so a stale / duplicate /
	// older report cannot regress reported_ver (or the reported content/outcomes
	// it stamps) and manufacture a drift flap. The SQL-text expectations below
	// fail if anyone reverts the guard to the old unconditional
	// `reported = $2, reported_ver = $3` form. The drift-clear arm must key off
	// the effective (post-GREATEST) version, not the raw reported version.
	t.Run("monotonic guard: reported_ver never regresses", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`reported_ver\s*=\s*GREATEST\(reported_ver, \$3\)`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(3), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		if err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"x": "y"}, 3, nil); err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
	t.Run("monotonic guard: content + outcomes only written when not stale", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`reported = CASE WHEN \$3 >= reported_ver THEN COALESCE\(reported, '\{\}'::jsonb\) \|\| \$2::jsonb ELSE reported END`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(3), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		if err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"x": "y"}, 3, nil); err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
	// per-key merge. Both reported and reported_outcomes must fold the
	// incoming keys in with `||` (COALESCE-guarded against a NULL column) rather
	// than full-replacing the column, so a Thing reporting a single changed key
	// (per-key dispatch) cannot wipe sibling keys' reported state. The
	// SQL-text expectation below fails if anyone reverts the merge to the old
	// `reported = $2` full-replace form.
	t.Run("per-key merge: reported folds in with || preserving siblings", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`reported = CASE WHEN \$3 >= reported_ver THEN COALESCE\(reported, '\{\}'::jsonb\) \|\| \$2::jsonb`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(5), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		if err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"killswitch": map[string]any{"engaged": true}}, 5, nil); err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
	t.Run("per-key merge: reported_outcomes folds in with ||", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`reported_outcomes = CASE WHEN \$3 >= reported_ver THEN COALESCE\(reported_outcomes, '\{\}'::jsonb\) \|\| \$4::jsonb`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(5), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		now := time.Now().UTC()
		ver := int64(5)
		if err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"x": "y"}, 5,
			map[string]ReportedKeyOutcome{"x": {AppliedAt: &now, AppliedVersion: &ver}}); err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
	t.Run("monotonic guard: drift clears off the effective version", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`status = CASE WHEN status = 'drift' AND GREATEST\(reported_ver, \$3\) >= desired_ver`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(3), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		store := New(mock)
		if err := store.UpdateShadowReport(context.Background(), "thing-1",
			map[string]any{"x": "y"}, 3, nil); err != nil {
			t.Fatalf("UpdateShadowReport: %v", err)
		}
	})
}

// TestUpdateDesiredForType covers the nexus-hub fan-out (per-Thing pg_notify
// emit) + the gate (non-nexus-hub types emit NO pg_notify) + marshal
// err + query err + scan err + notify err propagation.
func TestUpdateDesiredForType(t *testing.T) {
	t.Run("nexus-hub 2-row fan-out emits 2 notifies", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH next AS.*UPDATE thing AS t`).
			WithArgs("nexus-hub", "routing_rules", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
				AddRow("hub-1", int64(9)).
				AddRow("hub-2", int64(9)),
			)
		// Two pg_notify Execs — one per row, only because thingType=="nexus-hub".
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "hub-1").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "hub-2").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectCommit()

		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		n, ver, err := store.UpdateDesiredForType(context.Background(), tx,
			"nexus-hub", "routing_rules", map[string]any{"v": 1}, 0)
		if err != nil {
			t.Fatalf("UpdateDesiredForType: %v", err)
		}
		if n != 2 {
			t.Errorf("rowsAffected: got %d, want 2", n)
		}
		if ver != 9 {
			t.Errorf("shadowDesiredVer: got %d, want 9", ver)
		}
		_ = tx.Commit(context.Background())
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("nexus-hub must emit exactly 2 pg_notify Execs: %v", err)
		}
	})
	// every non-nexus-hub type (agent, ai-gateway, compliance-proxy,
	// control-plane) must NOT run the pg_notify loop — config is delivered to
	// those Things via the WebSocket broadcast, never via pg_notify. The mock
	// declares only Begin/Query/Commit and NO ExpectExec(pg_notify); pgxmock
	// in ordered mode would error on any unexpected pg_notify Exec, so a clean
	// run proves the loop was skipped for the gated type.
	for _, gatedType := range []string{"agent", "ai-gateway", "compliance-proxy", "control-plane"} {
		t.Run("gated type emits no notify: "+gatedType, func(t *testing.T) {
			mock, _ := pgxmock.NewPool()
			defer mock.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(`WITH next AS.*UPDATE thing AS t`).
				WithArgs(gatedType, "routing_rules", pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
					AddRow("th-1", int64(7)).
					AddRow("th-2", int64(7)),
				)
			mock.ExpectCommit()

			tx, _ := mock.Begin(context.Background())
			store := New(mock)
			n, ver, err := store.UpdateDesiredForType(context.Background(), tx,
				gatedType, "routing_rules", map[string]any{"v": 1}, 0)
			if err != nil {
				t.Fatalf("UpdateDesiredForType(%s): %v", gatedType, err)
			}
			if n != 2 {
				t.Errorf("rowsAffected: got %d, want 2", n)
			}
			if ver != 7 {
				t.Errorf("shadowDesiredVer: got %d, want 7", ver)
			}
			_ = tx.Commit(context.Background())
			// No pg_notify expectation was registered; if the gate failed and
			// the loop ran, the Exec would have no match and surface here.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("%s must emit NO pg_notify (got unexpected Exec): %v", gatedType, err)
			}
		})
	}
	t.Run("marshal state err pre-Query", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, _, err := store.UpdateDesiredForType(context.Background(), tx,
			"t", "k", make(chan int), 0)
		if err == nil {
			t.Fatal("expected marshal err")
		}
		if !strings.Contains(err.Error(), "marshal state") {
			t.Errorf("missing prefix: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("query err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		want := errors.New("planner err")
		mock.ExpectQuery(`WITH next AS`).
			WithArgs("t", "k", pgxmock.AnyArg()).
			WillReturnError(want)
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, _, err := store.UpdateDesiredForType(context.Background(), tx,
			"t", "k", map[string]any{}, 0)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("notify err propagates (nexus-hub path)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH next AS`).
			WithArgs("nexus-hub", "k", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
				AddRow("hub-1", int64(2)))
		want := errors.New("notify failed")
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "hub-1").
			WillReturnError(want)
		mock.ExpectRollback()

		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, _, err := store.UpdateDesiredForType(context.Background(), tx,
			"nexus-hub", "k", map[string]any{}, 0)
		if !errors.Is(err, want) {
			t.Errorf("must propagate; got: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
}

// TestWriteDesiredAndBumpVer covers happy + marshal err + nil-map
// normalises to "{}" + not-found → ErrNotFound + generic err wrap +
// notify err propagates.
func TestWriteDesiredAndBumpVer(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
			WithArgs("thing-1", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(int64(5)))
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "thing-1").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectCommit()

		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		v, err := store.WriteDesiredAndBumpVer(context.Background(), tx, "thing-1",
			map[string]any{"foo": "bar"})
		if err != nil {
			t.Fatalf("WriteDesiredAndBumpVer: %v", err)
		}
		if v != 5 {
			t.Errorf("ver: got %d, want 5", v)
		}
		_ = tx.Commit(context.Background())
	})
	t.Run("nil merged normalises to {}", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
			WithArgs("thing-1", []byte("{}")). // empty object, NOT "null"
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(int64(6)))
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "thing-1").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectCommit()

		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, err := store.WriteDesiredAndBumpVer(context.Background(), tx, "thing-1", nil)
		if err != nil {
			t.Fatalf("WriteDesiredAndBumpVer: %v", err)
		}
		_ = tx.Commit(context.Background())
	})
	t.Run("marshal err pre-Query", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, err := store.WriteDesiredAndBumpVer(context.Background(), tx, "thing-1",
			map[string]any{"bad": make(chan int)})
		if err == nil {
			t.Fatal("expected marshal err")
		}
		if !strings.Contains(err.Error(), "marshal merged desired") {
			t.Errorf("missing prefix: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`UPDATE thing\s+SET desired`).
			WithArgs("missing", pgxmock.AnyArg()).
			WillReturnError(pgx.ErrNoRows)
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, err := store.WriteDesiredAndBumpVer(context.Background(), tx, "missing", map[string]any{})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("generic err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		want := errors.New("planner err")
		mock.ExpectQuery(`UPDATE thing\s+SET desired`).
			WithArgs("thing-1", pgxmock.AnyArg()).
			WillReturnError(want)
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, err := store.WriteDesiredAndBumpVer(context.Background(), tx, "thing-1", map[string]any{})
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "write desired and bump ver") {
			t.Errorf("missing prefix: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("notify err propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`UPDATE thing\s+SET desired`).
			WithArgs("thing-1", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(int64(7)))
		want := errors.New("notify failed")
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "thing-1").
			WillReturnError(want)
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, err := store.WriteDesiredAndBumpVer(context.Background(), tx, "thing-1", map[string]any{})
		if !errors.Is(err, want) {
			t.Errorf("must propagate notify err; got: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
}

// TestNotifyConfigChanged direct happy + err for completeness —
// shadow_notify.go's only consumers are inside store/, so the helper
// gets covered indirectly already, but a direct test pins the
// channel-name + payload contract.
func TestNotifyConfigChanged(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs("config_changed", "thing-x").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectCommit()

		tx, _ := mock.Begin(context.Background())
		if err := notifyConfigChanged(context.Background(), tx, "thing-x"); err != nil {
			t.Fatalf("notifyConfigChanged: %v", err)
		}
		_ = tx.Commit(context.Background())
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		want := errors.New("connection refused")
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs("config_changed", "thing-x").
			WillReturnError(want)
		mock.ExpectRollback()

		tx, _ := mock.Begin(context.Background())
		err := notifyConfigChanged(context.Background(), tx, "thing-x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "pg_notify config_changed for thing-x") {
			t.Errorf("missing channel+payload prefix: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
}
