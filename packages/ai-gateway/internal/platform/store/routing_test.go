package store

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

var routingTestColumns = []string{
	"id", "name", "strategyType", "config", "matchConditions",
	"priority", "pipelineStage", "fallbackChain", "retryPolicy",
	"enabled",
}

func makeRoutingRow(id string) []any {
	cfg := json.RawMessage(`{"type":"direct"}`)
	mc := json.RawMessage(`{"models":[]}`)
	fc := json.RawMessage(`[]`)
	rp := json.RawMessage(`null`)
	return []any{
		id, "rule-name", "direct", cfg, mc, 100, 0, fc, rp, true,
	}
}

// TestRoutingRuleOrderingIsTotal pins the rule sort to a TOTAL order.
//
// The resolver takes the first matching rule as the primary one, so the sort
// decides which rule serves a request whenever two of them match. Sorting by
// (pipelineStage, priority) alone leaves ties to whatever order the database
// happens to return, and the rule cache holds that order for thirty minutes —
// so an operator can watch the winner change on a cache refresh they did not
// cause, with nothing in the trace to explain it.
//
// Creation time breaks the tie, with the id as the final total-order guarantee.
// Ordering by id alone would also be deterministic, but the id is a UUID: an
// admin could not predict or explain the winner, and a deployment where one
// rule had won by accident for months would silently switch to the other on
// upgrade. Creation order is the one answer an operator can reason about.
//
// This asserts the SQL because the ordering is the database's to perform; the
// behaviour it protects is that two rules sharing a priority resolve the same
// way on every load.
func TestRoutingRuleOrderingIsTotal(t *testing.T) {
	mock, db := newMockDB(t)
	mock.ExpectQuery(`ORDER BY "pipelineStage" ASC, priority DESC, "createdAt" ASC, id ASC`).
		WillReturnRows(pgxmock.NewRows(routingTestColumns).AddRow(makeRoutingRow("r1")...))

	if _, err := db.GetEnabledRoutingRules(context.Background()); err != nil {
		t.Fatalf("rule ordering must end in a unique key so equal priorities cannot tie: %v", err)
	}
}

func TestGetEnabledRoutingRules(t *testing.T) {
	t.Run("happy and cached", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "RoutingRule"\s+WHERE enabled = true`).
			WillReturnRows(pgxmock.NewRows(routingTestColumns).
				AddRow(makeRoutingRow("r1")...).
				AddRow(makeRoutingRow("r2")...))
		got, err := db.GetEnabledRoutingRules(context.Background())
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 || got[0].ID != "r1" {
			t.Errorf("unexpected: %+v", got)
		}

		// Second call must hit cache — no second ExpectQuery registered.
		got2, err := db.GetEnabledRoutingRules(context.Background())
		if err != nil {
			t.Fatalf("cached call err: %v", err)
		}
		if len(got2) != 2 {
			t.Errorf("cached: %+v", got2)
		}
	})

	t.Run("empty result set is cached, not re-queried", func(t *testing.T) {
		mock, db := newMockDB(t)
		// Exactly ONE query is expected for the whole sub-test. A
		// deployment with zero enabled routing rules is a legitimate
		// steady state; the second and third lookups must be served
		// from cache. pgxmock errors on an unexpected Query, so a
		// re-query surfaces as a failure here.
		mock.ExpectQuery(`FROM "RoutingRule"\s+WHERE enabled = true`).
			WillReturnRows(pgxmock.NewRows(routingTestColumns))

		for i := range 3 {
			got, err := db.GetEnabledRoutingRules(context.Background())
			if err != nil {
				t.Fatalf("lookup %d: unexpected error (empty result must be cached): %v", i, err)
			}
			if len(got) != 0 {
				t.Fatalf("lookup %d: want zero rules, got %+v", i, got)
			}
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("expectations: %v", err)
		}
	})

	t.Run("cached empty result still honours invalidation", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "RoutingRule"`).
			WillReturnRows(pgxmock.NewRows(routingTestColumns))
		got, err := db.GetEnabledRoutingRules(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("initial load: rules=%+v err=%v", got, err)
		}

		// An admin enabling the first routing rule reaches the gateway as
		// a Hub `routing_rules` push -> InvalidateRuleCache. The cached
		// empty result must not survive it.
		mock.ExpectQuery(`FROM "RoutingRule"`).
			WillReturnRows(pgxmock.NewRows(routingTestColumns).AddRow(makeRoutingRow("r-new")...))
		db.InvalidateRuleCache()

		got2, err := db.GetEnabledRoutingRules(context.Background())
		if err != nil {
			t.Fatalf("post-invalidate load: %v", err)
		}
		if len(got2) != 1 || got2[0].ID != "r-new" {
			t.Errorf("stale empty result served after invalidation: %+v", got2)
		}
	})

	t.Run("query err wraps and is not cached", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "RoutingRule"`).WillReturnError(want)
		_, err := db.GetEnabledRoutingRules(context.Background())
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get routing rules") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "RoutingRule"`).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("r1"))
		_, err := db.GetEnabledRoutingRules(context.Background())
		if err == nil || !strings.Contains(err.Error(), "scan routing rule") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})
}

func TestInvalidateRuleCache(t *testing.T) {
	mock, db := newMockDB(t)
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WillReturnRows(pgxmock.NewRows(routingTestColumns).AddRow(makeRoutingRow("r1")...))
	got, err := db.GetEnabledRoutingRules(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}

	// Invalidate forces a re-fetch; register a second ExpectQuery.
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WillReturnRows(pgxmock.NewRows(routingTestColumns).AddRow(makeRoutingRow("r2")...))
	db.InvalidateRuleCache()

	got2, err := db.GetEnabledRoutingRules(context.Background())
	if err != nil {
		t.Fatalf("reload err: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != "r2" {
		t.Errorf("reload mismatch: %+v", got2)
	}
}
