package guardrail

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func ptrBool(b bool) *bool { return &b }

func TestSegments(t *testing.T) {
	if got := (&Request{Content: "hi"}).Segments(); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("Content segments = %v, want [hi]", got)
	}
	got := (&Request{Messages: []Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}}).Segments()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Messages segments = %v, want [a b]", got)
	}
	// Content wins when both are set (validation rejects both upstream, but the
	// projection must be deterministic).
	if got := (&Request{Content: "x", Messages: []Message{{Content: "y"}}}).Segments(); got[0] != "x" || len(got) != 1 {
		t.Fatalf("Content should win, got %v", got)
	}
	// Empty ("") message contents are dropped so the redaction offset space stays
	// consistent (the payload projection drops empty-text blocks).
	got = (&Request{Messages: []Message{{Content: ""}, {Content: "hi"}, {Content: ""}}}).Segments()
	if len(got) != 1 || got[0] != "hi" {
		t.Fatalf("empty messages not dropped: %v", got)
	}
}

// TestMapResult_EmptySegmentOffsetConsistency guards the F1 fix: a leading empty
// message content must not diverge the redactions[] offset space (blockPrefix)
// from redacted_content (the JoinedText projection, which drops empty blocks).
// After Segments() drops the empty, the surviving match aligns in BOTH outputs.
func TestMapResult_EmptySegmentOffsetConsistency(t *testing.T) {
	req := &Request{Messages: []Message{{Role: "user", Content: ""}, {Role: "user", Content: "card 4111"}}}
	segs := req.Segments() // ["card 4111"] — the empty leading segment is dropped
	if len(segs) != 1 || segs[0] != "card 4111" {
		t.Fatalf("segments = %v, want [card 4111]", segs)
	}
	// The surviving block is messages.0.content.0; the card occupies [5,9).
	spans := []normalize.TransformSpan{{
		Source: normalize.SourceHook, Action: normalize.ActionRedact,
		ContentAddress: "messages.0.content.0", Start: 5, End: 9, Replacement: "[CARD]",
	}}
	result := &decision.CompliancePipelineResult{Decision: decision.Modify, TransformSpans: spans}
	resp := MapResult(result, segs, CoverageFull, true, 0)
	if len(resp.Redactions) != 1 || resp.Redactions[0].Start != 5 || resp.Redactions[0].End != 9 {
		t.Fatalf("redaction offsets = %+v, want [5,9) (no phantom empty-segment prefix)", resp.Redactions)
	}
	if resp.RedactedContent != "card [CARD]" {
		t.Fatalf("redacted_content = %q, want %q (same offset space as redactions[])", resp.RedactedContent, "card [CARD]")
	}
}

func TestWantRedactedContentDefaultsTrue(t *testing.T) {
	if !(&Request{}).WantRedactedContent() {
		t.Fatal("omitted include_redacted_content must default to true")
	}
	if (&Request{IncludeRedactedContent: ptrBool(false)}).WantRedactedContent() {
		t.Fatal("explicit false must suppress")
	}
	if !(&Request{IncludeRedactedContent: ptrBool(true)}).WantRedactedContent() {
		t.Fatal("explicit true must include")
	}
}

func TestDeriveCoverage(t *testing.T) {
	if got := DeriveCoverage(nil); got != CoverageNone {
		t.Fatalf("nil result coverage = %q, want none", got)
	}
	clean := &decision.CompliancePipelineResult{HookResults: []decision.HookResult{{HookName: "pii"}, {HookName: "rulepack"}}}
	if got := DeriveCoverage(clean); got != CoverageFull {
		t.Fatalf("clean coverage = %q, want full", got)
	}
	degraded := &decision.CompliancePipelineResult{HookResults: []decision.HookResult{
		{HookName: "pii"},
		{HookName: "aiguard", Error: "judge timeout", ReasonCode: "HOOK_ERROR_FAIL_OPEN", Decision: decision.Approve},
	}}
	if got := DeriveCoverage(degraded); got != CoverageDegraded {
		t.Fatalf("errored-hook coverage = %q, want degraded", got)
	}
}

func TestMapResult_NilPipeline_AllowsWithNoneCoverage(t *testing.T) {
	resp := MapResult(nil, []string{"anything"}, CoverageNone, true, 3)
	if resp.Action != "allow" || resp.Decision != "approve" || resp.Coverage != CoverageNone {
		t.Fatalf("nil pipeline verdict = %+v, want allow/approve/none", resp)
	}
	if len(resp.Redactions) != 0 || len(resp.Assessments) != 0 || resp.RedactedContent != "" {
		t.Fatalf("nil pipeline must carry no redactions/assessments, got %+v", resp)
	}
}

