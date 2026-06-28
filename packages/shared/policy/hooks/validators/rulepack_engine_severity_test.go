package validators

import (
	"strings"
	"testing"
	"time"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// --- severityEnforces exhaustive -------------------------------------------

func TestSeverityEnforces_AllCases(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"hard", true},
		{"soft", true}, // soft enforces too; the action comes from onMatch, not the tier
		{"warn", false},
		{"info", false},
		{"", false},        // empty → observe (default)
		{"unknown", false}, // unknown → observe (typo-safe)
		{"Hard", false},    // case-sensitive: not in switch
	}
	for _, c := range cases {
		if got := severityEnforces(c.in); got != c.want {
			t.Errorf("severityEnforces(%q): got %v want %v", c.in, got, c.want)
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

func TestRulePackEngine_InfoSeverity_NeverEnforces(t *testing.T) {
	// A block-policy hook MUST NOT enforce an info-severity (observe) rule;
	// informational rules are non-enforcing by design — they tag only.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "info-r", Severity: "info", Pattern: `\bxyz\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "block"}
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"xyz appears"}),
	})
	if res.Decision != Approve {
		t.Errorf("block-policy hook must not enforce an info (observe) rule; got %s", res.Decision)
	}
}

func TestRulePackEngine_EnforcingRule_AppliesHookBlockAction(t *testing.T) {
	// An enforcing (soft/hard) rule on a block-policy hook blocks — the action
	// is the hook's onMatch.Action, applied to any enforcing match.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "soft-r", Severity: "soft", Pattern: `\bnope\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "block"}
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"say nope to it"}),
	})
	if res.Decision != RejectHard {
		t.Errorf("enforcing rule on a block hook should block; got %s", res.Decision)
	}
}

func TestRulePackEngine_HardRule_OnRedactHook_Redacts(t *testing.T) {
	// Severity NEVER escalates past the operator's onMatch.Action. A hard
	// rule on a redact-policy hook redacts (Modify) — it does NOT block. The
	// hook's Action policy decides the action; the severity tier only decides
	// enforce-vs-observe.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "hard-r", Category: "secret_leak", Severity: "hard", Pattern: `\bAKIA[0-9A-Z]{16}\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"key AKIA1234567890ABCDEF here"}),
	})
	if res.Decision != Modify {
		t.Errorf("hard rule on a redact hook must redact (Modify), not escalate to block; got %s", res.Decision)
	}
	if res.Action != ActionRedact {
		t.Errorf("action should be the hook's redact policy; got %q", res.Action)
	}
	if res.BlockingRule == nil || res.BlockingRule.RuleID != "hard-r" {
		t.Errorf("redact match should still record attribution; got %+v", res.BlockingRule)
	}
}

