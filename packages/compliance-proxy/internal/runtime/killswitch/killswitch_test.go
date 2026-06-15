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

func TestKillSwitch_HistoryRecordsToggles(t *testing.T) {
	ks := newTestKillSwitch()

	ks.Toggle(false, "")
	ks.Toggle(true, "")
	ks.Toggle(false, "")

	hist := ks.History()
	if len(hist) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(hist))
	}
	// Newest-first ordering: most recent toggle (false) is index 0.
	if hist[0].Engaged != false {
		t.Errorf("expected newest entry engaged=false, got %v", hist[0].Engaged)
	}
	if hist[1].Engaged != true {
		t.Errorf("expected middle entry engaged=true, got %v", hist[1].Engaged)
	}
	if hist[2].Engaged != false {
		t.Errorf("expected oldest entry engaged=false, got %v", hist[2].Engaged)
	}
	for i, e := range hist {
		if e.ChangedBy != "api" {
			t.Errorf("entry %d: expected changedBy=api, got %q", i, e.ChangedBy)
		}
		if e.ForceClose {
			t.Errorf("entry %d: forceClose should be false for plain Toggle", i)
		}
	}
}

func TestKillSwitch_HistoryRecordsForceClose(t *testing.T) {
	ks := newTestKillSwitch()
	ks.SetForceCloseFunc(func() int { return 7 })

	ks.Toggle(true, "") // baseline
	_, closed := ks.ForceClose("")
	if closed != 7 {
		t.Errorf("expected 7 force-closed connections, got %d", closed)
	}

	hist := ks.History()
	if len(hist) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(hist))
	}
	// Newest is the force-close.
	fc := hist[0]
	if !fc.ForceClose {
		t.Errorf("newest entry should have forceClose=true")
	}
	if fc.ForceClosedCount != 7 {
		t.Errorf("expected forceClosedCount=7, got %d", fc.ForceClosedCount)
	}
	if fc.Engaged {
		t.Errorf("force-close should leave engaged=false")
	}
	if fc.ChangedBy != "api" {
		t.Errorf("expected changedBy=api, got %q", fc.ChangedBy)
	}
}

func TestKillSwitch_HistoryRingCap(t *testing.T) {
	ks := newTestKillSwitch()
	cap := ks.HistoryCapacity()

	// Push cap+5 toggles. Use bool flip so each is a "change".
	for i := range cap + 5 {
		ks.Toggle(i%2 == 0, "")
	}

	hist := ks.History()
	if len(hist) != cap {
		t.Fatalf("expected history capped at %d, got %d", cap, len(hist))
	}
	// Newest entry should reflect the most recent toggle: i=cap+4.
	wantNewest := (cap+4)%2 == 0
	if hist[0].Engaged != wantNewest {
		t.Errorf("expected newest entry engaged=%v, got %v", wantNewest, hist[0].Engaged)
	}
}

func TestKillSwitch_HistoryEmpty(t *testing.T) {
	ks := newTestKillSwitch()
	hist := ks.History()
	if len(hist) != 0 {
		t.Errorf("expected empty history on fresh KillSwitch, got %d entries", len(hist))
	}
	if ks.HistoryCapacity() != 100 {
		t.Errorf("expected default capacity 100, got %d", ks.HistoryCapacity())
	}
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
	hist := ks.History()
	if len(hist) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(hist))
	}
	if hist[0].ChangedBy != "break-glass" {
		t.Errorf("expected history changedBy=break-glass, got %q", hist[0].ChangedBy)
	}
	if !hist[0].Engaged {
		t.Errorf("expected history entry engaged=true, got %v", hist[0].Engaged)
	}
}

// TestKillSwitch_ApplyBreakGlassRedundant_NoHistory pins the short-circuit
// semantics documented on ApplyBreakGlass: when the incoming engaged flag
// already matches the current state, the apply is a no-op on the in-memory
// history — no duplicate entry is appended and no toggle log line is emitted —
// while the engaged state remains correct. This keeps a recovering Hub (which
// may re-push the same desired state) from spamming the operational toggle log.
func TestKillSwitch_ApplyBreakGlassRedundant_NoHistory(t *testing.T) {
	ks := newTestKillSwitch()

	// First break-glass flips false→true: one real history entry.
	if err := ks.ApplyBreakGlass(interception.Killswitch{Engaged: true}); err != nil {
		t.Fatalf("first ApplyBreakGlass: %v", err)
	}
	if got := len(ks.History()); got != 1 {
		t.Fatalf("after first apply: expected 1 history entry, got %d", got)
	}

	// Redundant break-glass with the same engaged=true: must short-circuit.
	if err := ks.ApplyBreakGlass(interception.Killswitch{Engaged: true}); err != nil {
		t.Fatalf("redundant ApplyBreakGlass: %v", err)
	}
	if !ks.IsEngaged() {
		t.Errorf("expected engaged=true to be preserved after redundant break-glass")
	}
	if got := len(ks.History()); got != 1 {
		t.Errorf("redundant break-glass must not append history: expected 1 entry, got %d", got)
	}

	// A genuine state change (true→false) still records, proving the
	// short-circuit only suppresses no-op applies.
	if err := ks.ApplyBreakGlass(interception.Killswitch{Engaged: false}); err != nil {
		t.Fatalf("state-changing ApplyBreakGlass: %v", err)
	}
	if ks.IsEngaged() {
		t.Errorf("expected engaged=false after disengaging break-glass")
	}
	if got := len(ks.History()); got != 2 {
		t.Errorf("state-changing break-glass must append history: expected 2 entries, got %d", got)
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

// TestKillSwitch_ForceCloseEmptyChangedByFallsBackToAPI mirrors the
// Toggle branch for ForceClose — same audit invariant.
func TestKillSwitch_ForceCloseEmptyChangedByFallsBackToAPI(t *testing.T) {
	ks := newTestKillSwitch()
	state, closed := ks.ForceClose("")
	if state.ChangedBy != "api" {
		t.Errorf("empty changedBy should fall back to api, got %q", state.ChangedBy)
	}
	if closed != 0 {
		t.Errorf("ForceClose without forceCloseFn should return 0, got %d", closed)
	}
}

// TestKillSwitch_SetForceCloseFuncInvokedByForceClose pins the
// SetForceCloseFunc → ForceClose plumbing — without this, the
// connection-killing side of an emergency stop would have no
// observable contract.
func TestKillSwitch_SetForceCloseFuncInvokedByForceClose(t *testing.T) {
	ks := newTestKillSwitch()
	invoked := false
	ks.SetForceCloseFunc(func() int {
		invoked = true
		return 7
	})
	_, closed := ks.ForceClose("admin")
	if !invoked {
		t.Error("ForceCloseFunc must be invoked by ForceClose")
	}
	if closed != 7 {
		t.Errorf("ForceClose should return fn result, got %d", closed)
	}
}
