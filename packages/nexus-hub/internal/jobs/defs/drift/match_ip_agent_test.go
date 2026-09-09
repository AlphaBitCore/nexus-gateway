package drift

import (
	"errors"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// matchIPAgent carries the verdict vocabulary, and the verdicts are not
// interchangeable: naming nobody is a gap an operator can see and act on, while
// naming the WRONG person is an attribution written into the audit trail that
// nothing later contradicts. The two-or-more case is the ordinary one on a
// shared NAT egress — an office, a VPN, a shared dev VM.
func TestMatchIPAgentVerdicts(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 8, 30, h, 0, 0, 0, time.UTC) }
	window := func(user string, from time.Time, to *time.Time) store.DeviceAssignmentWindow {
		w := store.DeviceAssignmentWindow{IP: "10.0.0.5", AssignedAt: from, ReleasedAt: to}
		w.UserID = user
		w.DeviceID = "dev-" + user
		w.DisplayName = user
		return w
	}
	noon := at(12)

	e := &IdentityEnricher{}

	t.Run("no source ip is not-found", func(t *testing.T) {
		_, err := e.matchIPAgent(store.PendingIdentityEvent{CreatedAt: at(10)}, nil)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("no windows is not-found", func(t *testing.T) {
		evt := store.PendingIdentityEvent{SourceIP: "10.0.0.5", CreatedAt: at(10)}
		if _, err := e.matchIPAgent(evt, nil); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("windows that do not cover the timestamp are not-found", func(t *testing.T) {
		evt := store.PendingIdentityEvent{SourceIP: "10.0.0.5", CreatedAt: at(14)}
		// Both windows closed before the event; the prefetch returns them
		// because they overlap the PAGE span, which is exactly why narrowing
		// per event is required rather than optional.
		windows := []store.DeviceAssignmentWindow{
			window("alice", at(8), &noon),
			window("bob", at(9), &noon),
		}
		if _, err := e.matchIPAgent(evt, windows); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound — neither window was in force at %v", err, at(14))
		}
	})

	t.Run("exactly one covering window is a match", func(t *testing.T) {
		evt := store.PendingIdentityEvent{SourceIP: "10.0.0.5", CreatedAt: at(14)}
		windows := []store.DeviceAssignmentWindow{
			window("alice", at(8), &noon), // released before the event
			window("bob", at(9), nil),     // still open
		}
		m, err := e.matchIPAgent(evt, windows)
		if err != nil {
			t.Fatalf("matchIPAgent: %v", err)
		}
		if m.EntityID != "bob" || m.Method != "ip_agent" {
			t.Errorf("matched %+v, want the open window for bob", m)
		}
	})

	t.Run("two covering windows refuse to name anybody", func(t *testing.T) {
		evt := store.PendingIdentityEvent{SourceIP: "10.0.0.5", CreatedAt: at(10)}
		windows := []store.DeviceAssignmentWindow{
			window("alice", at(8), nil),
			window("bob", at(9), nil),
		}
		if _, err := e.matchIPAgent(evt, windows); !errors.Is(err, store.ErrAmbiguous) {
			t.Errorf("err = %v, want ErrAmbiguous — two agents share this egress, and picking "+
				"the first is a confidently wrong attribution", err)
		}
	})
}
