package hookstore

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func sp(s string) *string { return &s }

var hcCols = []string{
	"id", "name", "type", "implementationId", "stage", "category", "endpoint", "script",
	"config", "priority", "timeoutMs", "failBehavior", "enabled", "applicableIngress", "createdAt", "updatedAt",
}

func hcRow(id, name string) []any {
	return []any{
		id, name, "webhook", "pii-scanner", "request", sp("compliance"), sp("https://h"), (*string)(nil),
		json.RawMessage(`{}`), 10, 5000, "fail_open", true, []string{"ALL"}, tNow, tNow,
	}
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestListHookConfigs(t *testing.T) {
	s, m := newMock(t)
	enabled := true
	p := HookConfigListParams{Q: "x", Enabled: &enabled, Pipeline: "request", Limit: 10}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "HookConfig"`).WithArgs("%x%", true).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "HookConfig"`).WithArgs("%x%", true, 10, 0).
		WillReturnRows(pgxmock.NewRows(hcCols).AddRow(hcRow("h1", "scan")...))
	hooks, total, err := s.ListHookConfigs(context.Background(), p)
	if err != nil || total != 1 || len(hooks) != 1 || hooks[0].ID != "h1" || hooks[0].FailBehavior != "fail_open" {
		t.Fatalf("ListHookConfigs: %+v total=%d err=%v", hooks, total, err)
	}
	// response pipeline branch
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m2.ExpectQuery(`FROM "HookConfig"`).WithArgs(0, 0).WillReturnRows(pgxmock.NewRows(hcCols))
	if _, _, err := s2.ListHookConfigs(context.Background(), HookConfigListParams{Pipeline: "response"}); err != nil {
		t.Fatalf("response pipeline: %v", err)
	}
}

func TestListHookConfigs_Errors(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnError(errors.New("boom"))
	if _, _, err := s.ListHookConfigs(context.Background(), HookConfigListParams{}); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM "HookConfig"`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListHookConfigs(context.Background(), HookConfigListParams{Limit: 5}); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := hcRow("h1", "scan")
	bad[14] = "not-a-time"
	m3.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM "HookConfig"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(hcCols).AddRow(bad...))
	if _, _, err := s3.ListHookConfigs(context.Background(), HookConfigListParams{Limit: 5}); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetHookConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "HookConfig" WHERE id = \$1`).WithArgs("h1").
		WillReturnRows(pgxmock.NewRows(hcCols).AddRow(hcRow("h1", "scan")...))
	if h, err := s.GetHookConfig(context.Background(), "h1"); err != nil || h == nil || h.ID != "h1" {
		t.Fatalf("GetHookConfig: %+v %v", h, err)
	}
	m.ExpectQuery(`FROM "HookConfig" WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if h, err := s.GetHookConfig(context.Background(), "missing"); err != nil || h != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", h, err)
	}
	m.ExpectQuery(`FROM "HookConfig" WHERE id`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetHookConfig(context.Background(), "e"); err == nil {
		t.Fatal("db error should surface")
	}
}

func TestCreateUpdateHookConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`INSERT INTO "HookConfig"`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows(hcCols).AddRow(hcRow("h1", "scan")...))
	if h, err := s.CreateHookConfig(context.Background(), CreateHookConfigParams{Name: "scan", Type: "webhook", Stage: "request"}); err != nil || h == nil {
		t.Fatalf("CreateHookConfig: %+v %v", h, err)
	}
	m.ExpectQuery(`INSERT INTO "HookConfig"`).WithArgs(anyArgs(13)...).WillReturnError(errors.New("dup"))
	if _, err := s.CreateHookConfig(context.Background(), CreateHookConfigParams{}); err == nil {
		t.Fatal("create error should surface")
	}
	m.ExpectQuery(`UPDATE "HookConfig" SET`).WithArgs(anyArgs(14)...).
		WillReturnRows(pgxmock.NewRows(hcCols).AddRow(hcRow("h1", "scan")...))
	if _, err := s.UpdateHookConfig(context.Background(), "h1", UpdateHookConfigParams{Name: sp("New")}); err != nil {
		t.Fatalf("UpdateHookConfig: %v", err)
	}
	m.ExpectQuery(`UPDATE "HookConfig"`).WithArgs(anyArgs(14)...).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateHookConfig(context.Background(), "h1", UpdateHookConfigParams{}); err == nil {
		t.Fatal("update error should surface")
	}
}

func TestDeleteHookConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "HookConfig" WHERE id = \$1`).WithArgs("h1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteHookConfig(context.Background(), "h1"); err != nil {
		t.Fatalf("DeleteHookConfig: %v", err)
	}
	m.ExpectExec(`DELETE FROM "HookConfig"`).WithArgs("gone").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.DeleteHookConfig(context.Background(), "gone"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing → ErrNoRows, got %v", err)
	}
	m.ExpectExec(`DELETE FROM "HookConfig"`).WithArgs("h1").WillReturnError(errors.New("fk"))
	if err := s.DeleteHookConfig(context.Background(), "h1"); err == nil {
		t.Fatal("exec error should surface")
	}
}

// TestReorderHooksByStage covers the transactional priority-reorder: count must
// equal len(ids), each gets a sequential priority, then commit.
func TestReorderHooksByStage(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`SELECT COUNT\(\*\) FROM "HookConfig" WHERE stage = \$1`).WithArgs("request").
			WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(2))
		m.ExpectExec(`UPDATE "HookConfig" SET priority`).WithArgs(0, "h1", "request").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		m.ExpectExec(`UPDATE "HookConfig" SET priority`).WithArgs(1, "h2", "request").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		m.ExpectCommit()
		if err := s.ReorderHooksByStage(context.Background(), "request", []string{"h1", "h2"}); err != nil {
			t.Fatalf("ReorderHooksByStage: %v", err)
		}
	})
	t.Run("count mismatch", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`SELECT COUNT`).WithArgs("request").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(3))
		m.ExpectRollback()
		if err := s.ReorderHooksByStage(context.Background(), "request", []string{"h1"}); err == nil {
			t.Fatal("count mismatch should error (must provide exactly N IDs)")
		}
	})
	t.Run("begin error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin().WillReturnError(errors.New("no tx"))
		if err := s.ReorderHooksByStage(context.Background(), "request", nil); err == nil {
			t.Fatal("begin error should surface")
		}
	})
	t.Run("count query error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`SELECT COUNT`).WithArgs("request").WillReturnError(errors.New("boom"))
		m.ExpectRollback()
		if err := s.ReorderHooksByStage(context.Background(), "request", []string{"h1"}); err == nil {
			t.Fatal("count query error should surface")
		}
	})
	t.Run("update error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`SELECT COUNT`).WithArgs("request").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
		m.ExpectExec(`UPDATE "HookConfig" SET priority`).WithArgs(0, "h1", "request").WillReturnError(errors.New("boom"))
		m.ExpectRollback()
		if err := s.ReorderHooksByStage(context.Background(), "request", []string{"h1"}); err == nil {
			t.Fatal("update error should surface")
		}
	})
	t.Run("commit error", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectBegin()
		m.ExpectQuery(`SELECT COUNT`).WithArgs("request").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
		m.ExpectExec(`UPDATE "HookConfig" SET priority`).WithArgs(0, "h1", "request").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		m.ExpectCommit().WillReturnError(errors.New("commit failed"))
		if err := s.ReorderHooksByStage(context.Background(), "request", []string{"h1"}); err == nil {
			t.Fatal("commit error should surface")
		}
	})
}
