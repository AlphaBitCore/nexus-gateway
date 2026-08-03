package lossmode

import "testing"

// The whole point of this package is one safety rule and two degradation rules. Each test below
// names the data-loss bug it prevents, because every failure here is silent in production: an
// audit pipeline that drops records looks exactly like an audit pipeline with less traffic.

// TestResolve_UnrecognisedIsNoLoss is the rule the package exists for. A typo in a config value
// must not be able to turn a compliance audit pipeline lossy — that is data loss that reports
// itself as nothing, since a dropped record leaves no trace by definition.
func TestResolve_UnrecognisedIsNoLoss(t *testing.T) {
	for _, in := range []string{
		"", " ", "SPILLBLOCK", "SpillBlock", "spill-block", "spill_block",
		"blok", "dropp", "none", "off", "false", "0", "lossy", "spillBlock",
	} {
		got := Resolve(in)
		if got != Default {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, Default)
		}
		if !got.NoLoss() {
			t.Errorf("Resolve(%q) = %q, which is LOSSY. An unrecognised config value must "+
				"never resolve to a mode that discards audit records.", in, got)
		}
	}
}

// TestResolve_ExactValuesRoundTrip pins that the four documented values are accepted verbatim.
// Case sensitivity is deliberate and is covered above: "SpillBlock" resolving to the default is
// safe, whereas silently accepting arbitrary casing would make the accepted vocabulary
// unknowable.
func TestResolve_ExactValuesRoundTrip(t *testing.T) {
	for _, want := range []Mode{SpillBlock, Block, Spill, Drop} {
		if got := Resolve(string(want)); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", want, got, want)
		}
	}
}

// TestLossyAndNoLoss_PartitionTheModes checks the two predicates are exact complements over the
// four modes. If a fifth mode is ever added and only one predicate is updated, a service could
// report "no loss" for a mode that drops — so the partition is asserted rather than assumed.
func TestLossyAndNoLoss_PartitionTheModes(t *testing.T) {
	for _, m := range []Mode{SpillBlock, Block, Spill, Drop} {
		if m.Lossy() == m.NoLoss() {
			t.Errorf("mode %q: Lossy()=%v and NoLoss()=%v — every mode must be exactly one of "+
				"the two, or a service will misreport whether its audit trail can lose records",
				m, m.Lossy(), m.NoLoss())
		}
	}
	if !SpillBlock.NoLoss() || !Block.NoLoss() {
		t.Error("spillblock and block must both report NoLoss")
	}
	if !Spill.Lossy() || !Drop.Lossy() {
		t.Error("spill and drop must both report Lossy")
	}
}

// TestWithoutDurableSink_NeverDegradesNoLossIntoADrop is the second safety rule. When a service
// is configured for a spool it does not have, the fallback must not quietly become lossy:
// spillblock degrades to block, which still loses nothing and merely stalls earlier.
func TestWithoutDurableSink_NeverDegradesNoLossIntoADrop(t *testing.T) {
	if got := SpillBlock.WithoutDurableSink(); got != Block {
		t.Fatalf("SpillBlock.WithoutDurableSink() = %q, want %q. Degrading a no-loss mode to a "+
			"lossy one because a spool is missing turns a misconfiguration into silent data loss.",
			got, Block)
	}
	if !SpillBlock.WithoutDurableSink().NoLoss() {
		t.Fatal("spillblock without a sink resolved to a lossy mode")
	}
}

// TestWithoutDurableSink_SpillBecomesDropHonestly is the other direction, and it is deliberate:
// an async spill with nowhere to spill IS a drop. Keeping the name "spill" there would make the
// configured mode lie about what the service does, which is worse than the honest downgrade.
func TestWithoutDurableSink_SpillBecomesDropHonestly(t *testing.T) {
	if got := Spill.WithoutDurableSink(); got != Drop {
		t.Fatalf("Spill.WithoutDurableSink() = %q, want %q", got, Drop)
	}
	// The modes that depend on no sink must pass through untouched.
	for _, m := range []Mode{Block, Drop} {
		if got := m.WithoutDurableSink(); got != m {
			t.Errorf("%q.WithoutDurableSink() = %q, want it unchanged — neither mode uses a spool",
				m, got)
		}
	}
}

func TestString(t *testing.T) {
	if got := SpillBlock.String(); got != "spillblock" {
		t.Fatalf("String() = %q, want %q", got, "spillblock")
	}
}

// TestOnOverflow_TheFullPolicyTable is the decision every service now shares. It is asserted as
// a table rather than per-case because the POINT is that all three services agree on every cell;
// a service that hand-rolled its own switch is how cp came to always spool and the agent to
// always drop with no way to select anything else.
func TestOnOverflow_TheFullPolicyTable(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		sink bool
		want Action
		why  string
	}{
		{SpillBlock, true, ActionSpool, "no-loss default spools first"},
		{SpillBlock, false, ActionBlock, "no sink: degrade to back-pressure, NEVER to a drop"},
		{Block, true, ActionBlock, "block never touches the spool even when one exists"},
		{Block, false, ActionBlock, "block has no sink dependency"},
		{Spill, true, ActionSpool, "spill uses the sink"},
		{Spill, false, ActionDrop, "no sink: an async spill with nowhere to spill IS a drop"},
		{Drop, true, ActionDrop, "drop ignores the sink"},
		{Drop, false, ActionDrop, "drop ignores the sink"},
	} {
		if got := tc.mode.OnOverflow(tc.sink); got != tc.want {
			t.Errorf("Mode(%q).OnOverflow(hasSink=%v) = %s, want %s — %s",
				tc.mode, tc.sink, got, tc.want, tc.why)
		}
	}
}

// TestOnOverflow_NoLossModesNeverDropWithoutASink is the property the table above encodes,
// asserted directly so it survives a table edit: neither no-loss mode may resolve to a drop just
// because a spool is missing.
func TestOnOverflow_NoLossModesNeverDropWithoutASink(t *testing.T) {
	for _, m := range []Mode{SpillBlock, Block} {
		for _, sink := range []bool{true, false} {
			if got := m.OnOverflow(sink); got == ActionDrop {
				t.Errorf("no-loss mode %q with hasSink=%v resolved to a DROP. A missing spool "+
					"must degrade to back-pressure, otherwise a misconfiguration becomes silent "+
					"audit data loss.", m, sink)
			}
		}
	}
}

func TestAction_String(t *testing.T) {
	for a, want := range map[Action]string{ActionSpool: "spool", ActionBlock: "block", ActionDrop: "drop", Action(9): "unknown"} {
		if got := a.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", a, got, want)
		}
	}
}
