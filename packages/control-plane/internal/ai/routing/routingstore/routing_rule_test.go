package routingstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// rrCols mirrors the 13-column rrColumns SELECT list in routing_rule.go.
var rrCols = []string{
	"id", "name", "description", "strategyType", "config", "matchConditions",
	"priority", "pipelineStage", "fallbackChain", "retryPolicy",
	"enabled", "createdAt", "updatedAt",
}

func sampleRow(id, name string, now time.Time) []any {
	desc := "test"
	return []any{
		id, name, &desc, "single",
		json.RawMessage(`{"providerId":"p","modelId":"m"}`),
		json.RawMessage(`{}`),
		0, 1,
		json.RawMessage(`[]`),
		nil, // retryPolicy → NULL
		true,
		now, now,
	}
}

func newStoreWithMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return New(mock), mock
}

// anyN returns N pgxmock.AnyArg matchers so WithArgs can match arg counts
// without locking in exact values.
func anyN(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// Pure helpers

func TestNew(t *testing.T) {
	s := New(nil)
	if s == nil {
		t.Fatal("New(nil) returned nil store")
	}
}

func TestEscapeILIKE(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"plain":      "plain",
		"with%pct":   `with\%pct`,
		"with_under": `with\_under`,
		"with\\back": `with\\back`,
		"all_%\\mix": `all\_\%\\mix`,
	}
	for in, want := range cases {
		if got := escapeILIKE(in); got != want {
			t.Errorf("escapeILIKE(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestJsonOrNil(t *testing.T) {
	if got := jsonOrNil(nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := jsonOrNil(json.RawMessage(``)); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
	if got := jsonOrNil(json.RawMessage(`null`)); got != nil {
		t.Errorf("\"null\" input: got %v, want nil", got)
	}
	raw := json.RawMessage(`{"foo":1}`)
	got := jsonOrNil(raw)
	if got == nil {
		t.Error("non-empty input: got nil, want passthrough")
	}
}

func TestListRoutingRules_Happy_NoFilters(t *testing.T) {
	s, mock := newStoreWithMock(t)
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "RoutingRule"`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule"`).
		WithArgs(10, 0).
		WillReturnRows(
			pgxmock.NewRows(rrCols).
				AddRow(sampleRow("r1", "rule-1", now)...).
				AddRow(sampleRow("r2", "rule-2", now)...),
		)

	rules, total, err := s.ListRoutingRules(context.Background(), RoutingRuleListParams{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if total != 2 {
		t.Errorf("total: got %d, want 2", total)
	}
	if len(rules) != 2 || rules[0].ID != "r1" || rules[1].ID != "r2" {
		t.Errorf("rules: %+v", rules)
	}
}

func TestListRoutingRules_EnabledFilter(t *testing.T) {
	s, mock := newStoreWithMock(t)
	enabled := true
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "RoutingRule" WHERE 1=1  AND enabled = \$1`).
		WithArgs(true).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE 1=1  AND enabled = \$1`).
		WithArgs(true, 10, 0).
		WillReturnRows(pgxmock.NewRows(rrCols))

	_, total, err := s.ListRoutingRules(context.Background(), RoutingRuleListParams{
		Enabled: &enabled, Limit: 10, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if total != 0 {
		t.Errorf("total: got %d, want 0", total)
	}
}

func TestListRoutingRules_StrategyAndQFilters(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`COUNT.*"strategyType" = \$1.*name ILIKE \$2 OR description ILIKE \$2`).
		WithArgs("smart", "%foo%").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule"`).
		WithArgs("smart", "%foo%", 5, 0).
		WillReturnRows(pgxmock.NewRows(rrCols).AddRow(sampleRow("r3", "rule-3", time.Now())...))

	rules, total, err := s.ListRoutingRules(context.Background(), RoutingRuleListParams{
		StrategyType: "smart", Q: "foo", Limit: 5, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if total != 1 || len(rules) != 1 || rules[0].ID != "r3" {
		t.Errorf("unexpected result total=%d rules=%+v", total, rules)
	}
}

func TestListRoutingRules_CountError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))

	_, _, err := s.ListRoutingRules(context.Background(), RoutingRuleListParams{Limit: 10})
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestListRoutingRules_QueryError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`COUNT`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule"`).WillReturnError(errors.New("query boom"))

	_, _, err := s.ListRoutingRules(context.Background(), RoutingRuleListParams{Limit: 10})
	if err == nil {
		t.Fatal("want error from Query")
	}
}

func TestListRoutingRules_ScanError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`COUNT`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	// One column short → scan fails.
	short := []string{"id", "name"}
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule"`).
		WillReturnRows(pgxmock.NewRows(short).AddRow("r1", "rule-1"))

	_, _, err := s.ListRoutingRules(context.Background(), RoutingRuleListParams{Limit: 10})
	if err == nil {
		t.Fatal("want scan error")
	}
}

func TestGetRoutingRule_Happy(t *testing.T) {
	s, mock := newStoreWithMock(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("r1").
		WillReturnRows(pgxmock.NewRows(rrCols).AddRow(sampleRow("r1", "rule-1", now)...))

	r, err := s.GetRoutingRule(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r == nil || r.ID != "r1" {
		t.Errorf("got %+v", r)
	}
}

func TestGetRoutingRule_NotFound(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	r, err := s.GetRoutingRule(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r != nil {
		t.Errorf("expected nil; got %+v", r)
	}
}

func TestGetRoutingRule_DBError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`WHERE id = \$1`).
		WithArgs("r1").
		WillReturnError(errors.New("query boom"))

	_, err := s.GetRoutingRule(context.Background(), "r1")
	if err == nil {
		t.Fatal("want DB error")
	}
}

func TestCreateRoutingRule_Happy(t *testing.T) {
	s, mock := newStoreWithMock(t)
	now := time.Now()
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(rrCols).AddRow(sampleRow("new-id", "new-rule", now)...))

	r, err := s.CreateRoutingRule(context.Background(), CreateRoutingRuleParams{
		Name:          "new-rule",
		StrategyType:  "single",
		Config:        json.RawMessage(`{}`),
		Priority:      0,
		PipelineStage: 1,
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r == nil || r.ID != "new-id" {
		t.Errorf("got %+v", r)
	}
}

func TestCreateRoutingRule_DBError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnError(errors.New("constraint violation"))

	_, err := s.CreateRoutingRule(context.Background(), CreateRoutingRuleParams{Name: "x"})
	if err == nil {
		t.Fatal("want DB error")
	}
}

func TestUpdateRoutingRule_NoRetryPolicyChange(t *testing.T) {
	s, mock := newStoreWithMock(t)
	now := time.Now()
	name := "renamed"
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(rrCols).AddRow(sampleRow("r1", "renamed", now)...))

	r, err := s.UpdateRoutingRule(context.Background(), "r1", UpdateRoutingRuleParams{Name: &name})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if r == nil || r.Name != "renamed" {
		t.Errorf("got %+v", r)
	}
}

func TestUpdateRoutingRule_WithRetryPolicy(t *testing.T) {
	s, mock := newStoreWithMock(t)
	now := time.Now()
	rp := json.RawMessage(`{"maxAttempts":3}`)
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(rrCols).AddRow(sampleRow("r1", "rule-1", now)...))

	_, err := s.UpdateRoutingRule(context.Background(), "r1", UpdateRoutingRuleParams{RetryPolicy: &rp})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestUpdateRoutingRule_ClearRetryPolicy(t *testing.T) {
	s, mock := newStoreWithMock(t)
	now := time.Now()
	empty := json.RawMessage(`null`)
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(rrCols).AddRow(sampleRow("r1", "rule-1", now)...))

	_, err := s.UpdateRoutingRule(context.Background(), "r1", UpdateRoutingRuleParams{RetryPolicy: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestUpdateRoutingRule_DBError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnError(errors.New("update boom"))

	_, err := s.UpdateRoutingRule(context.Background(), "r1", UpdateRoutingRuleParams{})
	if err == nil {
		t.Fatal("want DB error")
	}
}

func TestDeleteRoutingRule_Happy(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("r1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := s.DeleteRoutingRule(context.Background(), "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDeleteRoutingRule_NotFound(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("missing").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	err := s.DeleteRoutingRule(context.Background(), "missing")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("want pgx.ErrNoRows; got %v", err)
	}
}

func TestDeleteRoutingRule_DBError(t *testing.T) {
	s, mock := newStoreWithMock(t)
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("r1").
		WillReturnError(errors.New("delete boom"))

	if err := s.DeleteRoutingRule(context.Background(), "r1"); err == nil {
		t.Fatal("want DB error")
	}
}
