package validators

import (
	"strings"
	"testing"
)

// --- severityToDecision exhaustive -----------------------------------------

func TestSeverityToDecision_AllCases(t *testing.T) {
	cases := []struct {
		in   string
		want Decision
	}{
		{"hard", RejectHard},
		{"soft", BlockSoft},
		{"info", Approve},
		{"", Approve},        // empty → Approve (default)
		{"unknown", Approve}, // unknown → Approve (typo-safe)
		{"Hard", Approve},    // case-sensitive: not in switch
	}
	for _, c := range cases {
		if got := severityToDecision(c.in); got != c.want {
			t.Errorf("severityToDecision(%q): got %s want %s", c.in, got, c.want)
		}
	}
}

// --- strictestDecision exhaustive ------------------------------------------

func TestStrictestDecision_AllPairs(t *testing.T) {
	// Ordering (strictest → most permissive): RejectHard > BlockSoft > Modify > Approve.
	cases := []struct {
		a, b, want Decision
	}{
		{RejectHard, BlockSoft, RejectHard},
		{BlockSoft, RejectHard, RejectHard},
		{BlockSoft, Modify, BlockSoft},
		{Modify, BlockSoft, BlockSoft},
		{Modify, Approve, Modify},
		{Approve, Modify, Modify},
		{Approve, Approve, Approve},
		{RejectHard, RejectHard, RejectHard},
		{Decision("unknown"), Approve, Approve},      // unknown rank=0 < Approve rank=1
		{Decision("unknown"), Decision("unknown"), Decision("unknown")}, // both unknown: a returned
	}
	for _, c := range cases {
		if got := strictestDecision(c.a, c.b); got != c.want {
			t.Errorf("strictest(%q,%q): got %s want %s", c.a, c.b, got, c.want)
		}
	}
}

// --- parseRulePackInstalls error paths -------------------------------------

func TestParseRulePackInstalls_NilReturnsNil(t *testing.T) {
	out, err := parseRulePackInstalls(map[string]any{})
	if err != nil {
		t.Errorf("absent key: %v", err)
	}
	if out != nil {
		t.Errorf("got %v, want nil", out)
	}
}

func TestParseRulePackInstalls_ExplicitNilReturnsNil(t *testing.T) {
	out, err := parseRulePackInstalls(map[string]any{"_rulePackInstalls": nil})
	if err != nil {
		t.Errorf("nil value: %v", err)
	}
	if out != nil {
		t.Errorf("got %v want nil", out)
	}
}

func TestParseRulePackInstalls_UnsupportedTypeErrors(t *testing.T) {
	_, err := parseRulePackInstalls(map[string]any{"_rulePackInstalls": 42})
	if err == nil {
		t.Fatal("non-list, non-typed value should error")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("error should mention unsupported type: %v", err)
	}
}

func TestParseRulePackInstalls_MalformedJSONElementErrors(t *testing.T) {
	// []any with an element whose JSON re-marshal would yield an invalid
	// rulePackInstall shape — use a non-marshaling type that fails Marshal.
	// channels are not JSON-marshalable, so this triggers the Marshal error path.
	_, err := parseRulePackInstalls(map[string]any{
		"_rulePackInstalls": []any{make(chan int)},
	})
	if err == nil {
		t.Fatal("non-marshalable elem should error")
	}
	if !strings.Contains(err.Error(), "marshal") {
		t.Errorf("error should mention marshal: %v", err)
	}
}

// --- NewRulePackEngine error paths -----------------------------------------

func TestNewRulePackEngine_ParseInstallsErrorWrapped(t *testing.T) {
	_, err := NewRulePackEngine(&HookConfig{
		Config: map[string]any{"_rulePackInstalls": "not-a-list"},
	})
	if err == nil {
		t.Fatal("bad install shape should error")
	}
	if !strings.Contains(err.Error(), "rulepack-engine") {
		t.Errorf("error should be wrapped with rulepack-engine prefix: %v", err)
	}
}

func TestNewRulePackEngine_OnMatchValidationPropagates(t *testing.T) {
	_, err := NewRulePackEngine(&HookConfig{
		Config: map[string]any{
			"_rulePackInstalls": []rulePackInstall{},
			"onMatch":           map[string]any{"inflightAction": "purge"},
		},
	})
	if err == nil {
		t.Fatal("bad onMatch should be rejected")
	}
	if !strings.Contains(err.Error(), "rulepack-engine") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

// --- Execute: info severity emits tags only without blocking ----------------

func TestRulePackEngine_InfoSeverity_TagsOnlyNoBlock(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i-info", PackName: "info-pack", PackVersion: "1.0.0", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "info-1", Category: "metric", Severity: "info", Pattern: `\bping\b`,
		}},
	}})
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"ping the service"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Approve {
		t.Errorf("info severity: got %s want Approve (tag-only)", res.Decision)
	}
	if !containsString(res.Tags, "rulepack:info-pack") {
		t.Errorf("info match should still tag; got %v", res.Tags)
	}
	if !containsString(res.Tags, "rule:info-1") {
		t.Errorf("info match should tag rule:id; got %v", res.Tags)
	}
	if res.BlockingRule != nil {
		t.Errorf("info match must NOT set BlockingRule; got %+v", res.BlockingRule)
	}
}

func TestRulePackEngine_InfoSeverity_OnMatchCeilingIgnored(t *testing.T) {
	// onMatch.inflightAction = block-hard MUST NOT promote an info rule to
	// a block; informational rules are non-blocking by design.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "info-r", Severity: "info", Pattern: `\bxyz\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-hard"}
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"xyz appears"}),
	})
	if res.Decision != Approve {
		t.Errorf("operator block-hard must not override info severity; got %s", res.Decision)
	}
}

func TestRulePackEngine_OnMatchOverridePromotesSoftToHard(t *testing.T) {
	// Operator can promote a soft rule to hard via onMatch.inflightAction=block-hard.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "soft-r", Severity: "soft", Pattern: `\bnope\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-hard"}
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"say nope to it"}),
	})
	if res.Decision != RejectHard {
		t.Errorf("strictest-wins should promote soft→hard; got %s", res.Decision)
	}
}

// --- Execute: label/category tag stamping ----------------------------------

func TestRulePackEngine_LabelsStampedAsTags(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "r", Category: "phi", Severity: "hard", Pattern: `\bsecret\b`,
			Labels: []string{"customLabel", "another"},
		}},
	}})
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is secret"}),
	})
	if !containsString(res.Tags, "category:phi") {
		t.Errorf("Tags missing category:phi; got %v", res.Tags)
	}
	if !containsString(res.Tags, "customLabel") || !containsString(res.Tags, "another") {
		t.Errorf("Tags should include rule labels verbatim; got %v", res.Tags)
	}
}
