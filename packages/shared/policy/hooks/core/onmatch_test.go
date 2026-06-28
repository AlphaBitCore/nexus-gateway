package core

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

func TestParseOnMatch_AbsentReturnsBlockDefault(t *testing.T) {
	got, err := ParseOnMatch(map[string]any{})
	if err != nil {
		t.Fatalf("absent onMatch should not error: %v", err)
	}
	if got.Action != ActionBlock {
		t.Errorf("default Action: %q, want block", got.Action)
	}
	if got.Replacement != "[REDACTED_<RULE_ID>]" {
		t.Errorf("default Replacement: %q", got.Replacement)
	}
}

func TestParseOnMatch_NilEntryReturnsBlockDefault(t *testing.T) {
	got, err := ParseOnMatch(map[string]any{"onMatch": nil})
	if err != nil {
		t.Fatalf("nil onMatch should not error: %v", err)
	}
	if got.Action != ActionBlock {
		t.Errorf("default Action: %q", got.Action)
	}
}

func TestParseOnMatch_NonMapErrors(t *testing.T) {
	_, err := ParseOnMatch(map[string]any{"onMatch": "not-a-map"})
	if err == nil {
		t.Fatal("non-map onMatch should error")
	}
}

func TestParseOnMatch_Action(t *testing.T) {
	// New single-action key.
	got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"action": "redact"}})
	if err != nil || got.Action != ActionRedact {
		t.Fatalf("new key: err=%v action=%q", err, got.Action)
	}
	// Case-insensitive.
	got, err = ParseOnMatch(map[string]any{"onMatch": map[string]any{"action": "APPROVE"}})
	if err != nil || got.Action != ActionApprove {
		t.Fatalf("case-insensitive: err=%v action=%q", err, got.Action)
	}
	// Invalid.
	if _, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"action": "bogus"}}); err == nil {
		t.Fatal("expected invalid action error")
	}
}

func TestParseOnMatch_AllActions(t *testing.T) {
	cases := []struct {
		s    string
		want Action
	}{
		{"approve", ActionApprove},
		{"redact", ActionRedact},
		{"block", ActionBlock},
	}
	for _, c := range cases {
		got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"action": c.s}})
		if err != nil {
			t.Errorf("%q: %v", c.s, err)
		}
		if got.Action != c.want {
			t.Errorf("%q: got %q want %q", c.s, got.Action, c.want)
		}
	}
}

func TestParseOnMatch_LegacyKeysMapped(t *testing.T) {
	// Legacy inflightAction/storageAction map via ActionFromLegacy.
	cases := []struct {
		inflight string
		storage  string
		want     Action
	}{
		{"block-hard", "redact", ActionBlock},
		{"block-soft", "keep", ActionBlock},
		{"redact", "keep", ActionRedact},
		{"approve", "keep", ActionApprove},
		{"approve", "redact", ActionRedact},       // pathological → safe-upgrade
		{"approve", "drop-content", ActionRedact}, // pathological → safe-upgrade
	}
	for _, c := range cases {
		m := map[string]any{}
		if c.inflight != "" {
			m["inflightAction"] = c.inflight
		}
		if c.storage != "" {
			m["storageAction"] = c.storage
		}
		got, err := ParseOnMatch(map[string]any{"onMatch": m})
		if err != nil {
			t.Errorf("legacy %v: %v", m, err)
		}
		if got.Action != c.want {
			t.Errorf("legacy %v: got %q want %q", m, got.Action, c.want)
		}
	}
}

func TestParseOnMatch_NewActionWinsOverLegacy(t *testing.T) {
	// When both new and legacy keys are present, the new action wins.
	got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{
		"action":         "approve",
		"inflightAction": "block-hard",
	}})
	if err != nil || got.Action != ActionApprove {
		t.Fatalf("new wins: err=%v action=%q", err, got.Action)
	}
}

func TestParseOnMatch_InvalidLegacyErrors(t *testing.T) {
	_, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"inflightAction": "purge"}})
	if err == nil {
		t.Fatal("unknown legacy inflightAction should error")
	}
	if !strings.Contains(err.Error(), "inflightAction") {
		t.Errorf("error should mention field: %v", err)
	}
	if _, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{"storageAction": "destroy"}}); err == nil {
		t.Fatal("unknown legacy storageAction should error")
	}
}

func TestParseOnMatch_ReplacementOverride(t *testing.T) {
	got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{
		"action":      "redact",
		"replacement": "*MASKED*",
	}})
	if err != nil {
		t.Fatalf("ParseOnMatch: %v", err)
	}
	if got.Replacement != "*MASKED*" {
		t.Errorf("Replacement: %q", got.Replacement)
	}
}

func TestParseOnMatch_EmptyStringsIgnored(t *testing.T) {
	got, err := ParseOnMatch(map[string]any{"onMatch": map[string]any{
		"action":      "",
		"replacement": "",
	}})
	if err != nil {
		t.Fatalf("empty strings: %v", err)
	}
	if got.Action != ActionBlock {
		t.Errorf("default Action: %q", got.Action)
	}
	if got.Replacement != "[REDACTED_<RULE_ID>]" {
		t.Errorf("default Replacement: %q", got.Replacement)
	}
}

func TestDecisionForAction_AllMappings(t *testing.T) {
	cases := []struct {
		in   decision.Action
		want Decision
	}{
		{decision.ActionApprove, Approve},
		{decision.ActionRedact, Modify},
		{decision.ActionBlock, RejectHard},
		{decision.Action("unknown"), RejectHard}, // fail-closed
	}
	for _, c := range cases {
		if got := DecisionForAction(c.in); got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestActionFromDecision_AllMappings(t *testing.T) {
	cases := []struct {
		in   Decision
		want decision.Action
	}{
		{RejectHard, decision.ActionBlock},
		{BlockSoft, decision.ActionBlock},
		{Modify, decision.ActionRedact},
		{Approve, decision.ActionApprove},
		{Abstain, decision.ActionApprove},
	}
	for _, c := range cases {
		if got := ActionFromDecision(c.in); got != c.want {
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
	if got := ResolveReplacement("*** REDACTED ***", "x"); got != "*** REDACTED ***" {
		t.Errorf("plain template: %q", got)
	}
}

func TestResolveReplacement_EmptyTemplateUsesDefault(t *testing.T) {
	if got := ResolveReplacement("", "email"); got != "[REDACTED_EMAIL]" {
		t.Errorf("empty template should fall back to default; got %q", got)
	}
}

func TestStrictestDecision_Ordering(t *testing.T) {
	// RejectHard > BlockSoft > Modify > Approve > Abstain. Ties favor first arg.
	cases := []struct {
		name       string
		a, b, want Decision
	}{
		{"reject_vs_modify", RejectHard, Modify, RejectHard},
		{"modify_vs_reject", Modify, RejectHard, RejectHard},
		{"modify_vs_approve", Modify, Approve, Modify},
		{"approve_vs_modify", Approve, Modify, Modify},
		{"approve_vs_abstain", Approve, Abstain, Approve},
		{"abstain_vs_approve", Abstain, Approve, Approve},
		{"reject_vs_approve", RejectHard, Approve, RejectHard},
		{"approve_tie", Approve, Approve, Approve},
		{"modify_tie", Modify, Modify, Modify},
		{"reject_tie", RejectHard, RejectHard, RejectHard},
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
		{Modify, "redact"},
		{Approve, "approve"},
		{Abstain, "abstain"},
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