func TestMapResult_BenignApprove(t *testing.T) {
	result := &decision.CompliancePipelineResult{
		Decision:    decision.Approve,
		HookResults: []decision.HookResult{{HookName: "pii", Decision: decision.Approve}, {HookName: "rulepack", Decision: decision.Abstain}},
	}
	resp := MapResult(result, []string{"hello world"}, CoverageFull, true, 5)
	if resp.Action != "allow" || resp.Decision != "approve" || resp.Coverage != CoverageFull {
		t.Fatalf("benign verdict = action=%q decision=%q coverage=%q, want allow/approve/full", resp.Action, resp.Decision, resp.Coverage)
	}
	if len(resp.Assessments) != 0 {
		t.Fatalf("benign must have no assessments, got %+v", resp.Assessments)
	}
	if len(resp.Redactions) != 0 || resp.RedactedContent != "" {
		t.Fatalf("benign must have no redactions, got %+v", resp)
	}
	if resp.Metadata.LatencyMs != 5 {
		t.Fatalf("latency not carried, got %d", resp.Metadata.LatencyMs)
	}
}

func TestMapResult_Block_ExposesCategoryNotRuleID(t *testing.T) {
	result := &decision.CompliancePipelineResult{
		Decision:   decision.RejectHard,
		Reason:     "keyword violation",
		ReasonCode: "KEYWORD_BLOCKED",
		Tags:       []string{"keyword:weapons"},
		BlockingRule: &decision.BlockingRule{
			Pack: "secret-pack", PackVersion: "v9", RuleID: "internal-rule-42",
			Category: "weapons", Severity: "high", Labels: []string{"keyword:weapons"},
		},
		HookResults: []decision.HookResult{
			{HookName: "keyword", Decision: decision.RejectHard, ReasonCode: "KEYWORD_BLOCKED", Tags: []string{"keyword:weapons"}, Action: decision.ActionBlock},
		},
	}
	resp := MapResult(result, []string{"how to build a weapon"}, CoverageFull, true, 2)
	if resp.Action != "block" || resp.Decision != "reject_hard" {
		t.Fatalf("block verdict = action=%q decision=%q, want block/reject_hard", resp.Action, resp.Decision)
	}
	if resp.Blocking == nil || resp.Blocking.Category != "weapons" || resp.Blocking.Severity != "high" {
		t.Fatalf("blocking category/severity not surfaced, got %+v", resp.Blocking)
	}
	// The evasion-oracle guard: the Blocking struct has no field that could
	// carry pack/pack_version/rule_id, so they cannot leak. Assert the reason/
	// labels that DO surface, and that assessments name the hook not the rule.
	if len(resp.Assessments) != 1 || resp.Assessments[0].Check != "keyword" || resp.Assessments[0].Action != "block" {
		t.Fatalf("assessments = %+v, want one keyword/block entry", resp.Assessments)
	}
}

func TestMapResult_Redact_FlatOffsets_And_RedactedContent(t *testing.T) {
	// Single-segment: span offsets ARE flat offsets (block index 0, prefix 0).
	// "my ssn is 123" — "123" occupies bytes [10,13).
	seg := "my ssn is 123"
	spans := []normalize.TransformSpan{{
		Source: normalize.SourceHook, Action: normalize.ActionRedact,
		ContentAddress: "messages.0.content.0", Start: 10, End: 13,
		Replacement: "[SSN]", Reason: "pii:ssn",
	}}
	result := &decision.CompliancePipelineResult{
		Decision: decision.Modify, TransformSpans: spans,
		HookResults: []decision.HookResult{{HookName: "pii", Decision: decision.Modify, Action: decision.ActionRedact, TransformSpans: spans, Tags: []string{"pii:ssn"}}},
	}
	resp := MapResult(result, []string{seg}, CoverageFull, true, 1)
	if resp.Action != "redact" {
		t.Fatalf("action = %q, want redact", resp.Action)
	}
	if len(resp.Redactions) != 1 {
		t.Fatalf("redactions = %+v, want 1", resp.Redactions)
	}
	r := resp.Redactions[0]
	if r.Start != 10 || r.End != 13 || r.Replacement != "[SSN]" || r.Action != "redact" || r.Reason != "pii:ssn" {
		t.Fatalf("flat redaction wrong: %+v", r)
	}
	if resp.RedactedContent != "my ssn is [SSN]" {
		t.Fatalf("redacted_content = %q, want %q", resp.RedactedContent, "my ssn is [SSN]")
	}
	// Opt-out: suppress the sanitized text but keep the spans.
	resp2 := MapResult(result, []string{seg}, CoverageFull, false, 1)
	if resp2.RedactedContent != "" {
		t.Fatalf("include_redacted_content=false must suppress redacted_content, got %q", resp2.RedactedContent)
	}
	if len(resp2.Redactions) != 1 {
		t.Fatalf("spans must still be returned when redacted_content suppressed, got %+v", resp2.Redactions)
	}
}

