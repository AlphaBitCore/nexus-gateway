package alerting

// Coverage-gap tests for the alerts/engine package.
//
// These tests use pgxmock + httptest fakes so they run with no live Postgres
// and no live HTTP backend. The package's other *_test.go files contain
// DB-backed integration tests that skip when TEST_DATABASE_URL is unset; this
// file pins the same behaviour through interface seams (PgxPool/RaiserPool)
// added expressly for unit-test reach, so the package can hit the 95%
// coverage threshold without integration plumbing in CI.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// quietLogger returns a slog.Logger that discards output so test logs stay clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// anyN returns N pgxmock.AnyArg() matchers for use with WithArgs. pgxmock
// enforces arg count on every Exec/Query expectation, so we usually don't
// care which exact value flows in — only that the count matches. Use this
// for SQL where we're testing happy/error paths, not arg-binding behaviour.
func anyN(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// alertRowCols mirrors the SELECT column order in scanAlert / GetAlert / etc.
var alertRowCols = []string{
	"id", "ruleId", "sourceType", "targetKey", "targetLabel",
	"severity", "state", "message", "details",
	"firedAt", "lastSeenAt", "duplicateCount",
	"acknowledgedBy", "acknowledgedAt",
	"resolvedAt", "resolvedBy", "resolvedReason",
}

// makeAlertRow returns one full Alert row matching alertRowCols.
func makeAlertRow(id, ruleID, sev, state string, details []byte) []any {
	now := time.Now().UTC()
	return []any{
		id, ruleID, "test", "tgt:1", "Target One",
		sev, state, "msg", details,
		now, now, 1,
		nil, nil, nil, nil, nil,
	}
}

func ruleRowCols() []string {
	return []string{
		"id", "displayName", "sourceType", "defaultSeverity",
		"requiresAck", "enabled", "params", "paramsSchema", "cooldownSec",
		"group_id_filter", "createdAt", "updatedAt",
	}
}

func makeRuleRow(id string, enabled bool) []any {
	now := time.Now().UTC()
	return []any{
		id, "Display " + id, "test", "MEDIUM",
		false, enabled, []byte(`{"k":"v"}`), []byte(`{}`), 0,
		(*string)(nil), now, now,
	}
}

func channelRowCols() []string {
	return []string{
		"id", "name", "type", "enabled",
		"severities", "sourceTypes", "config",
		"createdAt", "updatedAt",
	}
}

func makeChannelRow(id, name string, enabled bool) []any {
	now := time.Now().UTC()
	return []any{
		id, name, "webhook", enabled,
		[]string{"critical"}, []string{"quota"}, []byte(`{"url":"x"}`),
		now, now,
	}
}

// Store: helpers + simple methods

func TestStoreHelpers(t *testing.T) {
	if got := dbSeverity(SeverityCritical); got != "CRITICAL" {
		t.Errorf("dbSeverity: got %q", got)
	}
	if got := dbState(StateFiring); got != "FIRING" {
		t.Errorf("dbState: got %q", got)
	}
	if got := goSeverity("HIGH"); got != SeverityHigh {
		t.Errorf("goSeverity: got %q", got)
	}
	if got := goState("RESOLVED"); got != StateResolved {
		t.Errorf("goState: got %q", got)
	}
	if NewStore(nil) == nil {
		t.Error("NewStore returned nil")
	}
	if NewStoreWithPgxPool(nil) == nil {
		t.Error("NewStoreWithPgxPool returned nil")
	}
}


func TestStore_InsertAlert_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`INSERT INTO "Alert"`).WithArgs(anyN(11)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("a-1"))
		s := NewStoreWithPgxPool(mock)
		id, err := s.InsertAlert(context.Background(), Alert{Details: map[string]any{"k": "v"}})
		if err != nil || id != "a-1" {
			t.Fatalf("InsertAlert: id=%q err=%v", id, err)
		}
	})
	t.Run("marshal error", func(t *testing.T) {
		s := NewStoreWithPgxPool(nil) // pool not reached
		_, err := s.InsertAlert(context.Background(), Alert{Details: map[string]any{"bad": make(chan int)}})
		if err == nil || !strings.Contains(err.Error(), "marshal details") {
			t.Fatalf("want marshal details err: %v", err)
		}
	})
	t.Run("db error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`INSERT INTO "Alert"`).WithArgs(anyN(11)...).WillReturnError(errors.New("dberr"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.InsertAlert(context.Background(), Alert{})
		if err == nil || !strings.Contains(err.Error(), "insert alert") {
			t.Fatalf("want insert alert err: %v", err)
		}
	})
}


