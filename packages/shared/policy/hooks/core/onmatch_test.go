package core

import (
	"strings"
	"testing"
)

func TestParseOnMatch_AbsentReturnsDefaults(t *testing.T) {
	got, err := ParseOnMatch(map[string]any{})
	if err != nil {
		t.Fatalf("absent onMatch should not error: %v", err)
	}
	if got.InflightAction != InflightBlockHard {
		t.Errorf("default Inflight: %q, want block-hard", got.InflightAction)
	}
	if got.StorageAction != StorageRedact {
		t.Errorf("default Storage: %q, want redact", got.StorageAction)
	}
	if got.Replacement != "[REDACTED_<RULE_ID>]" {
		t.Errorf("default Replacement: %q", got.Replacement)
	}
}

func TestParseOnMatch_NilEntryReturnsDefaults(t *testing.T) {
	got, err := ParseOnMatch(map[string]any{"onMatch": nil})
	if err != nil {
		t.Fatalf("nil onMatch should not error: %v", err)
	}
	if got.InflightAction != InflightBlockHard {
		t.Errorf("default Inflight: %q", got.InflightAction)
	}
}

func TestParseOnMatch_NonMapErrors(t *testing.T) {
	// onMatch must be an object; a string/number/array is a config bug.
	_, err := ParseOnMatch(map[string]any{"onMatch": "not-a-map"})
	if err == nil {
		t.Fatal("non-map onMatch should error")
	}
}

func TestParseOnMatch_ValidConfig(t *testing.T) {
	cfg := map[string]any{"onMatch": map[string]any{
		"inflightAction": "redact",
		"storageAction":  "drop-content",
		"replacement":    "*MASKED*",
	}}
	got, err := ParseOnMatch(cfg)
	if err != nil {
		t.Fatalf("ParseOnMatch: %v", err)
	}
	if got.InflightAction != InflightRedact {
		t.Errorf("Inflight: %q", got.InflightAction)
	}
	if got.StorageAction != StorageDropContent {
		t.Errorf("Storage: %q", got.StorageAction)
	}
	if got.Replacement != "*MASKED*" {
		t.Errorf("Replacement: %q", got.Replacement)
	}
}

func TestParseOnMatch_AllInflightActions(t *testing.T) {
	cases := []struct {
		s    string
		want InflightAction
	}{
		{"approve", InflightApprove},
		{"block-hard", InflightBlockHard},
		{"block-soft", InflightBlockSoft},
		{"redact", InflightRedact},
		{"APPROVE", InflightApprove}, // case-insensitive
	}
	for _, c := range cases {
		got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"inflightAction": c.s}})
		if err != nil {
			t.Errorf("%q: %v", c.s, err)
		}
		if got.InflightAction != c.want {
			t.Errorf("%q: got %q want %q", c.s, got.InflightAction, c.want)
		}
	}
}

func TestParseOnMatch_AllStorageActions(t *testing.T) {
	cases := []struct {
		s    string
		want StorageAction
	}{
		{"keep", StorageKeep},
		{"redact", StorageRedact},
		{"drop-content", StorageDropContent},
		{"DROP-CONTENT", StorageDropContent}, // case-insensitive
	}
	for _, c := range cases {
		got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"storageAction": c.s}})
		if err != nil {
			t.Errorf("%q: %v", c.s, err)
		}
		if got.StorageAction != c.want {
			t.Errorf("%q: got %q want %q", c.s, got.StorageAction, c.want)
		}
	}
}

func TestParseOnMatch_InvalidInflightActionErrors(t *testing.T) {
	_, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"inflightAction": "purge"}})
	if err == nil {
		t.Fatal("unknown inflight should error")
	}
	if !strings.Contains(err.Error(), "inflightAction") {
		t.Errorf("error should mention field: %v", err)
	}
}

func TestParseOnMatch_InvalidStorageActionErrors(t *testing.T) {
	_, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"storageAction": "destroy"}})
	if err == nil {
		t.Fatal("unknown storage should error")
	}
}

func TestParseOnMatch_EmptyStringIgnored(t *testing.T) {
	// Empty string values mean "use default" — must not error.
	got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{
		"inflightAction": "",
		"storageAction":  "",
		"replacement":    "",
	}})
	if err != nil {
		t.Fatalf("empty strings: %v", err)
	}
	if got.InflightAction != InflightBlockHard {
		t.Errorf("default Inflight: %q", got.InflightAction)
	}
	if got.Replacement != "[REDACTED_<RULE_ID>]" {
		t.Errorf("default Replacement: %q", got.Replacement)
	}
}

