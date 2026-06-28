package validators

// Coverage-completing tests for the cheap error/edge branches the existing
// suites leave uncovered. Every case asserts an observable outcome — the hook
// decision, the matched-rule attribution, the precise span/address, the exact
// construction error, or the prefilter's verdict — not bare line execution.

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// closer is the optional interface every content hook exposes; the RE2 build's
// Close is a no-op that must still return nil so a config swap never errors.
type closer interface{ Close() error }

// TestHooks_Close_NoOpOnRE2 asserts Close() on each content hook is a clean
// no-op under the default RE2 matcher (no native resources to free). This is
// the eviction path the pipeline calls on a config swap.
func TestHooks_Close_NoOpOnRE2(t *testing.T) {
	build := map[string]func() (Hook, error){
		"keyword-filter": func() (Hook, error) {
			return NewKeywordFilter(makeKeywordConfig([]map[string]any{
				{"pattern": "alpha", "category": "c"},
			}, false))
		},
		"content-safety": func() (Hook, error) {
			cfg := &HookConfig{
				ID: "cs-close", ImplementationID: "content-safety", Name: "cs",
				Config: map[string]any{"patterns": []any{
					map[string]any{"pattern": "beta", "category": "violence"},
				}},
			}
			return NewContentSafety(cfg)
		},
		"pii-detector": func() (Hook, error) {
			return NewPiiDetector(makePiiConfig([]map[string]any{
				{"id": "email", "regex": `\b\w+@\w+\.\w+\b`, "flags": "i"},
			}, "block"))
		},
		"rulepack-engine": func() (Hook, error) {
			return NewRulePackEngine(buildEngineConfig([]rulePackInstall{{
				InstallID: "i", PackName: "p", PackVersion: "v1", Enabled: true,
				Rules: []rulePackRule{{RuleID: "r", Severity: "hard", Pattern: `\bgamma\b`, Flags: "i"}},
			}}))
		},
	}
	for name, mk := range build {
		t.Run(name, func(t *testing.T) {
			h, err := mk()
			if err != nil {
				t.Fatalf("construct %s: %v", name, err)
			}
			c, ok := h.(closer)
			if !ok {
				t.Fatalf("%s does not implement Close()", name)
			}
			if err := c.Close(); err != nil {
				t.Errorf("%s Close() = %v, want nil (RE2 no-op)", name, err)
			}
			// Idempotent: a second close must also be clean.
			if err := c.Close(); err != nil {
				t.Errorf("%s second Close() = %v, want nil", name, err)
			}
		})
	}
}

// TestNewContentPrescan_UnstrippablePatternCollapsesToConservative asserts that
// when a pattern cannot be anchor-stripped, the prescan collapses to nil so
// MayMatchRaw conservatively returns true (never lets the proxy skip extraction
// on the hook's behalf). A leading-then-anchored construct that StripAnchors
// rejects exercises the bail-out branch.
func TestNewContentPrescan_UnstrippablePatternCollapsesToConservative(t *testing.T) {
	// An unparseable regex makes StripAnchors error, which collapses the prescan
	// to the conservative nil matcher. (Guard the precondition so the test fails
	// loudly rather than silently no-op'ing if StripAnchors ever starts tolerating
	// this input.)
	hostile := `(unclosed`
	if _, err := matcher.StripAnchors(hostile); err == nil {
		t.Fatalf("StripAnchors unexpectedly accepted %q; cannot exercise collapse branch", hostile)
	}
	pc := newContentPrescan([]matcher.Pattern{{ID: 0, Expr: hostile, Flags: "i"}})
	if pc.prescan != nil {
		t.Fatal("expected nil prescan after StripAnchors failure")
	}
	if !pc.MayMatchRaw([]byte("anything at all")) {
		t.Error("MayMatchRaw must be conservative (true) when prescan collapsed to nil")
	}
	if err := pc.closePrescan(); err != nil {
		t.Errorf("closePrescan on nil prescan = %v, want nil", err)
	}
}