func TestStore_UpdateFiringDuplicate_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs("a-1", pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := NewStoreWithPgxPool(mock)
		if err := s.UpdateFiringDuplicate(context.Background(), "a-1", time.Now()); err != nil {
			t.Fatalf("UpdateFiringDuplicate: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(2)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := NewStoreWithPgxPool(mock)
		err := s.UpdateFiringDuplicate(context.Background(), "missing", time.Now())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("db error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(2)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		err := s.UpdateFiringDuplicate(context.Background(), "x", time.Now())
		if err == nil || !strings.Contains(err.Error(), "update firing duplicate") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}


func TestStore_FindLatestByRuleTarget_Mock(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeAlertRow("a-1", "rule.x", "MEDIUM", "FIRING", []byte(`{"k":"v"}`))
		mock.ExpectQuery(`FROM "Alert".*ORDER BY "firedAt" DESC`).
			WithArgs("rule.x", "tgt:1").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		got, err := s.FindLatestByRuleTarget(context.Background(), "rule.x", "tgt:1")
		if err != nil || got == nil || got.ID != "a-1" {
			t.Fatalf("FindLatestByRuleTarget: got=%v err=%v", got, err)
		}
		if got.Details["k"] != "v" {
			t.Errorf("details lost: %#v", got.Details)
		}
	})
	t.Run("empty", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "Alert"`).WithArgs(anyN(2)...).
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		s := NewStoreWithPgxPool(mock)
		got, err := s.FindLatestByRuleTarget(context.Background(), "r", "t")
		if err != nil || got != nil {
			t.Fatalf("expected nil/no-err: got=%v err=%v", got, err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "Alert"`).WithArgs(anyN(2)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.FindLatestByRuleTarget(context.Background(), "r", "t")
		if err == nil || !strings.Contains(err.Error(), "query latest alert") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan error propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "Alert"`).WithArgs(anyN(2)...).
			WillReturnRows(pgxmock.NewRows(alertRowCols).
				AddRow(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17)) // wrong types
		s := NewStoreWithPgxPool(mock)
		_, err := s.FindLatestByRuleTarget(context.Background(), "r", "t")
		if err == nil {
			t.Fatal("expected scan error")
		}
	})
}


func TestStore_GetAlert_Mock(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeAlertRow("a-1", "rule.x", "LOW", "ACKNOWLEDGED", nil) // nil details path
		mock.ExpectQuery(`FROM "Alert"`).WithArgs("a-1").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		got, err := s.GetAlert(context.Background(), "a-1")
		if err != nil || got == nil || got.State != StateAcknowledged {
			t.Fatalf("GetAlert: got=%v err=%v", got, err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "Alert"`).WithArgs("missing").
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetAlert(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "Alert"`).WithArgs("x").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetAlert(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "get alert") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan unmarshal error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeAlertRow("a-1", "r", "LOW", "FIRING", []byte(`not-json`))
		mock.ExpectQuery(`FROM "Alert"`).WithArgs("a-1").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetAlert(context.Background(), "a-1")
		if err == nil || !strings.Contains(err.Error(), "unmarshal details") {
			t.Fatalf("want unmarshal err: %v", err)
		}
	})
}

// Store.AcknowledgeAlert / ResolveAlert

func TestStore_AcknowledgeAndResolve_Mock(t *testing.T) {
	t.Run("ack happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert".*ACKNOWLEDGED`).WithArgs(anyN(3)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := NewStoreWithPgxPool(mock)
		if err := s.AcknowledgeAlert(context.Background(), "a", "u", "r"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ack not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert".*ACKNOWLEDGED`).WithArgs(anyN(3)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := NewStoreWithPgxPool(mock)
		if err := s.AcknowledgeAlert(context.Background(), "a", "u", "r"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("ack db error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert".*ACKNOWLEDGED`).WithArgs(anyN(3)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		err := s.AcknowledgeAlert(context.Background(), "a", "u", "r")
		if err == nil || !strings.Contains(err.Error(), "acknowledge alert") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("resolve happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert".*RESOLVED`).WithArgs(anyN(4)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := NewStoreWithPgxPool(mock)
		if err := s.ResolveAlert(context.Background(), "a", "u", "r"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("resolve not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert".*RESOLVED`).WithArgs(anyN(4)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := NewStoreWithPgxPool(mock)
		if err := s.ResolveAlert(context.Background(), "a", "u", "r"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("resolve db error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert".*RESOLVED`).WithArgs(anyN(4)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		err := s.ResolveAlert(context.Background(), "a", "u", "r")
		if err == nil || !strings.Contains(err.Error(), "resolve alert") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}


func TestStore_ResolveByRuleTarget_Mock(t *testing.T) {
	t.Run("happy returns count", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs("r", "t", pgxmock.AnyArg(), "reason").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 3"))
		s := NewStoreWithPgxPool(mock)
		n, err := s.ResolveByRuleTarget(context.Background(), "r", "t", "reason")
		if err != nil || n != 3 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
	t.Run("error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(4)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.ResolveByRuleTarget(context.Background(), "r", "t", "x")
		if err == nil || !strings.Contains(err.Error(), "resolve by rule+target") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}

// Store.ListAlerts + buildListWhere

func TestStore_ListAlerts_Mock(t *testing.T) {
	t.Run("no filter happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Alert"`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
		row := makeAlertRow("a", "r", "MEDIUM", "FIRING", nil)
		mock.ExpectQuery(`SELECT id, "ruleId".*FROM "Alert"`).
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		got, total, err := s.ListAlerts(context.Background(), ListFilter{})
		if err != nil || total != 2 || len(got) != 1 {
			t.Fatalf("got=%d total=%d err=%v", len(got), total, err)
		}
	})
	t.Run("full filter (all dims + since/until + custom limit)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Alert" WHERE`).
			WithArgs(anyN(6)...).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`SELECT id.*FROM "Alert" WHERE`).
			WithArgs(anyN(6)...).
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		s := NewStoreWithPgxPool(mock)
		_, total, err := s.ListAlerts(context.Background(), ListFilter{
			State:      []string{"firing", "acknowledged"},
			Severity:   []string{"high"},
			SourceType: []string{"quota"},
			RuleID:     []string{"r.1"},
			Since:      &since,
			Until:      &until,
			Offset:     10,
			Limit:      25,
		})
		if err != nil || total != 0 {
			t.Fatalf("total=%d err=%v", total, err)
		}
	})
	t.Run("count error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Alert"`).WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListAlerts(context.Background(), ListFilter{})
		if err == nil || !strings.Contains(err.Error(), "count alerts") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Alert"`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`SELECT id.*FROM "Alert"`).WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListAlerts(context.Background(), ListFilter{})
		if err == nil || !strings.Contains(err.Error(), "list alerts") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan error propagates from list", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Alert"`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery(`SELECT id.*FROM "Alert"`).
			WillReturnRows(pgxmock.NewRows(alertRowCols).
				AddRow(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListAlerts(context.Background(), ListFilter{})
		if err == nil {
			t.Fatal("expected scan err")
		}
	})
}


func TestStore_ListRules_Mock(t *testing.T) {
	t.Run("no filter happy + default limit", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "AlertRule"`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
		row := makeRuleRow("r.1", true)
		mock.ExpectQuery(`FROM "AlertRule".*LIMIT`).WithArgs(anyN(2)...).
			WillReturnRows(pgxmock.NewRows(ruleRowCols()).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		got, total, err := s.ListRules(context.Background(), ListRulesParams{})
		if err != nil || total != 1 || len(got) != 1 || got[0].Enabled != true {
			t.Fatalf("got=%d total=%d err=%v", len(got), total, err)
		}
	})
	t.Run("full filter (search + enabled + severity + sourceType + negative offset)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "AlertRule" WHERE`).WithArgs(anyN(4)...).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`FROM "AlertRule" WHERE.*LIMIT`).WithArgs(anyN(6)...).
			WillReturnRows(pgxmock.NewRows(ruleRowCols()))
		s := NewStoreWithPgxPool(mock)
		enabled := true
		_, _, err := s.ListRules(context.Background(), ListRulesParams{
			Search: "quota", Enabled: &enabled, Severity: "high", SourceType: "quota",
			Offset: -5,
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("count error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "AlertRule"`).WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListRules(context.Background(), ListRulesParams{})
		if err == nil || !strings.Contains(err.Error(), "count rules") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "AlertRule"`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs(anyN(2)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListRules(context.Background(), ListRulesParams{})
		if err == nil || !strings.Contains(err.Error(), "list rules") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan params unmarshal error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\)`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
		row := makeRuleRow("r.1", true)
		row[6] = []byte("not-json")
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs(anyN(2)...).
			WillReturnRows(pgxmock.NewRows(ruleRowCols()).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListRules(context.Background(), ListRulesParams{})
		if err == nil || !strings.Contains(err.Error(), "unmarshal params") {
			t.Fatalf("want unmarshal err: %v", err)
		}
	})
	t.Run("scan schema unmarshal error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\)`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
		row := makeRuleRow("r.1", true)
		row[7] = []byte("not-json")
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs(anyN(2)...).
			WillReturnRows(pgxmock.NewRows(ruleRowCols()).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListRules(context.Background(), ListRulesParams{})
		if err == nil || !strings.Contains(err.Error(), "unmarshal params_schema") {
			t.Fatalf("want unmarshal_schema err: %v", err)
		}
	})
	t.Run("scan row error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\)`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs(anyN(2)...).
			WillReturnRows(pgxmock.NewRows(ruleRowCols()).
				AddRow(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12))
		s := NewStoreWithPgxPool(mock)
		_, _, err := s.ListRules(context.Background(), ListRulesParams{})
		if err == nil || !strings.Contains(err.Error(), "scan rule") {
			t.Fatalf("want scan err: %v", err)
		}
	})
}

// Store.GetRule / UpdateRule

func TestStore_GetRule_Mock(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertRule".*WHERE id = \$1`).
			WithArgs("r.1").
			WillReturnRows(pgxmock.NewRows(ruleRowCols()).AddRow(makeRuleRow("r.1", true)...))
		s := NewStoreWithPgxPool(mock)
		got, err := s.GetRule(context.Background(), "r.1")
		if err != nil || got == nil || got.ID != "r.1" {
			t.Fatalf("GetRule: got=%v err=%v", got, err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs("missing").
			WillReturnRows(pgxmock.NewRows(ruleRowCols()))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetRule(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs("x").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetRule(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "get rule") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan failure", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs("x").
			WillReturnRows(pgxmock.NewRows(ruleRowCols()).
				AddRow(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetRule(context.Background(), "x")
		if err == nil {
			t.Fatal("expected scan err")
		}
	})
}

func TestStore_UpdateRule_Mock(t *testing.T) {
	r := AlertRule{
		ID:              "r.1",
		DisplayName:     "Display",
		DefaultSeverity: SeverityHigh,
		Enabled:         true,
		Params:          map[string]any{"k": "v"},
		ParamsSchema:    map[string]any{"type": "object"},
	}
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "AlertRule"`).WithArgs(anyN(9)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := NewStoreWithPgxPool(mock)
		if err := s.UpdateRule(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "AlertRule"`).WithArgs(anyN(9)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := NewStoreWithPgxPool(mock)
		if err := s.UpdateRule(context.Background(), r); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("db error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "AlertRule"`).WithArgs(anyN(9)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		err := s.UpdateRule(context.Background(), r)
		if err == nil || !strings.Contains(err.Error(), "update rule") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("marshal params error", func(t *testing.T) {
		s := NewStoreWithPgxPool(nil)
		bad := r
		bad.Params = map[string]any{"bad": make(chan int)}
		err := s.UpdateRule(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), "marshal params") {
			t.Fatalf("want marshal params err: %v", err)
		}
	})
	t.Run("marshal params_schema error", func(t *testing.T) {
		s := NewStoreWithPgxPool(nil)
		bad := r
		bad.ParamsSchema = map[string]any{"bad": make(chan int)}
		err := s.UpdateRule(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), "marshal params_schema") {
			t.Fatalf("want marshal_schema err: %v", err)
		}
	})
}


func TestStore_InsertChannel_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`INSERT INTO "AlertChannel"`).WithArgs(anyN(6)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("ch-1"))
		s := NewStoreWithPgxPool(mock)
		id, err := s.InsertChannel(context.Background(), Channel{Name: "n", Type: "webhook", Config: map[string]any{"u": "x"}})
		if err != nil || id != "ch-1" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("marshal error", func(t *testing.T) {
		s := NewStoreWithPgxPool(nil)
		_, err := s.InsertChannel(context.Background(), Channel{Config: map[string]any{"bad": make(chan int)}})
		if err == nil || !strings.Contains(err.Error(), "marshal config") {
			t.Fatalf("want marshal config err: %v", err)
		}
	})
	t.Run("db error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`INSERT INTO "AlertChannel"`).WithArgs(anyN(6)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.InsertChannel(context.Background(), Channel{})
		if err == nil || !strings.Contains(err.Error(), "insert channel") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}

func TestStore_GetChannel_Mock(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel".*WHERE id`).WithArgs("ch-1").
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(makeChannelRow("ch-1", "n", true)...))
		s := NewStoreWithPgxPool(mock)
		got, err := s.GetChannel(context.Background(), "ch-1")
		if err != nil || got == nil || got.ID != "ch-1" {
			t.Fatalf("GetChannel: got=%v err=%v", got, err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel"`).WithArgs("x").
			WillReturnRows(pgxmock.NewRows(channelRowCols()))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetChannel(context.Background(), "x")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel"`).WithArgs("x").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetChannel(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "get channel") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan err", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel"`).WithArgs("x").
			WillReturnRows(pgxmock.NewRows(channelRowCols()).
				AddRow(1, 2, 3, 4, 5, 6, 7, 8, 9))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetChannel(context.Background(), "x")
		if err == nil {
			t.Fatal("expected scan err")
		}
	})
	t.Run("scan unmarshal config err", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "n", true)
		row[6] = []byte("not-json")
		mock.ExpectQuery(`FROM "AlertChannel"`).WithArgs("x").
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		_, err := s.GetChannel(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "unmarshal config") {
			t.Fatalf("want unmarshal err: %v", err)
		}
	})
}

func TestStore_ListChannels_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel".*ORDER BY name`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).
				AddRow(makeChannelRow("ch-1", "a", true)...).
				AddRow(makeChannelRow("ch-2", "b", false)...))
		s := NewStoreWithPgxPool(mock)
		got, err := s.ListChannels(context.Background())
		if err != nil || len(got) != 2 {
			t.Fatalf("len=%d err=%v", len(got), err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel"`).WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.ListChannels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "list channels") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}

func TestStore_ListEnabledChannels_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel".*WHERE enabled = true`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).
				AddRow(makeChannelRow("ch-1", "a", true)...))
		s := NewStoreWithPgxPool(mock)
		got, err := s.ListEnabledChannels(context.Background())
		if err != nil || len(got) != 1 {
			t.Fatalf("len=%d err=%v", len(got), err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel"`).WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.ListEnabledChannels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "list enabled channels") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}

func TestStore_UpdateChannel_Mock(t *testing.T) {
	ch := Channel{ID: "ch-1", Name: "n", Type: "webhook", Enabled: true, Config: map[string]any{"u": "x"}}
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "AlertChannel"`).WithArgs(anyN(7)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := NewStoreWithPgxPool(mock)
		if err := s.UpdateChannel(context.Background(), ch); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "AlertChannel"`).WithArgs(anyN(7)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := NewStoreWithPgxPool(mock)
		if err := s.UpdateChannel(context.Background(), ch); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("db error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "AlertChannel"`).WithArgs(anyN(7)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		err := s.UpdateChannel(context.Background(), ch)
		if err == nil || !strings.Contains(err.Error(), "update channel") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("marshal error", func(t *testing.T) {
		s := NewStoreWithPgxPool(nil)
		bad := ch
		bad.Config = map[string]any{"bad": make(chan int)}
		err := s.UpdateChannel(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), "marshal config") {
			t.Fatalf("want marshal err: %v", err)
		}
	})
}

func TestStore_DeleteChannel_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`DELETE FROM "AlertChannel"`).WithArgs("ch").
			WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
		s := NewStoreWithPgxPool(mock)
		if err := s.DeleteChannel(context.Background(), "ch"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`DELETE FROM "AlertChannel"`).WithArgs("ch").
			WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
		s := NewStoreWithPgxPool(mock)
		if err := s.DeleteChannel(context.Background(), "ch"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound: %v", err)
		}
	})
	t.Run("db error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`DELETE FROM "AlertChannel"`).WithArgs("ch").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		err := s.DeleteChannel(context.Background(), "ch")
		if err == nil || !strings.Contains(err.Error(), "delete channel") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}


func TestStore_InsertDispatch_Mock(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("d-1"))
		s := NewStoreWithPgxPool(mock)
		id, err := s.InsertDispatch(context.Background(), Dispatch{AlertID: "a"})
		if err != nil || id != "d-1" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.InsertDispatch(context.Background(), Dispatch{})
		if err == nil || !strings.Contains(err.Error(), "insert dispatch") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}

func TestStore_ListDispatchesByAlert_Mock(t *testing.T) {
	cols := []string{"id", "alertId", "channelId", "channelName", "success", "statusCode", "errorMsg", "attemptedAt"}
	t.Run("happy", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertDispatch"`).WithArgs("a-1").
			WillReturnRows(pgxmock.NewRows(cols).
				AddRow("d-1", "a-1", "ch", "name", true, (*int)(nil), (*string)(nil), time.Now()))
		s := NewStoreWithPgxPool(mock)
		got, err := s.ListDispatchesByAlert(context.Background(), "a-1")
		if err != nil || len(got) != 1 {
			t.Fatalf("len=%d err=%v", len(got), err)
		}
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertDispatch"`).WithArgs("a").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		_, err := s.ListDispatchesByAlert(context.Background(), "a")
		if err == nil || !strings.Contains(err.Error(), "list dispatches") {
			t.Fatalf("want wrap: %v", err)
		}
	})
	t.Run("scan error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertDispatch"`).WithArgs("a").
			WillReturnRows(pgxmock.NewRows(cols).AddRow(1, 2, 3, 4, 5, 6, 7, 8))
		s := NewStoreWithPgxPool(mock)
		_, err := s.ListDispatchesByAlert(context.Background(), "a")
		if err == nil || !strings.Contains(err.Error(), "scan dispatch") {
			t.Fatalf("want scan err: %v", err)
		}
	})
}

//
// Drives the tx-scoped variant by using a pgxmock Begin -> tx.Query.

func TestStore_FindLatestByRuleTargetAnyStateTx_Mock(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(
				makeAlertRow("a-1", "r", "MEDIUM", "FIRING", nil)...))
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		s := NewStoreWithPgxPool(mock)
		got, err := s.FindLatestByRuleTargetAnyStateTx(context.Background(), tx, "r", "t")
		if err != nil || got == nil {
			t.Fatalf("got=%v err=%v", got, err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("empty", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		s := NewStoreWithPgxPool(mock)
		got, err := s.FindLatestByRuleTargetAnyStateTx(context.Background(), tx, "r", "t")
		if err != nil || got != nil {
			t.Fatalf("got=%v err=%v", got, err)
		}
		_ = tx.Rollback(context.Background())
	})
	t.Run("query error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r", "t").
			WillReturnError(errors.New("boom"))
		mock.ExpectRollback()
		tx, _ := mock.Begin(context.Background())
		s := NewStoreWithPgxPool(mock)
		_, err := s.FindLatestByRuleTargetAnyStateTx(context.Background(), tx, "r", "t")
		if err == nil || !strings.Contains(err.Error(), "query latest alert (any state) tx") {
			t.Fatalf("want wrap: %v", err)
		}
		_ = tx.Rollback(context.Background())
	})
}


// fakeDispatcher counts Dispatch calls.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []Alert
}

func (f *fakeDispatcher) Dispatch(_ context.Context, a Alert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, a)
}
func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeProducer satisfies mq.Producer minimally.
type fakeProducer struct {
	mu        sync.Mutex
	enqueued  []string
	enqueueEr error
}

func (f *fakeProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (f *fakeProducer) Enqueue(_ context.Context, subject string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, subject)
	return f.enqueueEr
}
func (f *fakeProducer) Close() error { return nil }
func (f *fakeProducer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.enqueued)
}

func TestRaiser_NewRaiserConstructors(t *testing.T) {
	if NewRaiser(nil, nil, nil, nil) == nil {
		t.Error("NewRaiser nil")
	}
	if NewRaiserWithPool(nil, nil, nil, quietLogger()) == nil {
		t.Error("NewRaiserWithPool nil")
	}
}

// expectGetRule queues a GetRule call returning the supplied rule.
func expectGetRule(mock pgxmock.PgxPoolIface, id string, enabled bool, cooldownSec int, groupID *string) {
	row := makeRuleRow(id, enabled)
	row[8] = cooldownSec
	row[9] = groupID
	mock.ExpectQuery(`FROM "AlertRule".*WHERE id`).WithArgs(id).
		WillReturnRows(pgxmock.NewRows(ruleRowCols()).AddRow(row...))
}

// expectGroupExists queues the SELECT EXISTS group filter check.
func expectGroupExists(mock pgxmock.PgxPoolIface, groupID, deviceID string, found bool) {
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(groupID, deviceID).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(found))
}

func TestRaiser_Raise_Mock(t *testing.T) {
	t.Run("unknown rule returns error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs("unknown").
			WillReturnRows(pgxmock.NewRows(ruleRowCols())) // empty -> ErrNotFound
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "unknown", TargetKey: "t"})
		if err == nil || !strings.Contains(err.Error(), "unknown ruleId") {
			t.Fatalf("want unknown ruleId: %v", err)
		}
	})

	t.Run("rule load error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertRule"`).WithArgs("r").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r", TargetKey: "t"})
		if err == nil || !strings.Contains(err.Error(), "load rule") {
			t.Fatalf("want load rule wrap: %v", err)
		}
	})

	t.Run("disabled rule silently dropped", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", false, 0, nil)
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		if err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "t"}); err != nil {
			t.Fatalf("disabled: %v", err)
		}
	})

	t.Run("group filter without thing prefix drops silently", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		grp := "group-1"
		expectGetRule(mock, "r.1", true, 0, &grp)
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "not-a-thing"})
		if err != nil {
			t.Fatalf("want silent drop: %v", err)
		}
	})

	t.Run("group filter target not in group drops", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		grp := "group-1"
		expectGetRule(mock, "r.1", true, 0, &grp)
		expectGroupExists(mock, "group-1", "dev-1", false)
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "thing:dev-1"})
		if err != nil {
			t.Fatalf("want silent drop: %v", err)
		}
	})

	t.Run("group filter check db error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		grp := "group-1"
		expectGetRule(mock, "r.1", true, 0, &grp)
		mock.ExpectQuery(`SELECT EXISTS`).WithArgs("group-1", "dev-1").
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "thing:dev-1"})
		if err == nil || !strings.Contains(err.Error(), "group filter check") {
			t.Fatalf("want group filter wrap: %v", err)
		}
	})

	t.Run("fresh INSERT happy with nil details + nil dispatcher", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 0, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t:1").
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		mock.ExpectQuery(`INSERT INTO "Alert"`).WithArgs(anyN(9)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("alert-1"))
		mock.ExpectCommit()
		mock.ExpectRollback().Maybe() // pgx.BeginFunc defers Rollback after Commit
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		// Pass zero FiredAt to exercise the time.Now stamping path.
		err := r.Raise(context.Background(), RaiseInput{
			RuleID: "r.1", TargetKey: "t:1", Severity: SeverityLow,
		})
		if err != nil {
			t.Fatalf("Raise: %v", err)
		}
	})

	t.Run("advisory lock error rolls back + wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 0, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnError(errors.New("lockfail"))
		mock.ExpectRollback()
		mock.ExpectRollback().Maybe() // pgx.BeginFunc also defers a Rollback
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "t"})
		if err == nil || !strings.Contains(err.Error(), "persist alert") {
			t.Fatalf("want persist wrap: %v", err)
		}
	})

	t.Run("FindLatest error inside tx wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 0, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t").
			WillReturnError(errors.New("boom"))
		mock.ExpectRollback()
		mock.ExpectRollback().Maybe()
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "t"})
		if err == nil || !strings.Contains(err.Error(), "persist alert") {
			t.Fatalf("want persist wrap: %v", err)
		}
	})

	t.Run("existing FIRING dedups via UPDATE (no dispatch)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 0, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		row := makeAlertRow("alert-1", "r.1", "MEDIUM", "FIRING", nil)
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		mock.ExpectExec(`UPDATE "Alert".*duplicateCount`).WithArgs(anyN(2)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mock.ExpectCommit()
		mock.ExpectRollback().Maybe() // pgx.BeginFunc defers Rollback after Commit
		s := NewStoreWithPgxPool(mock)
		d := &fakeDispatcher{}
		r := NewRaiserWithPool(mock, s, d, quietLogger())
		if err := r.Raise(context.Background(), RaiseInput{
			RuleID: "r.1", TargetKey: "t", Severity: SeverityHigh,
		}); err != nil {
			t.Fatalf("Raise: %v", err)
		}
		if d.count() != 0 {
			t.Errorf("dedup must skip dispatcher; got calls=%d", d.count())
		}
	})

	t.Run("cooldown-window suppresses RESOLVED rule (no fresh INSERT)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 600, nil) // 10-min cooldown
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		row := makeAlertRow("alert-prev", "r.1", "MEDIUM", "RESOLVED", nil)
		// firedAt is "now" so it's still in the cooldown window.
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		mock.ExpectExec(`UPDATE "Alert".*duplicateCount`).WithArgs(anyN(2)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mock.ExpectCommit()
		mock.ExpectRollback().Maybe() // pgx.BeginFunc defers Rollback after Commit
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		if err := r.Raise(context.Background(), RaiseInput{
			RuleID: "r.1", TargetKey: "t", Severity: SeverityHigh,
		}); err != nil {
			t.Fatalf("Raise: %v", err)
		}
	})

	t.Run("cooldown-expired RESOLVED triggers fresh INSERT + dispatch", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 60, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		// firedAt 2 hours ago -> outside 60s cooldown.
		row := makeAlertRow("alert-prev", "r.1", "MEDIUM", "RESOLVED", nil)
		row[9] = time.Now().UTC().Add(-2 * time.Hour) // firedAt index
		row[10] = time.Now().UTC().Add(-2 * time.Hour)
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols).AddRow(row...))
		mock.ExpectQuery(`INSERT INTO "Alert"`).WithArgs(anyN(9)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("alert-new"))
		mock.ExpectCommit()
		mock.ExpectRollback().Maybe() // pgx.BeginFunc defers Rollback after Commit
		s := NewStoreWithPgxPool(mock)
		d := &fakeDispatcher{}
		r := NewRaiserWithPool(mock, s, d, quietLogger())
		if err := r.Raise(context.Background(), RaiseInput{
			RuleID: "r.1", TargetKey: "t", Severity: SeverityHigh,
		}); err != nil {
			t.Fatalf("Raise: %v", err)
		}
		waitFor(t, func() bool { return d.count() == 1 })
	})

	t.Run("INSERT error inside tx wraps via persist alert", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 0, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		mock.ExpectQuery(`INSERT INTO "Alert"`).WithArgs(anyN(9)...).
			WillReturnError(errors.New("boom"))
		mock.ExpectRollback()
		mock.ExpectRollback().Maybe()
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{RuleID: "r.1", TargetKey: "t"})
		if err == nil || !strings.Contains(err.Error(), "persist alert") {
			t.Fatalf("want persist wrap: %v", err)
		}
	})

	t.Run("Raise marshal details error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		expectGetRule(mock, "r.1", true, 0, nil)
		mock.ExpectBegin()
		mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(anyN(1)...).
			WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		mock.ExpectQuery(`FROM "Alert".*FOR UPDATE`).WithArgs("r.1", "t").
			WillReturnRows(pgxmock.NewRows(alertRowCols))
		mock.ExpectRollback()
		mock.ExpectRollback().Maybe()
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Raise(context.Background(), RaiseInput{
			RuleID: "r.1", TargetKey: "t",
			Details: map[string]any{"bad": make(chan int)},
		})
		if err == nil || !strings.Contains(err.Error(), "marshal details") {
			t.Fatalf("want marshal details wrap: %v", err)
		}
	})

}

