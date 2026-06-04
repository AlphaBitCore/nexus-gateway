package queue

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

// TestQueryEventsFiltered_AIOnly_FiltersToInspect verifies the #88
// AI-only branch matches rows where action=inspect (the primary
// AI signal) or domain_rule_id is non-empty. Non-AI rows (passthrough
// without a domain rule) are excluded — fixes the pre-#88 UI bug where
// "AI only" over-fetched + JS-filtered with broken pagination + wrong
// total.
func TestQueryEventsFiltered_AIOnly_FiltersToInspect(t *testing.T) {
	q := newTestQueue(t)
	now := time.Now().UTC()
	mustRecord(t, q, event.Event{
		ID: "ai-by-inspect", Timestamp: now,
		TargetHost: "openai.com", Action: "inspect",
	})
	mustRecord(t, q, event.Event{
		ID: "ai-by-rule", Timestamp: now,
		TargetHost: "x.com", Action: "passthrough", DomainRuleID: "rule-7",
	})
	mustRecord(t, q, event.Event{
		ID: "non-ai", Timestamp: now,
		TargetHost: "apple.com", Action: "passthrough",
	})

	events, total, err := q.QueryEventsFiltered(QueryEventsFilter{
		AIOnly: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (inspect + domain-rule)", total)
	}
	got := map[string]bool{}
	for _, e := range events {
		got[e.ID] = true
	}
	if !got["ai-by-inspect"] || !got["ai-by-rule"] {
		t.Errorf("expected both AI rows; got %v", got)
	}
	if got["non-ai"] {
		t.Errorf("non-AI row leaked into AI Only result")
	}
}

// TestQueryEventsFiltered_Since_FiltersByCreatedAt verifies the time
// window narrows by created_at, the same column used for ORDER BY.
// A row older than the window must not appear; rows inside must.
func TestQueryEventsFiltered_Since_FiltersByCreatedAt(t *testing.T) {
	q := newTestQueue(t)
	now := time.Now().UTC()
	// Record events; created_at defaults to NOW() in SQL. Insert both
	// fresh and rewind one's created_at via direct UPDATE.
	mustRecord(t, q, event.Event{ID: "fresh-1", Timestamp: now, TargetHost: "fresh.example.com", Action: "passthrough"})
	mustRecord(t, q, event.Event{ID: "fresh-2", Timestamp: now, TargetHost: "fresh.example.com", Action: "inspect"})
	mustRecord(t, q, event.Event{ID: "stale", Timestamp: now.Add(-48 * time.Hour), TargetHost: "stale.example.com", Action: "passthrough"})
	// Push one row's created_at into the past.
	if _, err := q.db.Exec("UPDATE audit_events SET created_at = datetime('now', '-48 hours') WHERE id = ?", "stale"); err != nil {
		t.Fatal(err)
	}

	// Since = 1h ago should only return the two fresh rows.
	events, total, err := q.QueryEventsFiltered(QueryEventsFilter{
		Since: time.Now().Add(-1 * time.Hour),
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (stale row beyond window)", total)
	}
	for _, e := range events {
		if e.ID == "stale" {
			t.Errorf("stale row leaked through Since filter")
		}
	}
}

// TestQueryEventsFiltered_BothFiltersCompose verifies AIOnly + Since
// combine via SQL AND, not OR — only AI rows inside the time window
// appear.
func TestQueryEventsFiltered_BothFiltersCompose(t *testing.T) {
	q := newTestQueue(t)
	now := time.Now().UTC()
	mustRecord(t, q, event.Event{ID: "fresh-ai", Timestamp: now, TargetHost: "openai.com", Action: "inspect"})
	mustRecord(t, q, event.Event{ID: "fresh-non-ai", Timestamp: now, TargetHost: "apple.com", Action: "passthrough"})
	mustRecord(t, q, event.Event{ID: "stale-ai", Timestamp: now.Add(-48 * time.Hour), TargetHost: "anthropic.com", Action: "inspect"})
	if _, err := q.db.Exec("UPDATE audit_events SET created_at = datetime('now', '-48 hours') WHERE id = ?", "stale-ai"); err != nil {
		t.Fatal(err)
	}

	events, total, err := q.QueryEventsFiltered(QueryEventsFilter{
		AIOnly: true,
		Since:  time.Now().Add(-1 * time.Hour),
		Limit:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 (fresh-ai only)", total)
	}
	if len(events) != 1 || events[0].ID != "fresh-ai" {
		t.Errorf("expected only fresh-ai; got %d events: %v", len(events), eventIDs(events))
	}
}

func mustRecord(t *testing.T, q *Queue, e event.Event) {
	t.Helper()
	if err := q.Record(e); err != nil {
		t.Fatalf("Record %s: %v", e.ID, err)
	}
}

func eventIDs(events []event.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}