// TestContentPrescan_MayMatchRaw_EmptyBody asserts the empty-body fast path:
// a built prefilter returns false on empty bytes (nothing can match), letting
// the proxy skip extraction. Uses a real keyword-filter whose prescan is built.
func TestContentPrescan_MayMatchRaw_EmptyBody(t *testing.T) {
	h, err := NewKeywordFilter(makeKeywordConfig([]map[string]any{
		{"pattern": "needle", "category": "c"},
	}, false))
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}
	kf := h.(*KeywordFilter)
	if kf.prescan == nil {
		t.Fatal("expected a built prefilter for a simple literal pattern")
	}
	// A content-scanning hook declares ScansContent()==true: the proxy may only
	// skip extraction when MayMatchRaw is false, never unconditionally.
	if !kf.ScansContent() {
		t.Error("ScansContent must be true for a content-scanning hook")
	}
	if kf.MayMatchRaw([]byte{}) {
		t.Error("MayMatchRaw on empty body must be false")
	}
	if !kf.MayMatchRaw([]byte("here is a needle in the body")) {
		t.Error("MayMatchRaw must be true when the raw body carries the pattern")
	}
	if kf.MayMatchRaw([]byte("no match present here")) {
		t.Error("MayMatchRaw must be false when no pattern is present in a non-empty body")
	}
}

// TestPiiDetector_Approve_LuhnSkipKeepsScanning covers executeApprove's
// Luhn-skip continue branch: a number that fires the regex but FAILS Luhn must
// not produce a PII tag, while a valid card in the same body must. Asserts the
// detect-only outcome (tags present, payload untouched, decision Approve).
func TestPiiDetector_Approve_LuhnSkipKeepsScanning(t *testing.T) {
	cfg := makePiiConfig([]map[string]any{
		{"id": "credit_card", "regex": `\b\d{16}\b`, "flags": "", "luhn": true},
	}, "approve")

	// 4111111111111111 is a Luhn-valid Visa test number; 1234567812345670... is
	// crafted to fail Luhn. First a body with ONLY a Luhn-invalid number: the
	// continue branch fires for every match and the scan finds nothing.
	luhnInvalid := "card 1234567812345678 here"
	res, err := mustExec(t, cfg, luhnInvalid)
	if err != nil {
		t.Fatalf("Execute(invalid): %v", err)
	}
	if res.Decision != Approve {
		t.Fatalf("Luhn-invalid only: Decision = %s, want APPROVE", res.Decision)
	}
	if res.ReasonCode == "PII_DETECTED" {
		t.Error("Luhn-invalid number must not be reported as PII")
	}

	// Now a valid card: detect path runs and tags.
	res2, err := mustExec(t, cfg, "card 4111111111111111 here")
	if err != nil {
		t.Fatalf("Execute(valid): %v", err)
	}
	if res2.ReasonCode != "PII_DETECTED" {
		t.Fatalf("Luhn-valid: ReasonCode = %q, want PII_DETECTED", res2.ReasonCode)
	}
	if !containsTag(res2.Tags, "compliance:pii") {
		t.Errorf("expected compliance:pii tag, got %v", res2.Tags)
	}
	if res2.Decision != Approve {
		t.Errorf("approve action keeps Decision APPROVE, got %s", res2.Decision)
	}
}

// mustExec builds a PiiDetector and runs it over a single chat segment.
func mustExec(t *testing.T, cfg *HookConfig, segment string) (*HookResult, error) {
	t.Helper()
	h, err := NewPiiDetector(cfg)
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	return h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{segment}),
	})
}