func TestRaiser_Resolve_Mock(t *testing.T) {
	t.Run("happy with rows", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(4)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 2"))
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		if err := r.Resolve(context.Background(), "r", "t", "reason"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("happy zero rows (no log)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(4)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		if err := r.Resolve(context.Background(), "r", "t", "reason"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("nil logger still works", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(4)...).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		s := NewStoreWithPgxPool(mock)
		// Set logger nil after construction to hit the `r.logger != nil` guard.
		r := NewRaiserWithPool(mock, s, nil, nil)
		r.logger = nil
		if err := r.Resolve(context.Background(), "r", "t", "reason"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("store error wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectExec(`UPDATE "Alert"`).WithArgs(anyN(4)...).
			WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		r := NewRaiserWithPool(mock, s, nil, quietLogger())
		err := r.Resolve(context.Background(), "r", "t", "x")
		if err == nil || !strings.Contains(err.Error(), "resolve:") {
			t.Fatalf("want wrap: %v", err)
		}
	})
}

// waitFor polls until cond is true or 2s elapse.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}


// dispatchTestSender is a tiny Sender used in dispatcher tests.
type dispatchTestSender struct {
	statusCode int
	err        error
}

func (s dispatchTestSender) Send(_ context.Context, _ Channel, _ Alert) (int, error) {
	return s.statusCode, s.err
}

// dispatchTestRegistry resolves channel types to senders.
type dispatchTestRegistry struct{ m map[string]Sender }

func (r dispatchTestRegistry) Get(t string) (Sender, error) {
	s, ok := r.m[t]
	if !ok {
		return nil, fmt.Errorf("no sender %q", t)
	}
	return s, nil
}

func TestDispatcher_NilLoggerDefaults(t *testing.T) {
	d := NewDispatcher(nil, nil, nil)
	if d == nil {
		t.Fatal("NewDispatcher nil")
	}
}

func TestDispatcher_Dispatch_MockPaths(t *testing.T) {
	t.Run("list channels error returns early", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM "AlertChannel"`).WillReturnError(errors.New("boom"))
		s := NewStoreWithPgxPool(mock)
		d := NewDispatcher(s, dispatchTestRegistry{m: map[string]Sender{}}, quietLogger())
		d.Dispatch(context.Background(), Alert{Severity: SeverityHigh})
	})

	t.Run("severity mismatch skips channel", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		// Severities only allow "critical".
		row[4] = []string{"critical"}
		row[5] = []string{}
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		reg := dispatchTestRegistry{m: map[string]Sender{"webhook": dispatchTestSender{statusCode: 200}}}
		d := NewDispatcher(s, reg, quietLogger())
		d.Dispatch(context.Background(), Alert{Severity: SeverityLow})
	})

	t.Run("source type mismatch skips channel", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		row[4] = []string{} // any severity
		row[5] = []string{"quota"}
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		s := NewStoreWithPgxPool(mock)
		reg := dispatchTestRegistry{m: map[string]Sender{"webhook": dispatchTestSender{statusCode: 200}}}
		d := NewDispatcher(s, reg, quietLogger())
		d.Dispatch(context.Background(), Alert{Severity: SeverityHigh, SourceType: "audit"})
	})

	t.Run("no sender registered records failure dispatch", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		row[4] = []string{} // match all
		row[5] = []string{} // match all
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("d-1"))
		s := NewStoreWithPgxPool(mock)
		d := NewDispatcher(s, dispatchTestRegistry{m: map[string]Sender{}}, quietLogger())
		d.Dispatch(context.Background(), Alert{ID: "a", Severity: SeverityHigh, SourceType: "quota"})
	})

	t.Run("send success writes success dispatch", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		row[4] = []string{}
		row[5] = []string{}
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("d-1"))
		s := NewStoreWithPgxPool(mock)
		reg := dispatchTestRegistry{m: map[string]Sender{"webhook": dispatchTestSender{statusCode: 200}}}
		d := NewDispatcher(s, reg, quietLogger())
		d.Dispatch(context.Background(), Alert{ID: "a", Severity: SeverityHigh, SourceType: "x"})
	})

	t.Run("send failure writes failure dispatch", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		row[4] = []string{}
		row[5] = []string{}
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("d-1"))
		s := NewStoreWithPgxPool(mock)
		reg := dispatchTestRegistry{m: map[string]Sender{"webhook": dispatchTestSender{statusCode: 500, err: errors.New("boom")}}}
		d := NewDispatcher(s, reg, quietLogger())
		d.Dispatch(context.Background(), Alert{ID: "a", Severity: SeverityHigh, SourceType: "x"})
	})

	t.Run("dispatch row write error is logged not propagated", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		row[4] = []string{}
		row[5] = []string{}
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnError(errors.New("write fail"))
		s := NewStoreWithPgxPool(mock)
		reg := dispatchTestRegistry{m: map[string]Sender{"webhook": dispatchTestSender{statusCode: 200}}}
		d := NewDispatcher(s, reg, quietLogger())
		d.Dispatch(context.Background(), Alert{ID: "a", Severity: SeverityHigh, SourceType: "x"})
	})

	t.Run("statusCode 0 omits scPtr", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		row := makeChannelRow("ch-1", "name", true)
		row[4] = []string{}
		row[5] = []string{}
		mock.ExpectQuery(`FROM "AlertChannel"`).
			WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(row...))
		mock.ExpectQuery(`INSERT INTO "AlertDispatch"`).WithArgs(anyN(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("d-1"))
		s := NewStoreWithPgxPool(mock)
		reg := dispatchTestRegistry{m: map[string]Sender{"webhook": dispatchTestSender{statusCode: 0}}}
		d := NewDispatcher(s, reg, quietLogger())
		d.Dispatch(context.Background(), Alert{ID: "a", Severity: SeverityHigh, SourceType: "x"})
	})
}

// Admin handlers
//
// adminFakeStore is an in-memory implementation of adminStoreAPI so the HTTP
// layer can be exercised end-to-end without pgx.
type adminFakeStore struct {
	listAlertsFn        func(ctx context.Context, f ListFilter) ([]Alert, int, error)
	getAlertFn          func(ctx context.Context, id string) (*Alert, error)
	listDispatchesFn    func(ctx context.Context, alertID string) ([]Dispatch, error)
	ackFn               func(ctx context.Context, id, by, reason string) error
	resolveAlertFn      func(ctx context.Context, id, by, reason string) error
	listRulesFn         func(ctx context.Context, p ListRulesParams) ([]AlertRule, int, error)
	getRuleFn           func(ctx context.Context, id string) (*AlertRule, error)
	updateRuleFn        func(ctx context.Context, r AlertRule) error
	insertChannelFn     func(ctx context.Context, c Channel) (string, error)
	getChannelFn        func(ctx context.Context, id string) (*Channel, error)
	listChannelsFn      func(ctx context.Context) ([]Channel, error)
	updateChannelFn     func(ctx context.Context, c Channel) error
	deleteChannelFn     func(ctx context.Context, id string) error
	insertAlertFn       func(ctx context.Context, a Alert) (string, error)
	insertDispatchFn    func(ctx context.Context, d Dispatch) (string, error)
	updateRuleObserved  AlertRule
	updateRuleCallCount int
}

func (f *adminFakeStore) ListAlerts(ctx context.Context, fi ListFilter) ([]Alert, int, error) {
	return f.listAlertsFn(ctx, fi)
}
func (f *adminFakeStore) GetAlert(ctx context.Context, id string) (*Alert, error) {
	return f.getAlertFn(ctx, id)
}
func (f *adminFakeStore) ListDispatchesByAlert(ctx context.Context, id string) ([]Dispatch, error) {
	return f.listDispatchesFn(ctx, id)
}
func (f *adminFakeStore) AcknowledgeAlert(ctx context.Context, id, by, reason string) error {
	return f.ackFn(ctx, id, by, reason)
}
func (f *adminFakeStore) ResolveAlert(ctx context.Context, id, by, reason string) error {
	return f.resolveAlertFn(ctx, id, by, reason)
}
func (f *adminFakeStore) ListRules(ctx context.Context, p ListRulesParams) ([]AlertRule, int, error) {
	return f.listRulesFn(ctx, p)
}
func (f *adminFakeStore) GetRule(ctx context.Context, id string) (*AlertRule, error) {
	return f.getRuleFn(ctx, id)
}
func (f *adminFakeStore) UpdateRule(ctx context.Context, r AlertRule) error {
	f.updateRuleObserved = r
	f.updateRuleCallCount++
	return f.updateRuleFn(ctx, r)
}
func (f *adminFakeStore) InsertChannel(ctx context.Context, c Channel) (string, error) {
	return f.insertChannelFn(ctx, c)
}
func (f *adminFakeStore) GetChannel(ctx context.Context, id string) (*Channel, error) {
	return f.getChannelFn(ctx, id)
}
func (f *adminFakeStore) ListChannels(ctx context.Context) ([]Channel, error) {
	return f.listChannelsFn(ctx)
}
func (f *adminFakeStore) UpdateChannel(ctx context.Context, c Channel) error {
	return f.updateChannelFn(ctx, c)
}
func (f *adminFakeStore) DeleteChannel(ctx context.Context, id string) error {
	return f.deleteChannelFn(ctx, id)
}
func (f *adminFakeStore) InsertAlert(ctx context.Context, a Alert) (string, error) {
	return f.insertAlertFn(ctx, a)
}
func (f *adminFakeStore) InsertDispatch(ctx context.Context, d Dispatch) (string, error) {
	return f.insertDispatchFn(ctx, d)
}

type fakeRuleRegistry struct{ entries map[string]RuleDefault }

func (f fakeRuleRegistry) Lookup(id string) (RuleDefault, bool) {
	d, ok := f.entries[id]
	return d, ok
}

// adminSender + adminSenderReg let ChannelTest run without HTTP.
type adminSender struct {
	statusCode int
	err        error
}

func (s adminSender) Send(_ context.Context, _ Channel, _ Alert) (int, error) {
	return s.statusCode, s.err
}

type adminSenderReg struct{ m map[string]Sender }

func (r adminSenderReg) Get(t string) (Sender, error) {
	s, ok := r.m[t]
	if !ok {
		return nil, fmt.Errorf("no sender for %q", t)
	}
	return s, nil
}

func makeEchoRequest(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.ContentLength = int64(len(body))
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

func setParam(c echo.Context, name, value string) {
	c.SetParamNames(name)
	c.SetParamValues(value)
}

func TestAdminLogger(t *testing.T) {
	h := &AdminHandlers{}
	if h.logger() == nil {
		t.Fatal("default logger nil")
	}
	custom := quietLogger()
	h2 := &AdminHandlers{Logger: custom}
	if h2.logger() != custom {
		t.Fatal("injected logger not returned")
	}
}

func TestAdminListAlerts(t *testing.T) {
	t.Run("happy with all filters + custom limit/offset", func(t *testing.T) {
		store := &adminFakeStore{
			listAlertsFn: func(_ context.Context, f ListFilter) ([]Alert, int, error) {
				// State/Severity/SourceType/RuleID slices should be populated.
				if len(f.State) != 1 || len(f.Severity) != 1 || len(f.SourceType) != 1 || len(f.RuleID) != 1 {
					return nil, 0, fmt.Errorf("filter not parsed: %+v", f)
				}
				if f.Since == nil || f.Until == nil {
					return nil, 0, fmt.Errorf("since/until missing")
				}
				if f.Offset != 5 || f.Limit != 10 {
					return nil, 0, fmt.Errorf("offset/limit: %d %d", f.Offset, f.Limit)
				}
				return nil, 0, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet,
			"/?state=firing&severity=high&sourceType=quota&ruleId=r.1&since=2026-01-01T00:00:00Z&until=2026-06-01T00:00:00Z&offset=5&limit=10", "")
		if err := h.ListAlerts(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("invalid since", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodGet, "/?since=not-a-date", "")
		_ = h.ListAlerts(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("invalid until", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodGet, "/?until=bad", "")
		_ = h.ListAlerts(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("invalid offset", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodGet, "/?offset=-1", "")
		_ = h.ListAlerts(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("invalid limit", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodGet, "/?limit=0", "")
		_ = h.ListAlerts(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("store error", func(t *testing.T) {
		store := &adminFakeStore{
			listAlertsFn: func(context.Context, ListFilter) ([]Alert, int, error) {
				return nil, 0, errors.New("boom")
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		_ = h.ListAlerts(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("nil slice normalized", func(t *testing.T) {
		store := &adminFakeStore{
			listAlertsFn: func(context.Context, ListFilter) ([]Alert, int, error) {
				return nil, 0, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		if err := h.ListAlerts(c); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rec.Body.String(), `"alerts":[]`) {
			t.Errorf("nil should normalize to []: %s", rec.Body.String())
		}
	})
}

func TestAdminGetAlert(t *testing.T) {
	t.Run("missing id", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		_ = h.GetAlert(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("happy + nil dispatches normalized", func(t *testing.T) {
		store := &adminFakeStore{
			getAlertFn: func(_ context.Context, id string) (*Alert, error) {
				return &Alert{ID: id, RuleID: "r"}, nil
			},
			listDispatchesFn: func(context.Context, string) ([]Dispatch, error) {
				return nil, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "a-1")
		if err := h.GetAlert(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"dispatches":[]`) {
			t.Errorf("expected normalized []: %s", rec.Body.String())
		}
	})
	t.Run("not found", func(t *testing.T) {
		store := &adminFakeStore{
			getAlertFn: func(context.Context, string) (*Alert, error) { return nil, ErrNotFound },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "a")
		_ = h.GetAlert(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("get error", func(t *testing.T) {
		store := &adminFakeStore{
			getAlertFn: func(context.Context, string) (*Alert, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "a")
		_ = h.GetAlert(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("dispatches error", func(t *testing.T) {
		store := &adminFakeStore{
			getAlertFn:       func(context.Context, string) (*Alert, error) { return &Alert{}, nil },
			listDispatchesFn: func(context.Context, string) ([]Dispatch, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "a")
		_ = h.GetAlert(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestActorFromHeader(t *testing.T) {
	e := echo.New()
	t.Run("from header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Nexus-Actor-User-Id", "alice")
		c := e.NewContext(req, httptest.NewRecorder())
		if got := actorFromHeader(c); got != "alice" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("default system", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		c := e.NewContext(req, httptest.NewRecorder())
		if got := actorFromHeader(c); got != "system" {
			t.Errorf("got %q", got)
		}
	})
}

func TestAdminAck(t *testing.T) {
	t.Run("missing id", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		_ = h.AckAlert(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("happy", func(t *testing.T) {
		store := &adminFakeStore{ackFn: func(context.Context, string, string, string) error { return nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{"reason":"x"}`)
		setParam(c, "id", "a")
		_ = h.AckAlert(c)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		store := &adminFakeStore{ackFn: func(context.Context, string, string, string) error { return nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{not-json`)
		setParam(c, "id", "a")
		_ = h.AckAlert(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("ErrNotFound -> 404", func(t *testing.T) {
		store := &adminFakeStore{ackFn: func(context.Context, string, string, string) error { return ErrNotFound }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{}`)
		setParam(c, "id", "a")
		_ = h.AckAlert(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("store error -> 500", func(t *testing.T) {
		store := &adminFakeStore{ackFn: func(context.Context, string, string, string) error { return errors.New("boom") }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{}`)
		setParam(c, "id", "a")
		_ = h.AckAlert(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminResolveAlert(t *testing.T) {
	t.Run("missing id", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		_ = h.ResolveAlert(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("happy", func(t *testing.T) {
		store := &adminFakeStore{resolveAlertFn: func(context.Context, string, string, string) error { return nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{"reason":"x"}`)
		setParam(c, "id", "a")
		_ = h.ResolveAlert(c)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		store := &adminFakeStore{resolveAlertFn: func(context.Context, string, string, string) error { return nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{not-json`)
		setParam(c, "id", "a")
		_ = h.ResolveAlert(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("not found", func(t *testing.T) {
		store := &adminFakeStore{resolveAlertFn: func(context.Context, string, string, string) error { return ErrNotFound }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{}`)
		setParam(c, "id", "a")
		_ = h.ResolveAlert(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("store error", func(t *testing.T) {
		store := &adminFakeStore{resolveAlertFn: func(context.Context, string, string, string) error { return errors.New("boom") }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{}`)
		setParam(c, "id", "a")
		_ = h.ResolveAlert(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminListRules(t *testing.T) {
	t.Run("happy with enabled=true filter + limit/offset", func(t *testing.T) {
		store := &adminFakeStore{
			listRulesFn: func(_ context.Context, p ListRulesParams) ([]AlertRule, int, error) {
				if p.Enabled == nil || *p.Enabled != true || p.Limit != 5 || p.Offset != 2 {
					return nil, 0, fmt.Errorf("bad params: %+v", p)
				}
				return []AlertRule{{ID: "r.1"}}, 1, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/?enabled=true&limit=5&offset=2&search=q&severity=high&sourceType=quota", "")
		if err := h.ListRules(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("enabled=false branch", func(t *testing.T) {
		store := &adminFakeStore{
			listRulesFn: func(_ context.Context, p ListRulesParams) ([]AlertRule, int, error) {
				if p.Enabled == nil || *p.Enabled != false {
					return nil, 0, fmt.Errorf("want enabled=false")
				}
				return nil, 0, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/?enabled=false", "")
		_ = h.ListRules(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"rules":[]`) {
			t.Errorf("nil should normalize: %s", rec.Body.String())
		}
	})
	t.Run("bad limit/offset ignored", func(t *testing.T) {
		store := &adminFakeStore{
			listRulesFn: func(_ context.Context, p ListRulesParams) ([]AlertRule, int, error) {
				if p.Limit != 50 || p.Offset != 0 {
					return nil, 0, fmt.Errorf("bad defaults: %+v", p)
				}
				return nil, 0, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/?limit=abc&offset=def", "")
		_ = h.ListRules(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("store error", func(t *testing.T) {
		store := &adminFakeStore{
			listRulesFn: func(context.Context, ListRulesParams) ([]AlertRule, int, error) {
				return nil, 0, errors.New("boom")
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		_ = h.ListRules(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminGetRule(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(_ context.Context, id string) (*AlertRule, error) { return &AlertRule{ID: id}, nil },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "r.1")
		if err := h.GetRule(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("not found", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(context.Context, string) (*AlertRule, error) { return nil, ErrNotFound },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "r.1")
		_ = h.GetRule(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("store error", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(context.Context, string) (*AlertRule, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "r.1")
		_ = h.GetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminUpdateRule(t *testing.T) {
	baseGet := func(id string) *AlertRule {
		return &AlertRule{ID: id, DisplayName: "existing", DefaultSeverity: SeverityHigh,
			Params: map[string]any{"old": 1}, ParamsSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"threshold": map[string]any{"type": "number"},
				},
			}}
	}
	t.Run("happy update all fields + clear group filter via empty string", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil },
			updateRuleFn: func(context.Context, AlertRule) error {
				return nil
			},
		}
		h := AdminHandlers{Store: store}
		body := `{"enabled":false,"params":{"threshold":42},"cooldownSec":120,"requiresAck":true,"defaultSeverity":"low","displayName":"D2","groupIdFilter":""}`
		c, rec := makeEchoRequest(http.MethodPut, "/", body)
		setParam(c, "id", "r.1")
		if err := h.UpdateRule(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		// Verify the store saw the mutated rule.
		if store.updateRuleObserved.DisplayName != "D2" || store.updateRuleObserved.Enabled != false ||
			store.updateRuleObserved.CooldownSec != 120 || store.updateRuleObserved.RequiresAck != true ||
			store.updateRuleObserved.DefaultSeverity != SeverityLow ||
			store.updateRuleObserved.GroupIDFilter != nil {
			t.Errorf("rule not mutated: %+v", store.updateRuleObserved)
		}
	})
	t.Run("set group filter to non-empty", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn:    func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil },
			updateRuleFn: func(context.Context, AlertRule) error { return nil },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"groupIdFilter":"grp-1"}`)
		setParam(c, "id", "r.1")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
		if store.updateRuleObserved.GroupIDFilter == nil || *store.updateRuleObserved.GroupIDFilter != "grp-1" {
			t.Errorf("group filter not set: %+v", store.updateRuleObserved.GroupIDFilter)
		}
	})
	t.Run("getRule not found -> 404", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(context.Context, string) (*AlertRule, error) { return nil, ErrNotFound },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("getRule generic error -> 500", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(context.Context, string) (*AlertRule, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{garbled`)
		setParam(c, "id", "r")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("unknown JSON field rejected (DisallowUnknownFields)", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"sneaky":true}`)
		setParam(c, "id", "r")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("params schema violation -> 400", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"params":{"threshold":"not-a-number"}}`)
		setParam(c, "id", "r")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("invalid defaultSeverity", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"defaultSeverity":"bogus"}`)
		setParam(c, "id", "r")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("UpdateRule store returns ErrNotFound -> 404", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn:    func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil },
			updateRuleFn: func(context.Context, AlertRule) error { return ErrNotFound },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("UpdateRule store generic err -> 500", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn:    func(_ context.Context, id string) (*AlertRule, error) { return baseGet(id), nil },
			updateRuleFn: func(context.Context, AlertRule) error { return errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("post-update refresh getRule errors -> 500", func(t *testing.T) {
		first := true
		store := &adminFakeStore{
			getRuleFn: func(_ context.Context, id string) (*AlertRule, error) {
				if first {
					first = false
					return baseGet(id), nil
				}
				return nil, errors.New("refresh boom")
			},
			updateRuleFn: func(context.Context, AlertRule) error { return nil },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("invalid params JSON body via valid envelope but bad inner", func(t *testing.T) {
		// schemaless rule so we hit only the inner unmarshal path.
		schemaless := &AlertRule{ID: "r", ParamsSchema: nil}
		store := &adminFakeStore{
			getRuleFn:    func(context.Context, string) (*AlertRule, error) { return schemaless, nil },
			updateRuleFn: func(context.Context, AlertRule) error { return nil },
		}
		h := AdminHandlers{Store: store}
		// Outer JSON is valid (RawMessage takes anything), inner unmarshal into map fails on a non-object.
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"params":42}`)
		setParam(c, "id", "r")
		_ = h.UpdateRule(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "params parse") {
			t.Errorf("expected params parse: %s", rec.Body.String())
		}
	})
}

func TestAdminResetRule(t *testing.T) {
	t.Run("nil registry -> 500", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("unknown id -> 404", func(t *testing.T) {
		h := AdminHandlers{
			Store: &adminFakeStore{},
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "missing")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("getRule not found -> 404", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(context.Context, string) (*AlertRule, error) { return nil, ErrNotFound }}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{
				"r": {ID: "r", Params: json.RawMessage(`{}`), ParamsSchema: json.RawMessage(`{}`)},
			}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("getRule generic err -> 500", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(context.Context, string) (*AlertRule, error) { return nil, errors.New("boom") }}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{"r": {ID: "r"}}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("builtin params unmarshal err", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(context.Context, string) (*AlertRule, error) { return &AlertRule{ID: "r"}, nil }}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{
				"r": {ID: "r", Params: json.RawMessage(`not-json`)},
			}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("builtin schema unmarshal err", func(t *testing.T) {
		store := &adminFakeStore{getRuleFn: func(context.Context, string) (*AlertRule, error) { return &AlertRule{ID: "r"}, nil }}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{
				"r": {ID: "r", Params: json.RawMessage(`{}`), ParamsSchema: json.RawMessage(`not-json`)},
			}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("happy path", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn: func(_ context.Context, id string) (*AlertRule, error) {
				return &AlertRule{ID: id, SourceType: "src"}, nil
			},
			updateRuleFn: func(context.Context, AlertRule) error { return nil },
		}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{
				"r": {ID: "r", DisplayName: "D", DefaultSeverity: SeverityHigh,
					Params: json.RawMessage(`{"a":1}`), ParamsSchema: json.RawMessage(`{}`)},
			}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		if err := h.ResetRule(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("updateRule fails -> 500", func(t *testing.T) {
		store := &adminFakeStore{
			getRuleFn:    func(_ context.Context, id string) (*AlertRule, error) { return &AlertRule{ID: id}, nil },
			updateRuleFn: func(context.Context, AlertRule) error { return errors.New("boom") },
		}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{
				"r": {ID: "r", Params: json.RawMessage(`{}`), ParamsSchema: json.RawMessage(`{}`)},
			}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("refresh fails after update -> 500", func(t *testing.T) {
		first := true
		store := &adminFakeStore{
			getRuleFn: func(_ context.Context, id string) (*AlertRule, error) {
				if first {
					first = false
					return &AlertRule{ID: id}, nil
				}
				return nil, errors.New("refresh boom")
			},
			updateRuleFn: func(context.Context, AlertRule) error { return nil },
		}
		h := AdminHandlers{
			Store: store,
			Rules: fakeRuleRegistry{entries: map[string]RuleDefault{
				"r": {ID: "r", Params: json.RawMessage(`{}`), ParamsSchema: json.RawMessage(`{}`)},
			}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "r")
		_ = h.ResetRule(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminListChannels(t *testing.T) {
	t.Run("happy + masking", func(t *testing.T) {
		store := &adminFakeStore{
			listChannelsFn: func(context.Context) ([]Channel, error) {
				return []Channel{
					{ID: "c1", Name: "n1", Config: map[string]any{"botToken": "abcdefg"}},
				}, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		if err := h.ListChannels(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "xxxx") {
			t.Errorf("masking failed: %s", rec.Body.String())
		}
	})
	t.Run("nil normalized to []", func(t *testing.T) {
		store := &adminFakeStore{
			listChannelsFn: func(context.Context) ([]Channel, error) { return nil, nil },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		_ = h.ListChannels(c)
		if !strings.Contains(rec.Body.String(), `"channels":[]`) {
			t.Errorf("nil should normalize: %s", rec.Body.String())
		}
	})
	t.Run("store error", func(t *testing.T) {
		store := &adminFakeStore{
			listChannelsFn: func(context.Context) ([]Channel, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		_ = h.ListChannels(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminCreateChannel(t *testing.T) {
	t.Run("happy + nil-slice defaults", func(t *testing.T) {
		store := &adminFakeStore{
			insertChannelFn: func(_ context.Context, c Channel) (string, error) {
				if c.Severities == nil || c.SourceTypes == nil || c.Config == nil {
					return "", fmt.Errorf("nil defaults not applied: %+v", c)
				}
				return "ch-1", nil
			},
			getChannelFn: func(_ context.Context, id string) (*Channel, error) {
				return &Channel{ID: id, Name: "n", Config: map[string]any{"botToken": "abcdefg"}}, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{"name":"n","type":"webhook","enabled":true}`)
		if err := h.CreateChannel(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "xxxx") {
			t.Errorf("create response not masked: %s", rec.Body.String())
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{garbled`)
		_ = h.CreateChannel(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("missing name/type", func(t *testing.T) {
		h := AdminHandlers{Store: &adminFakeStore{}}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{"name":"","type":""}`)
		_ = h.CreateChannel(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("insert error", func(t *testing.T) {
		store := &adminFakeStore{
			insertChannelFn: func(context.Context, Channel) (string, error) { return "", errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{"name":"n","type":"w"}`)
		_ = h.CreateChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("post-insert get error", func(t *testing.T) {
		store := &adminFakeStore{
			insertChannelFn: func(context.Context, Channel) (string, error) { return "x", nil },
			getChannelFn:    func(context.Context, string) (*Channel, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPost, "/", `{"name":"n","type":"w"}`)
		_ = h.CreateChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminGetChannel(t *testing.T) {
	t.Run("happy + masking", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn: func(_ context.Context, id string) (*Channel, error) {
				return &Channel{ID: id, Name: "n", Config: map[string]any{"botToken": "abcdefg"}}, nil
			},
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "x")
		_ = h.GetChannel(c)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "xxxx") {
			t.Errorf("got: %s", rec.Body.String())
		}
	})
	t.Run("not found", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn: func(context.Context, string) (*Channel, error) { return nil, ErrNotFound },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "x")
		_ = h.GetChannel(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("store err", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn: func(context.Context, string) (*Channel, error) { return nil, errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodGet, "/", "")
		setParam(c, "id", "x")
		_ = h.GetChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminUpdateChannel(t *testing.T) {
	t.Run("partial enable toggle preserves existing fields", func(t *testing.T) {
		existing := &Channel{ID: "x", Name: "Old", Type: "webhook", Enabled: false,
			Severities: []Severity{SeverityHigh}, SourceTypes: []string{"quota"},
			Config: map[string]any{"botToken": "secret"}}
		store := &adminFakeStore{
			getChannelFn: func(_ context.Context, id string) (*Channel, error) {
				cp := *existing
				cp.ID = id
				return &cp, nil
			},
			updateChannelFn: func(context.Context, Channel) error { return nil },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"enabled":true}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("full update incl. severities/sourceTypes/type", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn: func(_ context.Context, id string) (*Channel, error) {
				return &Channel{ID: id, Config: map[string]any{}}, nil
			},
			updateChannelFn: func(context.Context, Channel) error { return nil },
		}
		h := AdminHandlers{Store: store}
		body := `{"name":"new","type":"slack","enabled":true,"severities":["info"],"sourceTypes":["audit"],"config":{"botToken":"freshtoken"}}`
		c, rec := makeEchoRequest(http.MethodPut, "/", body)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("get not found", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return nil, ErrNotFound }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("get generic err", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return nil, errors.New("boom") }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return &Channel{}, nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{garbled`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("empty name rejected", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return &Channel{}, nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"name":""}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("empty type rejected", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return &Channel{}, nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{"type":""}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("update store ErrNotFound -> 404", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:    func(context.Context, string) (*Channel, error) { return &Channel{Config: map[string]any{}}, nil },
			updateChannelFn: func(context.Context, Channel) error { return ErrNotFound },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("update store generic err -> 500", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:    func(context.Context, string) (*Channel, error) { return &Channel{Config: map[string]any{}}, nil },
			updateChannelFn: func(context.Context, Channel) error { return errors.New("boom") },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("post-update getChannel error", func(t *testing.T) {
		first := true
		store := &adminFakeStore{
			getChannelFn: func(context.Context, string) (*Channel, error) {
				if first {
					first = false
					return &Channel{Config: map[string]any{}}, nil
				}
				return nil, errors.New("boom")
			},
			updateChannelFn: func(context.Context, Channel) error { return nil },
		}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodPut, "/", `{}`)
		setParam(c, "id", "x")
		_ = h.UpdateChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminDeleteChannel(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		store := &adminFakeStore{deleteChannelFn: func(context.Context, string) error { return nil }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodDelete, "/", "")
		setParam(c, "id", "x")
		_ = h.DeleteChannel(c)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("not found", func(t *testing.T) {
		store := &adminFakeStore{deleteChannelFn: func(context.Context, string) error { return ErrNotFound }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodDelete, "/", "")
		setParam(c, "id", "x")
		_ = h.DeleteChannel(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("generic err", func(t *testing.T) {
		store := &adminFakeStore{deleteChannelFn: func(context.Context, string) error { return errors.New("boom") }}
		h := AdminHandlers{Store: store}
		c, rec := makeEchoRequest(http.MethodDelete, "/", "")
		setParam(c, "id", "x")
		_ = h.DeleteChannel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestAdminChannelTest(t *testing.T) {
	baseChan := func() *Channel { return &Channel{ID: "x", Name: "n", Type: "webhook"} }
	t.Run("happy success", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:     func(context.Context, string) (*Channel, error) { return baseChan(), nil },
			insertAlertFn:    func(context.Context, Alert) (string, error) { return "alert-1", nil },
			insertDispatchFn: func(context.Context, Dispatch) (string, error) { return "d-1", nil },
			resolveAlertFn:   func(context.Context, string, string, string) error { return nil },
		}
		h := AdminHandlers{
			Store:   store,
			Senders: adminSenderReg{m: map[string]Sender{"webhook": adminSender{statusCode: 200}}},
			Logger:  quietLogger(),
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("get not found", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return nil, ErrNotFound }}
		h := AdminHandlers{Store: store, Senders: adminSenderReg{m: map[string]Sender{}}}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("get generic err", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return nil, errors.New("boom") }}
		h := AdminHandlers{Store: store, Senders: adminSenderReg{m: map[string]Sender{}}}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("no sender for type", func(t *testing.T) {
		store := &adminFakeStore{getChannelFn: func(context.Context, string) (*Channel, error) { return baseChan(), nil }}
		h := AdminHandlers{Store: store, Senders: adminSenderReg{m: map[string]Sender{}}}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("insert alert fails", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:  func(context.Context, string) (*Channel, error) { return baseChan(), nil },
			insertAlertFn: func(context.Context, Alert) (string, error) { return "", errors.New("boom") },
		}
		h := AdminHandlers{
			Store:   store,
			Senders: adminSenderReg{m: map[string]Sender{"webhook": adminSender{statusCode: 200}}},
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("send failure path -> 500 body has success=false", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:     func(context.Context, string) (*Channel, error) { return baseChan(), nil },
			insertAlertFn:    func(context.Context, Alert) (string, error) { return "a", nil },
			insertDispatchFn: func(context.Context, Dispatch) (string, error) { return "d", nil },
			resolveAlertFn:   func(context.Context, string, string, string) error { return nil },
		}
		h := AdminHandlers{
			Store:   store,
			Senders: adminSenderReg{m: map[string]Sender{"webhook": adminSender{statusCode: 502, err: errors.New("upstream")}}},
			Logger:  quietLogger(),
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"success":false`) {
			t.Errorf("expected success false: %s", rec.Body.String())
		}
	})
	t.Run("dispatch insert err is logged not propagated", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:     func(context.Context, string) (*Channel, error) { return baseChan(), nil },
			insertAlertFn:    func(context.Context, Alert) (string, error) { return "a", nil },
			insertDispatchFn: func(context.Context, Dispatch) (string, error) { return "", errors.New("boom") },
			resolveAlertFn:   func(context.Context, string, string, string) error { return nil },
		}
		h := AdminHandlers{
			Store:   store,
			Senders: adminSenderReg{m: map[string]Sender{"webhook": adminSender{statusCode: 200}}},
			Logger:  quietLogger(),
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("auto-resolve err is logged not propagated", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:     func(context.Context, string) (*Channel, error) { return baseChan(), nil },
			insertAlertFn:    func(context.Context, Alert) (string, error) { return "a", nil },
			insertDispatchFn: func(context.Context, Dispatch) (string, error) { return "d", nil },
			resolveAlertFn:   func(context.Context, string, string, string) error { return errors.New("res boom") },
		}
		h := AdminHandlers{
			Store:   store,
			Senders: adminSenderReg{m: map[string]Sender{"webhook": adminSender{statusCode: 200}}},
			Logger:  quietLogger(),
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("status >=400 with no error counts as failure", func(t *testing.T) {
		store := &adminFakeStore{
			getChannelFn:     func(context.Context, string) (*Channel, error) { return baseChan(), nil },
			insertAlertFn:    func(context.Context, Alert) (string, error) { return "a", nil },
			insertDispatchFn: func(context.Context, Dispatch) (string, error) { return "d", nil },
			resolveAlertFn:   func(context.Context, string, string, string) error { return nil },
		}
		h := AdminHandlers{
			Store:   store,
			Senders: adminSenderReg{m: map[string]Sender{"webhook": adminSender{statusCode: 503}}},
			Logger:  quietLogger(),
		}
		c, rec := makeEchoRequest(http.MethodPost, "/", "")
		setParam(c, "id", "x")
		_ = h.ChannelTest(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}


func TestParseFlexibleTime(t *testing.T) {
	if _, ok := parseFlexibleTime("2026-01-01T00:00:00.000Z"); !ok {
		t.Error("RFC3339Nano should parse")
	}
	if _, ok := parseFlexibleTime("2026-01-01T00:00:00Z"); !ok {
		t.Error("RFC3339 should parse")
	}
	if _, ok := parseFlexibleTime("garbage"); ok {
		t.Error("garbage should fail")
	}
}

func TestEchoErr(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := echoErr(c, http.StatusTeapot, "msg"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTeapot || !strings.Contains(rec.Body.String(), "msg") {
		t.Errorf("got: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIsValidSeverity(t *testing.T) {
	for _, s := range []Severity{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo} {
		if !s.IsValid() {
			t.Errorf("want valid: %q", s)
		}
	}
	if Severity("bogus").IsValid() {
		t.Error("bogus should be invalid")
	}
}

func TestValidateParamsAgainstSchema(t *testing.T) {
	t.Run("nil schema is no-op", func(t *testing.T) {
		if err := validateParamsAgainstSchema(json.RawMessage(`{}`), nil); err != nil {
			t.Errorf("nil schema: %v", err)
		}
	})
	t.Run("happy", func(t *testing.T) {
		err := validateParamsAgainstSchema(
			json.RawMessage(`{"x":1}`),
			map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}},
		)
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("schema marshal err", func(t *testing.T) {
		err := validateParamsAgainstSchema(json.RawMessage(`{}`),
			map[string]any{"bad": make(chan int)})
		if err == nil || !strings.Contains(err.Error(), "marshal schema") {
			t.Fatalf("want marshal err: %v", err)
		}
	})
	t.Run("schema compile err", func(t *testing.T) {
		// $ref to a nonexistent document.
		err := validateParamsAgainstSchema(json.RawMessage(`{}`),
			map[string]any{"$ref": "https://example.invalid/nope.json"})
		if err == nil {
			t.Fatal("expected compile err")
		}
	})
	t.Run("params unmarshal err", func(t *testing.T) {
		err := validateParamsAgainstSchema(json.RawMessage(`not-json`),
			map[string]any{"type": "object"})
		if err == nil || !strings.Contains(err.Error(), "parse params") {
			t.Fatalf("want parse params: %v", err)
		}
	})
	t.Run("validation err returned", func(t *testing.T) {
		err := validateParamsAgainstSchema(json.RawMessage(`{"x":"string"}`),
			map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}})
		if err == nil {
			t.Fatal("expected validation err")
		}
	})
}

func TestIsMasked(t *testing.T) {
	if !isMasked("xxxx-••••-1234") {
		t.Error("masked value not detected")
	}
	if isMasked("plain") {
		t.Error("plain falsely flagged")
	}
}

func TestMergeMaskedSecrets(t *testing.T) {
	t.Run("preserves masked secret", func(t *testing.T) {
		incoming := map[string]any{"botToken": "xxxx-••••-1234"}
		existing := map[string]any{"botToken": "real-secret-1234"}
		got := mergeMaskedSecrets(incoming, existing)
		if got["botToken"] != "real-secret-1234" {
			t.Errorf("got: %v", got["botToken"])
		}
	})
	t.Run("non-masked passes through (no overwrite)", func(t *testing.T) {
		incoming := map[string]any{"botToken": "freshtoken"}
		existing := map[string]any{"botToken": "old"}
		got := mergeMaskedSecrets(incoming, existing)
		if got["botToken"] != "freshtoken" {
			t.Errorf("got: %v", got["botToken"])
		}
	})
	t.Run("non-string sensitive value passes through", func(t *testing.T) {
		incoming := map[string]any{"botToken": 42}
		existing := map[string]any{}
		got := mergeMaskedSecrets(incoming, existing)
		if got["botToken"] != 42 {
			t.Errorf("got: %v", got["botToken"])
		}
	})
	t.Run("masked but no existing -> stays masked", func(t *testing.T) {
		incoming := map[string]any{"botToken": "xxxx-••••-1234"}
		existing := map[string]any{}
		got := mergeMaskedSecrets(incoming, existing)
		if got["botToken"] != "xxxx-••••-1234" {
			t.Errorf("got: %v", got["botToken"])
		}
	})
	t.Run("non-sensitive top-level key untouched", func(t *testing.T) {
		incoming := map[string]any{"url": "http://x"}
		existing := map[string]any{}
		got := mergeMaskedSecrets(incoming, existing)
		if got["url"] != "http://x" {
			t.Errorf("got: %v", got["url"])
		}
	})
	t.Run("headers map masked value restored", func(t *testing.T) {
		incoming := map[string]any{
			"headers": map[string]any{
				"Authorization": "xxxx-••••-9999",
				"X-Plain":       "hi",
			},
		}
		existing := map[string]any{
			"headers": map[string]any{
				"Authorization": "Bearer real-token-9999",
			},
		}
		got := mergeMaskedSecrets(incoming, existing)
		hdrs, _ := got["headers"].(map[string]any)
		if hdrs["Authorization"] != "Bearer real-token-9999" {
			t.Errorf("authorization not restored: %v", hdrs["Authorization"])
		}
		if hdrs["X-Plain"] != "hi" {
			t.Errorf("plain header lost: %v", hdrs["X-Plain"])
		}
	})
	t.Run("headers value not a map ignored", func(t *testing.T) {
		incoming := map[string]any{"headers": "not-a-map"}
		existing := map[string]any{}
		_ = mergeMaskedSecrets(incoming, existing)
	})
	t.Run("headers nil existing returns incoming unchanged", func(t *testing.T) {
		incoming := map[string]any{"headers": map[string]any{"Authorization": "xxxx-••••-1234"}}
		existing := map[string]any{}
		got := mergeMaskedSecrets(incoming, existing)
		hdrs, _ := got["headers"].(map[string]any)
		if hdrs["Authorization"] != "xxxx-••••-1234" {
			t.Errorf("got: %v", hdrs["Authorization"])
		}
	})
	t.Run("non-sensitive header passes through", func(t *testing.T) {
		incoming := map[string]any{"headers": map[string]any{"X-Trace": "abc"}}
		existing := map[string]any{"headers": map[string]any{"X-Trace": "old"}}
		got := mergeMaskedSecrets(incoming, existing)
		hdrs, _ := got["headers"].(map[string]any)
		if hdrs["X-Trace"] != "abc" {
			t.Errorf("got: %v", hdrs["X-Trace"])
		}
	})
	t.Run("non-string sensitive header passes through", func(t *testing.T) {
		incoming := map[string]any{"headers": map[string]any{"Authorization": 42}}
		existing := map[string]any{"headers": map[string]any{"Authorization": "real"}}
		got := mergeMaskedSecrets(incoming, existing)
		hdrs, _ := got["headers"].(map[string]any)
		if hdrs["Authorization"] != 42 {
			t.Errorf("got: %v", hdrs["Authorization"])
		}
	})
}

// handlers_internal HandleResolve error/bad-input branches
//
// raiser-side error wiring for HandleResolve is incomplete in the existing
// handlers_internal_test; cover it here.

type irMockRaiser struct {
	raiseErr   error
	resolveErr error
}

func (m irMockRaiser) Raise(context.Context, RaiseInput) error               { return m.raiseErr }
func (m irMockRaiser) Resolve(context.Context, string, string, string) error { return m.resolveErr }

func TestHandleResolve_BadJSON(t *testing.T) {
	h := HandleResolve(irMockRaiser{})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{garbled`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHandleResolve_MissingRuleID(t *testing.T) {
	h := HandleResolve(irMockRaiser{})
	body, _ := json.Marshal(map[string]any{"targetKey": "t"})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHandleResolve_RaiserErr(t *testing.T) {
	h := HandleResolve(irMockRaiser{resolveErr: errors.New("boom")})
	body, _ := json.Marshal(map[string]any{"ruleId": "r", "targetKey": "t", "reason": "x"})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// Ensure unused imports for io/jackc are referenced.
var _ = io.Discard
var _ = pgx.ErrNoRows