func TestDecisionForInflight_AllMappings(t *testing.T) {
	cases := []struct {
		in   InflightAction
		want Decision
	}{
		{InflightApprove, Approve},
		{InflightBlockHard, RejectHard},
		{InflightBlockSoft, BlockSoft},
		{InflightRedact, Modify},
		{InflightAction("unknown"), RejectHard}, // safe-default
	}
	for _, c := range cases {
		if got := DecisionForInflight(c.in); got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestResolveReplacement_SubstitutesRuleID(t *testing.T) {
	if got := ResolveReplacement("[REDACTED_<RULE_ID>]", "pii_email"); got != "[REDACTED_PII_EMAIL]" {
		t.Errorf("got %q", got)
	}
	if got := ResolveReplacement("<RULE_ID>_only", "x"); got != "X_only" {
		t.Errorf("partial template: %q", got)
	}
	// Template without placeholder passes through.
	if got := ResolveReplacement("*** REDACTED ***", "x"); got != "*** REDACTED ***" {
		t.Errorf("plain template: %q", got)
	}
}

func TestResolveReplacement_EmptyTemplateUsesDefault(t *testing.T) {
	if got := ResolveReplacement("", "email"); got != "[REDACTED_EMAIL]" {
		t.Errorf("empty template should fall back to default; got %q", got)
	}
}

func TestStrictestStorageAction_Ordering(t *testing.T) {
	// drop-content > redact > keep > ""
	cases := []struct {
		a, b, want StorageAction
	}{
		{StorageDropContent, StorageRedact, StorageDropContent},
		{StorageRedact, StorageDropContent, StorageDropContent},
		{StorageRedact, StorageKeep, StorageRedact},
		{StorageKeep, StorageRedact, StorageRedact},
		{StorageKeep, StorageKeep, StorageKeep},
		{"", StorageKeep, StorageKeep},
		{StorageDropContent, "", StorageDropContent},
		{"", "", ""}, // both empty → empty
	}
	for _, c := range cases {
		if got := StrictestStorageAction(c.a, c.b); got != c.want {
			t.Errorf("Strictest(%q,%q): got %q want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestStrictestDecision_Ordering(t *testing.T) {
	// RejectHard > BlockSoft > Modify > Approve > Abstain.
	// BlockSoft > Modify matches pipeline.mergeResults which prefers
	// hasSoftReject over hasModify when both fire.
	// Ties favor the first argument.
	cases := []struct {
		name       string
		a, b, want Decision
	}{
		// Adjacent rank pairs in both directions.
		{"reject_vs_block_soft", RejectHard, BlockSoft, RejectHard},
		{"block_soft_vs_reject", BlockSoft, RejectHard, RejectHard},
		{"block_soft_vs_modify", BlockSoft, Modify, BlockSoft},
		{"modify_vs_block_soft", Modify, BlockSoft, BlockSoft},
		{"modify_vs_approve", Modify, Approve, Modify},
		{"approve_vs_modify", Approve, Modify, Modify},
		{"approve_vs_abstain", Approve, Abstain, Approve},
		{"abstain_vs_approve", Abstain, Approve, Approve},
		// Wide jumps.
		{"reject_vs_approve", RejectHard, Approve, RejectHard},
		{"approve_vs_reject", Approve, RejectHard, RejectHard},
		{"reject_vs_abstain", RejectHard, Abstain, RejectHard},
		{"abstain_vs_reject", Abstain, RejectHard, RejectHard},
		{"modify_vs_reject", Modify, RejectHard, RejectHard},
		{"reject_vs_modify", RejectHard, Modify, RejectHard},
		// Ties — first argument wins so reconcile callers (suggested first,
		// ceiling second) keep the suggested label when no override fires.
		{"approve_tie", Approve, Approve, Approve},
		{"modify_tie", Modify, Modify, Modify},
		{"block_soft_tie", BlockSoft, BlockSoft, BlockSoft},
		{"reject_tie", RejectHard, RejectHard, RejectHard},
		// Abstain is rank 0 — Strictest(Abstain, X) is X for any non-zero X,
		// but Strictest(Abstain, Abstain) returns Abstain (the first arg).
		{"abstain_tie", Abstain, Abstain, Abstain},
		{"unrecognised_treated_as_zero", Decision("garbage"), Approve, Approve},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StrictestDecision(c.a, c.b); got != c.want {
				t.Errorf("Strictest(%q,%q): got %q want %q", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestLabelForDecision(t *testing.T) {
	cases := []struct {
		in   Decision
		want string
	}{
		{RejectHard, "block-hard"},
		{BlockSoft, "block-soft"},
		{Modify, "redact"},
		{Approve, "approve"},
		// Abstain falls into the default arm — lowercased Decision string.
		{Abstain, "abstain"},
		// Unrecognised Decision: lowercased verbatim.
		{Decision("MYSTERY"), "mystery"},
	}
	for _, c := range cases {
		t.Run(string(c.in), func(t *testing.T) {
			if got := LabelForDecision(c.in); got != c.want {
				t.Errorf("LabelForDecision(%q): got %q want %q", c.in, got, c.want)
			}
		})
	}
}

func TestLabelForDecision_RoundTripsWithDecisionForInflight(t *testing.T) {
	// Round-trip every InflightAction through DecisionForInflight then
	// back via LabelForDecision. The two helpers should be exact inverses
	// for the four reconcile-applicable inflight verbs.
	for _, in := range []InflightAction{InflightApprove, InflightBlockHard, InflightBlockSoft, InflightRedact} {
		d := DecisionForInflight(in)
		if got := LabelForDecision(d); got != string(in) {
			t.Errorf("round-trip %q → %q → %q (want %q)", in, d, got, in)
		}
	}
}

func TestDecisionForInflight_MapsAllFour(t *testing.T) {
	// Documents the inflight→Decision mapping that StrictestDecision
	// relies on at the reconcile call site.
	cases := []struct {
		in   InflightAction
		want Decision
	}{
		{InflightApprove, Approve},
		{InflightBlockHard, RejectHard},
		{InflightBlockSoft, BlockSoft},
		{InflightRedact, Modify},
	}
	for _, c := range cases {
		if got := DecisionForInflight(c.in); got != c.want {
			t.Errorf("DecisionForInflight(%q): got %q want %q", c.in, got, c.want)
		}
	}
	// Unknown InflightAction → safe default RejectHard.
	if got := DecisionForInflight("nonsense"); got != RejectHard {
		t.Errorf("DecisionForInflight(unknown): got %q want RejectHard", got)
	}
}