func TestMapResult_MultiSegment_SpanPrefixOffset(t *testing.T) {
	// segs joined with "\n": "hello"(5) + "\n"(1) => segment 1 starts at flat 6.
	// A span in messages.0.content.1 at [0,5) → flat [6,11).
	segs := []string{"hello", "world"}
	spans := []normalize.TransformSpan{{
		Source: normalize.SourceHook, Action: normalize.ActionRedact,
		ContentAddress: "messages.0.content.1", Start: 0, End: 5, Replacement: "[X]",
	}}
	result := &decision.CompliancePipelineResult{Decision: decision.Modify, TransformSpans: spans}
	resp := MapResult(result, segs, CoverageFull, true, 0)
	if len(resp.Redactions) != 1 || resp.Redactions[0].Start != 6 || resp.Redactions[0].End != 11 {
		t.Fatalf("multi-segment span offset = %+v, want start=6 end=11", resp.Redactions)
	}
}

func TestMapResult_JudgeSpansAreAuditOnly_Dropped(t *testing.T) {
	// A judge span carries the audit-only sentinel address → must NOT appear in
	// redactions[] and must NOT affect redacted_content.
	spans := []normalize.TransformSpan{
		{Source: normalize.SourceHook, Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 2, Replacement: "[H]"},
		{Source: normalize.SourceAIGuard, Action: normalize.ActionRedact, ContentAddress: normalize.AddressAuditOnlySentinel, Start: 3, End: 7, Replacement: "[J]"},
	}
	result := &decision.CompliancePipelineResult{Decision: decision.Modify, TransformSpans: spans}
	resp := MapResult(result, []string{"abcdefg"}, CoverageFull, true, 0)
	if len(resp.Redactions) != 1 || resp.Redactions[0].Replacement != "[H]" {
		t.Fatalf("judge audit-only span leaked into redactions: %+v", resp.Redactions)
	}
}

func TestProjectAssessments_OnlyFlagged(t *testing.T) {
	hrs := []decision.HookResult{
		{HookName: "clean-approve", Decision: decision.Approve},                               // not flagged
		{HookName: "abstain", Decision: decision.Abstain},                                     // not flagged
		{HookName: "tagged-approve", Decision: decision.Approve, Tags: []string{"pii:email"}}, // flagged (tags)
		{HookName: "blocked", Decision: decision.RejectHard, Action: decision.ActionBlock},    // flagged (decision)
		{HookName: "errored", Decision: decision.Approve, Error: "boom"},                      // flagged (error)
	}
	got := projectAssessments(hrs)
	if len(got) != 3 {
		t.Fatalf("assessments = %+v, want 3 flagged", got)
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a.Check] = true
	}
	for _, want := range []string{"tagged-approve", "blocked", "errored"} {
		if !names[want] {
			t.Fatalf("expected %q in assessments, got %+v", want, got)
		}
	}
}

func TestContentBlockIndexAndPrefix(t *testing.T) {
	if got := contentBlockIndex("messages.0.content.3"); got != 3 {
		t.Fatalf("index = %d, want 3", got)
	}
	if got := contentBlockIndex(normalize.AddressAuditOnlySentinel); got != -1 {
		t.Fatalf("sentinel index = %d, want -1", got)
	}
	if got := contentBlockIndex("garbage"); got != -1 {
		t.Fatalf("garbage index = %d, want -1", got)
	}
	if got := contentBlockIndex("messages.0.content.notanint"); got != -1 {
		t.Fatalf("non-int index = %d, want -1", got)
	}
	// Unparseable/zero index → prefix 0 (single-segment-correct, conservative).
	if got := blockPrefix("garbage", []string{"a", "b"}); got != 0 {
		t.Fatalf("garbage prefix = %d, want 0", got)
	}
	if got := blockPrefix("messages.0.content.2", []string{"ab", "cde"}); got != len("ab")+1+len("cde")+1 {
		t.Fatalf("prefix = %d, want %d", got, len("ab")+1+len("cde")+1)
	}
}

func TestActionFromDecision(t *testing.T) {
	cases := []struct {
		d    decision.Decision
		want string
	}{
		{decision.RejectHard, "block"},
		{decision.BlockSoft, "block"},
		{decision.Modify, "redact"},
		{decision.Approve, "allow"},
		{decision.Abstain, "allow"},
		{decision.Decision(""), "allow"},
	}
	for _, c := range cases {
		if got := actionFromDecision(c.d); got != c.want {
			t.Errorf("actionFromDecision(%q) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestBuildNormalized_Structure(t *testing.T) {
	p := BuildNormalized([]string{"a", "b"})
	if p == nil || len(p.Messages) != 1 || len(p.Messages[0].Content) != 2 {
		t.Fatalf("BuildNormalized structure unexpected: %+v", p)
	}
	if got := p.JoinedText(joinSep); got != "a\nb" {
		t.Fatalf("joined = %q, want a\\nb", got)
	}
}