// TestPiiDetector_Redact_EmbeddingInputsAddressing covers collectRedactions'
// KindAIEmbedding branch: PII inside Inputs is redacted and the span address is
// "inputs.<index>", with empty inputs skipped. Asserts the precise address and
// the masked replacement.
func TestPiiDetector_Redact_EmbeddingInputsAddressing(t *testing.T) {
	h, err := NewPiiDetector(makePiiConfig([]map[string]any{
		{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`, "flags": "i"},
	}, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	in := &HookInput{
		Normalized: &normalize.NormalizedPayload{
			Kind:             normalize.KindAIEmbedding,
			NormalizeVersion: normalize.SchemaVersion,
			Inputs:           []string{"", "reach me at bob@example.com please"},
		},
	}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Fatalf("Decision = %s, want MODIFY", res.Decision)
	}
	if len(res.TransformSpans) != 1 {
		t.Fatalf("got %d spans, want 1", len(res.TransformSpans))
	}
	sp := res.TransformSpans[0]
	if sp.ContentAddress != "inputs.1" {
		t.Errorf("span address = %q, want inputs.1 (empty input[0] skipped)", sp.ContentAddress)
	}
	if sp.SourceID != "email" {
		t.Errorf("span SourceID = %q, want email", sp.SourceID)
	}
}

// TestPiiDetector_Redact_ReasoningAndToolResultAddressing covers the
// ContentReasoning (scope-gated) and ContentToolResult addressing branches in
// collectRedactions. Scope=include_reasoning opts reasoning blocks in; the
// tool-result output carries its own ".toolResult" address suffix.
func TestPiiDetector_Redact_ReasoningAndToolResultAddressing(t *testing.T) {
	cfg := makePiiConfig([]map[string]any{
		{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`, "flags": "i"},
	}, "redact")
	cfg.Scope = "include_reasoning"

	h, err := NewPiiDetector(cfg)
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	in := &HookInput{
		Normalized: &normalize.NormalizedPayload{
			Kind:             normalize.KindAIChat,
			NormalizeVersion: normalize.SchemaVersion,
			Messages: []normalize.Message{{
				Role: normalize.RoleUser,
				Content: []normalize.ContentBlock{
					{Type: normalize.ContentReasoning, Text: "thinking about r1@reason.io now"},
					{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{Output: "tool said t1@tool.io done"}},
				},
			}},
		},
	}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Fatalf("Decision = %s, want MODIFY", res.Decision)
	}
	addrs := map[string]bool{}
	for _, sp := range res.TransformSpans {
		addrs[sp.ContentAddress] = true
	}
	if !addrs["messages.0.content.0"] {
		t.Errorf("missing reasoning span address messages.0.content.0; got %v", addrs)
	}
	if !addrs["messages.0.content.1.toolResult"] {
		t.Errorf("missing tool-result span address messages.0.content.1.toolResult; got %v", addrs)
	}
}

// TestParseRulePackInstalls_UnsupportedType asserts the typed factory error
// when _rulePackInstalls is neither []rulePackInstall nor []any.
func TestParseRulePackInstalls_UnsupportedType(t *testing.T) {
	cfg := &HookConfig{
		ID: "h", ImplementationID: "rulepack-engine", Name: "n",
		Config: map[string]any{"_rulePackInstalls": "not-a-list"},
	}
	_, err := NewRulePackEngine(cfg)
	if err == nil {
		t.Fatal("expected error for string _rulePackInstalls, got nil")
	}
	if got := err.Error(); !contains(got, "unsupported type") {
		t.Errorf("error = %q, want it to mention unsupported type", got)
	}
}

// TestParseRulePackInstalls_GenericAnyShapeUnmarshalError asserts the []any
// JSON round-trip error path: an element whose JSON shape cannot unmarshal into
// rulePackInstall (a field typed wrong) surfaces an unmarshal error.
func TestParseRulePackInstalls_GenericAnyShapeUnmarshalError(t *testing.T) {
	cfg := &HookConfig{
		ID: "h", ImplementationID: "rulepack-engine", Name: "n",
		Config: map[string]any{
			// enabled must be bool; a string forces json.Unmarshal to fail.
			"_rulePackInstalls": []any{
				map[string]any{"installId": "i", "enabled": "yes-please"},
			},
		},
	}
	_, err := NewRulePackEngine(cfg)
	if err == nil {
		t.Fatal("expected unmarshal error for malformed []any install, got nil")
	}
	if got := err.Error(); !contains(got, "unmarshal _rulePackInstalls") {
		t.Errorf("error = %q, want it to mention unmarshal _rulePackInstalls", got)
	}
}

// TestParseRulePackInstalls_GenericAnyShapeHappyPath asserts the []any path
// also succeeds end-to-end: a JSON-shaped install round-trips and the engine
// blocks on a hard-severity match, proving the unmarshalled rules drive a real
// decision (not just that parsing returned nil error).
func TestParseRulePackInstalls_GenericAnyShapeHappyPath(t *testing.T) {
	cfg := &HookConfig{
		ID: "h", ImplementationID: "rulepack-engine", Name: "n",
		Config: map[string]any{
			"_rulePackInstalls": []any{
				map[string]any{
					"installId": "i", "packName": "p", "packVersion": "v1", "enabled": true,
					"rules": []any{
						map[string]any{"ruleId": "r1", "severity": "hard", "pattern": `\bclassified\b`, "flags": "i"},
					},
				},
			},
		},
	}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is CLASSIFIED material"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Fatalf("Decision = %s, want REJECT_HARD from unmarshalled hard rule", res.Decision)
	}
	if res.BlockingRule == nil || res.BlockingRule.RuleID != "r1" {
		t.Errorf("BlockingRule attribution missing/wrong: %+v", res.BlockingRule)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
