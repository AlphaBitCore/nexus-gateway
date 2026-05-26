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

var driftCols = []string{
	"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at",
}

// TestFindDriftedThings covers happy + query err wrap.
func TestFindDriftedThings(t *testing.T) {
	t.Run("happy multi-row", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM thing\s+WHERE status IN \('online', 'drift'\)\s+AND desired_ver != reported_ver`).
			WillReturnRows(pgxmock.NewRows(driftCols).
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

// TestListDriftedThings covers happy + query err wrap (uses
// status='drift' OR online+mismatch — distinct SQL from
// FindDriftedThings; verify the same row shape threads through).
func TestListDriftedThings(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM thing\s+WHERE status = 'drift'\s+OR \(status = 'online' AND desired_ver != reported_ver\)`).
			WillReturnRows(pgxmock.NewRows(driftCols).
				AddRow("dev-1", "agent", "drift", int64(1), int64(0), &now),
			)
		store := New(mock)
		got, err := store.ListDriftedThings(context.Background())
		if err != nil || len(got) != 1 || got[0].ID != "dev-1" {
			t.Errorf("got=%+v err=%v", got, err)
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
}

// TestUpdateDesiredForType covers happy fan-out + per-Thing
// pg_notify emit + marshal err + query err + scan err.
func TestUpdateDesiredForType(t *testing.T) {
	t.Run("happy 2-row fan-out emits 2 notifies", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH next AS.*UPDATE thing AS t`).
			WithArgs("ai-gateway", "routing_rules", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
				AddRow("gw-1", int64(9)).
				AddRow("gw-2", int64(9)),
			)
		// Two pg_notify Execs — one per row.
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "gw-1").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "gw-2").
			WillReturnResult(pgxmock.NewResult("SELECT", 0))
		mock.ExpectCommit()

		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		n, ver, err := store.UpdateDesiredForType(context.Background(), tx,
			"ai-gateway", "routing_rules", map[string]any{"v": 1}, 0)
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
	})
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
	t.Run("notify err propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH next AS`).
			WithArgs("t", "k", pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
				AddRow("th-1", int64(2)))
		want := errors.New("notify failed")
		mock.ExpectExec(`SELECT pg_notify`).
			WithArgs(ConfigChangedChannel, "th-1").
			WillReturnError(want)
		mock.ExpectRollback()

		tx, _ := mock.Begin(context.Background())
		store := New(mock)
		_, _, err := store.UpdateDesiredForType(context.Background(), tx,
			"t", "k", map[string]any{}, 0)
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
