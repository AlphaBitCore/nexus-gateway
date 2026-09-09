package governancestore

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// agentWindowPredicate is the ownership bound the agent leg must carry.
//
// It is pinned as a REGEX ON THE SQL rather than checked through returned rows
// because pgxmock never executes the statement — it replays columns by
// position. A test that only asserts the scanned struct stays green when the
// predicate is deleted, which is exactly how an unwindowed query shipped: the
// existing expectations match on `FROM traffic_event` and cannot see this at all.
const agentWindowPredicate = `da\."userId" = \$1\s+AND traffic_event\.timestamp >= da\."assignedAt"\s+AND \(da\."releasedAt" IS NULL OR traffic_event\.timestamp < da\."releasedAt"\)`

// TestUserAudit_AgentLegIsScopedToTheAssignmentWindow covers all three reads.
//
// Without the window the agent leg is a bare `thing_id IN (the user's devices)`,
// so a device reassigned A -> B surfaces B's agent events in A's audit view and
// vice versa: the released assignment row still names the device, and nothing
// says WHEN it was theirs. That is a cross-subject content leak on the screen an
// operator reads to answer "what did this person do". The DSAR access and erase
// paths have always been scoped this way; these three had not caught up.
func TestUserAudit_AgentLegIsScopedToTheAssignmentWindow(t *testing.T) {
	t.Run("count and list", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event[\s\S]*` + agentWindowPredicate).
			WithArgs("u1").
			WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
		m.ExpectQuery(`FROM traffic_event[\s\S]*`+agentWindowPredicate).
			WithArgs("u1", 10, 0).
			WillReturnRows(pgxmock.NewRows(
				[]string{"id", "source", "timestamp", "target_host", "latency_ms",
					"entity_id", "entity_type", "request_hook_decision", "details"},
			).AddRow("e1", "agent", tNow, sp("h"), ip(5), sp("u1"), sp("user"), sp("ALLOW"), []byte(`{}`)))

		evs, total, err := s.GetUserAuditEvents(context.Background(), "u1", 10, 0)
		if err != nil {
			t.Fatalf("GetUserAuditEvents: %v", err)
		}
		if total != 1 || len(evs) != 1 {
			t.Fatalf("unexpected result: %+v total=%d", evs, total)
		}
	})

	t.Run("summary", func(t *testing.T) {
		s, m := newMock(t)
		m.ExpectQuery(`FROM traffic_event[\s\S]*` + agentWindowPredicate).
			WithArgs("u1").
			WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c", "d", "e"}).
				AddRow(3, 1, 1, 1, &tNow))

		sum, err := s.GetUserAuditSummary(context.Background(), "u1")
		if err != nil {
			t.Fatalf("GetUserAuditSummary: %v", err)
		}
		if sum.TotalEvents != 3 || sum.AgentEvents != 1 {
			t.Fatalf("unexpected summary: %+v", sum)
		}
	})
}

// TestUserAudit_VirtualKeyLegIsNotWindowed pins the other half of the decision.
// The entity_id leg names the subject directly, so it needs no window — adding
// one there would silently drop the subject's own virtual-key traffic.
func TestUserAudit_VirtualKeyLegIsNotWindowed(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`WHERE entity_id = \$1`).
		WithArgs("u1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m.ExpectQuery(`WHERE entity_id = \$1`).
		WithArgs("u1", 10, 0).
		WillReturnRows(pgxmock.NewRows(
			[]string{"id", "source", "timestamp", "target_host", "latency_ms",
				"entity_id", "entity_type", "request_hook_decision", "details"}))

	if _, _, err := s.GetUserAuditEvents(context.Background(), "u1", 10, 0); err != nil {
		t.Fatalf("GetUserAuditEvents: %v", err)
	}
}
