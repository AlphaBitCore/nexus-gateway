package killswitch

import (
	"log/slog"
	"os"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

func newTestKillSwitch() *KillSwitch {
	return NewKillSwitch(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// ApplyBreakGlass mirrors a Toggle with changedBy="break-glass" so operators
// can distinguish emergency PUT hits from normal shadow applies. A break-glass
// that changes state (here: false→true) must flip the engaged flag and record
// exactly one history entry attributed to break-glass.
func TestKillSwitch_ApplyBreakGlass(t *testing.T) {
	ks := newTestKillSwitch()

	if err := ks.ApplyBreakGlass(interception.Killswitch{Engaged: true}); err != nil {
		t.Fatalf("ApplyBreakGlass: %v", err)
	}
	if !ks.IsEngaged() {
		t.Errorf("expected engaged=true after break-glass")
	}
	st := ks.State()
	if !st.Engaged {
		t.Errorf("State().Engaged = false, want true after break-glass")
	}
	if st.ChangedBy != "break-glass" {
		t.Errorf("State().ChangedBy = %q, want break-glass", st.ChangedBy)
	}
	if st.LastChanged.IsZero() {
		t.Error("State().LastChanged was never stamped")
	}
}

// TestKillSwitch_ApplyBreakGlassRedundant_LeavesTheStampAlone pins the
// short-circuit documented on ApplyBreakGlass: when the incoming engaged flag
// already matches, the apply must not restamp lastChanged/changedBy or emit a
// toggle log line, while the engaged state stays correct. This keeps a
// recovering Hub, which may re-push the same desired state, from spamming the
// operational log and from overwriting who really last changed it.
//
// The observable moved from the in-memory History ring (deleted along with the
// unreachable force-close feature) to State(), which is what the /killswitch
// GET surfaces. changedBy is the discriminator rather than a timestamp
// comparison on purpose: it is discrete, so the assertion does not rest on two
// calls landing in different nanoseconds.
func TestKillSwitch_ApplyBreakGlassRedundant_LeavesTheStampAlone(t *testing.T) {
	ks := newTestKillSwitch()

	// Engage under a distinct identity so a restamp is unmistakable.
	ks.Toggle(true, "operator-A")
	if got := ks.State().ChangedBy; got != "operator-A" {
		t.Fatalf("setup: ChangedBy = %q, want operator-A", got)
	}

	// Redundant break-glass with the same engaged=true: must short-circuit.
	if err := ks.ApplyBreakGlass(interception.Killswitch{Engaged: true}); err != nil {
		t.Fatalf("redundant ApplyBreakGlass: %v", err)
	}
	if !ks.IsEngaged() {
		t.Errorf("expected engaged=true to be preserved after redundant break-glass")
	}
	if got := ks.State().ChangedBy; got != "operator-A" {
		t.Errorf("redundant break-glass restamped ChangedBy to %q; the short-circuit "+
			"exists so a re-pushed desired state does not overwrite who last acted", got)
	}

	// A genuine state change still stamps, proving the short-circuit only
	// suppresses no-op applies.
	if err := ks.ApplyBreakGlass(interception.Killswitch{Engaged: false}); err != nil {
		t.Fatalf("state-changing ApplyBreakGlass: %v", err)
	}
	if ks.IsEngaged() {
		t.Errorf("expected engaged=false after disengaging break-glass")
	}
	if got := ks.State().ChangedBy; got != "break-glass" {
		t.Errorf("a real state change must stamp ChangedBy; got %q", got)
	}
}

// TestKillSwitch_State covers the State() accessor — read after a
// toggle should reflect the new engaged flag and audit metadata.
// Without this, the State() readers in the /killswitch GET handler
// would have no direct test pin.
func TestKillSwitch_State(t *testing.T) {
	ks := newTestKillSwitch()
	before := ks.State()
	if before.Engaged {
		t.Error("State.Engaged should default to false")
	}

	ks.Toggle(true, "admin@example.com")
	st := ks.State()
	if !st.Engaged {
		t.Error("State.Engaged should be true after Toggle(true)")
	}
	if st.ChangedBy != "admin@example.com" {
		t.Errorf("State.ChangedBy = %q, want admin@example.com", st.ChangedBy)
	}
	if st.LastChanged.IsZero() {
		t.Error("State.LastChanged should be set after Toggle")
	}
}

// TestKillSwitch_Snapshot covers Snapshot() — the configtypes shape
// fed into the /runtime/config read surface. Should reflect only the
// engaged flag (audit fields stay internal).
func TestKillSwitch_Snapshot(t *testing.T) {
	ks := newTestKillSwitch()
	if ks.Snapshot().Engaged {
		t.Error("Snapshot.Engaged should default to false")
	}
	ks.Toggle(true, "test")
	snap := ks.Snapshot()
	if !snap.Engaged {
		t.Error("Snapshot.Engaged should be true after Toggle(true)")
	}
	// interception.Killswitch is intentionally small — audit fields not
	// part of the shape — so we just assert the bool flips.
	_ = interception.Killswitch{} // touch import
}

// TestKillSwitch_ToggleEmptyChangedByFallsBackToAPI covers the
// `if changedBy == "" { changedBy = "api" }` branch — direct
// API hits without BFF must still produce an audit-distinguishable
// entry.
func TestKillSwitch_ToggleEmptyChangedByFallsBackToAPI(t *testing.T) {
	ks := newTestKillSwitch()
	ks.Toggle(true, "")
	if got := ks.State().ChangedBy; got != "api" {
		t.Errorf("empty changedBy should fall back to api, got %q", got)
	}
}
