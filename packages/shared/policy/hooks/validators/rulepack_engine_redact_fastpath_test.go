package validators

import (
	"testing"
	"time"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestRulePackEngine_Redact_BenignNoMatchContract locks the zero-match
// contract of the redact path on a COMPLETE scan: benign content yields an
// untouched Approve result — no tags, no spans, no modified content. The
// zero-match early return must be behaviorally identical to walking the
// full rule loop with an empty hit set.
func TestRulePackEngine_Redact_BenignNoMatchContract(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "ssn", Category: "pii.ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	res, err := h.Execute(t.Context(), &HookInput{Normalized: PayloadFromTextSegments([]string{"a perfectly benign sentence"})})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("benign content must Approve, got %s", res.Decision)
	}
	if len(res.Tags) != 0 || len(res.TransformSpans) != 0 || len(res.ModifiedContent) != 0 {
		t.Errorf("benign content must leave the result untouched; tags=%v spans=%v modified=%v",
			res.Tags, res.TransformSpans, res.ModifiedContent)
	}
}

// TestRulePackEngine_Redact_IncompleteScanFailSafe guards the fail-safe the
// zero-match early return must NOT swallow: when the matcher scan was
// INCOMPLETE, an empty hit set proves nothing — executeRedact must still
// re-confirm every rule with RE2 and mask the PII the truncated scan missed.
func TestRulePackEngine_Redact_IncompleteScanFailSafe(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "i", PackName: "pii", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "ssn", Category: "pii.ssn", Severity: "hard", Pattern: `\b\d{3}-\d{2}-\d{4}\b`}},
	}})
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	e, ok := h.(*RulePackEngine)
	if !ok {
		t.Fatalf("expected *RulePackEngine, got %T", h)
	}
	input := &HookInput{Normalized: PayloadFromTextSegments([]string{"ssn 123-45-6789 leaked"})}
	result := &core.HookResult{Decision: Approve}
	// Empty matched + complete=false = the truncated-scan worst case.
	res, err := e.executeRedact(input, result, map[[2]int]struct{}{}, false, time.Now())
	if err != nil {
		t.Fatalf("executeRedact: %v", err)
	}
	if res.Decision != core.Modify || len(res.TransformSpans) == 0 {
		t.Fatalf("incomplete scan with empty hit set must still fail-safe redact; decision=%s spans=%d",
			res.Decision, len(res.TransformSpans))
	}
}