func TestRulePackEngine_HardRule_OnApproveHook_ObserveOnly(t *testing.T) {
	// A pure-observe hook (onMatch.action=approve) tags even hard matches
	// without enforcing — severity never forces enforcement past the policy.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "obs", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "hard-o", Severity: "hard", Pattern: `\bnope\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "approve"}
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(t.Context(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"say nope to it"}),
	})
	if res.Decision != Approve {
		t.Errorf("approve-policy hook is observe-only; got %s", res.Decision)
	}
	if res.BlockingRule != nil {
		t.Errorf("observe-only match must not set BlockingRule; got %+v", res.BlockingRule)
	}
	if !containsString(res.Tags, "rule:hard-o") {
		t.Errorf("observe match should still tag the rule; got %v", res.Tags)
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

// TestRulePackEngine_Redact_MasksMatchedContent is the gate that the F3 gap
// lacked: it proves a redact rule-pack hook produces a precise masking span over
// the matched bytes (not just a decision). Without it the engine returned a
// redact "decision" with no TransformSpans, so nothing was ever masked.
func TestRulePackEngine_Redact_MasksMatchedContent(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{
			RuleID: "ssn", Category: "pii.ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`,
		}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	text := "my ssn is 123-45-6789 ok"
	res, err := h.Execute(t.Context(), &HookInput{Normalized: PayloadFromTextSegments([]string{text})})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify || res.Action != ActionRedact {
		t.Fatalf("redact must Modify+redact; got decision=%s action=%s", res.Decision, res.Action)
	}
	if len(res.TransformSpans) != 1 {
		t.Fatalf("expected exactly one masking span; got %d: %+v", len(res.TransformSpans), res.TransformSpans)
	}
	sp := res.TransformSpans[0]
	if sp.ContentAddress != "messages.0.content.0" {
		t.Errorf("span address = %q, want messages.0.content.0", sp.ContentAddress)
	}
	if sp.SourceID != "ssn" {
		t.Errorf("span must attribute the rule; SourceID = %q, want ssn", sp.SourceID)
	}
	// The decisive masking proof: the span's byte range exactly covers the SSN, so
	// applying it removes the sensitive substring.
	if got := text[sp.Start:sp.End]; got != "123-45-6789" {
		t.Errorf("span [%d:%d] = %q, want the SSN 123-45-6789", sp.Start, sp.End, got)
	}
	if sp.Replacement == "" || strings.Contains(sp.Replacement, "123-45-6789") {
		t.Errorf("replacement must be a non-leaking mask; got %q", sp.Replacement)
	}
	if res.BlockingRule == nil || res.BlockingRule.RuleID != "ssn" {
		t.Errorf("redact must attribute via BlockingRule; got %+v", res.BlockingRule)
	}
	// ModifiedContent is what the proxy actually rewrites the forwarded + stored
	// body from (it does NOT consume TransformSpans for the body rewrite). Without
	// it the spans are produced but nothing is masked end-to-end — this asserts the
	// redacted block carries the mask and not the raw SSN.
	if len(res.ModifiedContent) != 1 {
		t.Fatalf("redact must emit ModifiedContent for the proxy body rewrite; got %d blocks", len(res.ModifiedContent))
	}
	mc := res.ModifiedContent[0].Text
	if strings.Contains(mc, "123-45-6789") {
		t.Errorf("ModifiedContent still contains the raw SSN: %q", mc)
	}
	if !strings.Contains(mc, sp.Replacement) {
		t.Errorf("ModifiedContent must carry the mask %q; got %q", sp.Replacement, mc)
	}
}

// TestRulePackEngine_Redact_EmbeddingInputs covers the KindAIEmbedding address
// branch: a redact match in an embedding input must mask "inputs.<i>".
func TestRulePackEngine_Redact_EmbeddingInputs(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:   normalize.KindAIEmbedding,
		Inputs: []string{"ssn 123-45-6789 here"},
	}}
	res, err := h.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.TransformSpans) != 1 || res.TransformSpans[0].ContentAddress != "inputs.0" {
		t.Fatalf("embedding redact span should address inputs.0; got %+v", res.TransformSpans)
	}
}

// TestRulePackEngine_Redact_ToolResult covers the ContentToolResult address
// branch: a redact match inside a tool-result output masks the toolResult slot.
func TestRulePackEngine_Redact_ToolResult(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{{Content: []normalize.ContentBlock{
			{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{Output: "leak 123-45-6789 end"}},
		}}},
	}}
	res, err := h.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.TransformSpans) != 1 || res.TransformSpans[0].ContentAddress != "messages.0.content.0.toolResult" {
		t.Fatalf("tool-result redact span should address messages.0.content.0.toolResult; got %+v", res.TransformSpans)
	}
}

// TestRulePackEngine_Redact_ObserveSeverityTagsNoMask covers the observe-only
// branch inside executeRedact: a warn-severity rule on a redact hook tags the
// match but masks nothing (severity gates enforcement, not the action).
func TestRulePackEngine_Redact_ObserveSeverityTagsNoMask(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "warn-ssn", Category: "pii.ssn", Severity: "warn", Pattern: `\b\d{3}-\d{2}-\d{4}\b`}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	res, err := h.Execute(t.Context(), &HookInput{Normalized: PayloadFromTextSegments([]string{"ssn 123-45-6789"})})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("warn-severity rule on a redact hook is observe-only; want Approve, got %s", res.Decision)
	}
	if len(res.TransformSpans) != 0 {
		t.Errorf("observe-only must not mask; got spans %+v", res.TransformSpans)
	}
	tagged := false
	for _, tg := range res.Tags {
		if tg == "rule:warn-ssn" {
			tagged = true
		}
	}
	if !tagged {
		t.Errorf("observe-only match should still tag; got %v", res.Tags)
	}
}

