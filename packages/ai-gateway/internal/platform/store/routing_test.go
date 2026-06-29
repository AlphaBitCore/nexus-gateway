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