// TestRulePackEngine_Close_ReleasesMatcher covers the engine Close (matcher
// teardown on config swap) and its idempotency.
func TestRulePackEngine_Close_ReleasesMatcher(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "r", Severity: "hard", Pattern: `\bnope\b`}},
	}})
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	closer, ok := h.(interface{ Close() error })
	if !ok {
		t.Fatal("engine must implement Close")
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("second Close must be idempotent: %v", err)
	}
}

// TestRulePackEngine_Redact_ReasoningScope covers the ContentReasoning address
// branch: with scope=include_reasoning a redact match inside a reasoning block is
// masked at its message/content address.
func TestRulePackEngine_Redact_ReasoningScope(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	cfg.Scope = "include_reasoning"
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{{Content: []normalize.ContentBlock{
			{Type: normalize.ContentReasoning, Text: "thinking 123-45-6789 done"},
		}}},
	}}
	res, err := h.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.TransformSpans) != 1 || res.TransformSpans[0].ContentAddress != "messages.0.content.0" {
		t.Fatalf("reasoning redact (include_reasoning) should mask at messages.0.content.0; got %+v", res.TransformSpans)
	}
}

// TestRulePackEngine_Redact_TruncatedScanFailSafe pins the compliance fail-safe:
// when the cgo scan truncates (alloc abort), its hit set may be missing a rule
// whose content carries PII. With complete=false, redaction must re-localise
// EVERY rule and still mask, even if the matched set is empty. The same empty
// matched set with complete=true must NOT mask (it trusts the matcher) — so this
// proves the fail-safe, not just that redaction works.
func TestRulePackEngine_Redact_TruncatedScanFailSafe(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{
			{RuleID: "ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`},
			// A second rule that does NOT match the content: under the incomplete
			// fail-safe every rule is a candidate, so this exercises the RE2
			// "candidate didn't actually match → skip" branch.
			{RuleID: "akia", Severity: "hard", Pattern: `\bAKIA[0-9A-Z]{16}\b`},
		},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	eng := h.(*RulePackEngine)
	text := "my ssn is 123-45-6789 ok"
	mkInput := func() *HookInput {
		return &HookInput{Normalized: PayloadFromTextSegments([]string{text})}
	}
	emptyMatched := map[[2]int]struct{}{} // simulate a scan that dropped every hit

	// complete=false → fail-safe: re-localise all rules, SSN masked despite no hits.
	failSafe, err := eng.executeRedact(mkInput(), &HookResult{Decision: Approve}, emptyMatched, false, time.Now())
	if err != nil {
		t.Fatalf("executeRedact: %v", err)
	}
	if len(failSafe.TransformSpans) != 1 {
		t.Fatalf("truncated scan must fail safe and still mask; got %d spans: %+v", len(failSafe.TransformSpans), failSafe.TransformSpans)
	}
	if got := text[failSafe.TransformSpans[0].Start:failSafe.TransformSpans[0].End]; got != "123-45-6789" {
		t.Errorf("fail-safe span must cover the SSN; got %q", got)
	}

	// complete=true with the same empty matched set → trusts the matcher → no mask.
	trusted, err := eng.executeRedact(mkInput(), &HookResult{Decision: Approve}, emptyMatched, true, time.Now())
	if err != nil {
		t.Fatalf("executeRedact: %v", err)
	}
	if len(trusted.TransformSpans) != 0 {
		t.Errorf("complete scan with no matched rules must not mask (proves the fail-safe is what masks); got %+v", trusted.TransformSpans)
	}
}
