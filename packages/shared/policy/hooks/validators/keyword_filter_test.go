package validators

import (
	"context"
	"testing"
)

func makeKeywordConfig(patterns []map[string]any, caseSensitive bool) *HookConfig {
	ifaces := make([]any, len(patterns))
	for i, p := range patterns {
		ifaces[i] = p
	}
	return &HookConfig{
		ID:               "kw-1",
		ImplementationID: "keyword-filter",
		Name:             "Test Keyword Filter",
		Config: map[string]any{
			"patterns":      ifaces,
			"caseSensitive": caseSensitive,
		},
	}
}

func TestKeywordFilter_Match(t *testing.T) {
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "secret-project", "category": "confidential", "severity": "hard"},
	}, false)

	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}

	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"tell me about secret-project plans"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("expected REJECT_HARD, got %s", result.Decision)
	}
	if result.ReasonCode != "KEYWORD_BLOCKED" {
		t.Errorf("expected reasonCode KEYWORD_BLOCKED, got %s", result.ReasonCode)
	}
	if result.Reason != "keyword matched: confidential" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

func TestKeywordFilter_NoMatch(t *testing.T) {
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "forbidden-word", "category": "blocked", "severity": "hard"},
	}, false)

	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}

	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is perfectly fine content"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("expected APPROVE, got %s", result.Decision)
	}
}

func TestKeywordFilter_RedactAction(t *testing.T) {
	// Per-pattern severity is gone; operators set the single onMatch.action.
	// action=redact yields a Modify decision and stamps the redact action.
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "maybe-bad", "category": "review"},
	}, false)
	cfg.Config["onMatch"] = map[string]any{"action": "redact"}

	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}

	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this contains maybe-bad content"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Modify {
		t.Errorf("expected MODIFY (action=redact), got %s", result.Decision)
	}
	if result.Action != ActionRedact {
		t.Errorf("result.Action = %q, want redact", result.Action)
	}
	// Keyword matches carry no spans (the audit writer degrades to a
	// placeholder); the hook itself emits none.
	if len(result.TransformSpans) != 0 {
		t.Errorf("keyword-filter produces no spans, got %d", len(result.TransformSpans))
	}
}

func TestKeywordFilter_ApproveAction(t *testing.T) {
	// action=approve: the match is recorded (reason/reasonCode/action stamped)
	// but the request flows through unchanged.
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "watch-me", "category": "monitor"},
	}, false)
	cfg.Config["onMatch"] = map[string]any{"action": "approve"}

	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}
	result, err := hook.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"please watch-me closely"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("expected APPROVE (action=approve), got %s", result.Decision)
	}
	if result.Action != ActionApprove {
		t.Errorf("result.Action = %q, want approve", result.Action)
	}
	if result.ReasonCode != "KEYWORD_BLOCKED" {
		t.Errorf("match must still record reasonCode, got %q", result.ReasonCode)
	}
}

func TestKeywordFilter_CaseInsensitive(t *testing.T) {
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "blocked", "category": "test", "severity": "hard"},
	}, false)

	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}

	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is BLOCKED content"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("expected REJECT_HARD for case-insensitive match, got %s", result.Decision)
	}
}

func TestKeywordFilter_InvalidRegex(t *testing.T) {
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "[invalid", "category": "test", "severity": "hard"},
	}, false)

	_, err := NewKeywordFilter(cfg)
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

func TestKeywordFilter_MissingPatternsRejected(t *testing.T) {
	// Without the `patterns` key entirely, the loader must reject —
	// not silently install a no-op filter that lets traffic through.
	cfg := &HookConfig{
		ID:               "kf-x",
		ImplementationID: "keyword-filter",
		Stage:            "request",
		Config:           map[string]any{},
	}
	_, err := NewKeywordFilter(cfg)
	if err == nil {
		t.Fatal("missing patterns should error")
	}
}

func TestKeywordFilter_PatternsNotArrayRejected(t *testing.T) {
	cfg := &HookConfig{
		ID:               "kf-x",
		ImplementationID: "keyword-filter",
		Stage:            "request",
		Config:           map[string]any{"patterns": "not-an-array"},
	}
	_, err := NewKeywordFilter(cfg)
	if err == nil {
		t.Fatal("non-array patterns should error")
	}
}

func TestKeywordFilter_PatternEntryNotObjectRejected(t *testing.T) {
	cfg := &HookConfig{
		ID:               "kf-x",
		ImplementationID: "keyword-filter",
		Stage:            "request",
		Config:           map[string]any{"patterns": []any{"raw-string"}},
	}
	_, err := NewKeywordFilter(cfg)
	if err == nil {
		t.Fatal("non-object pattern entry should error")
	}
}

func TestKeywordFilter_EmptyPatternStringRejected(t *testing.T) {
	cfg := &HookConfig{
		ID:               "kf-x",
		ImplementationID: "keyword-filter",
		Stage:            "request",
		Config: map[string]any{
			"patterns": []any{map[string]any{"pattern": ""}},
		},
	}
	_, err := NewKeywordFilter(cfg)
	if err == nil {
		t.Fatal("empty pattern string should error")
	}
}

func TestKeywordFilter_OnMatchValidationPropagates(t *testing.T) {
	// A bad onMatch field must fail-fast at construction; otherwise an
	// admin-typo'd inflightAction would silently fall back to the default.
	cfg := &HookConfig{
		ID:               "kf-x",
		ImplementationID: "keyword-filter",
		Stage:            "request",
		Config: map[string]any{
			"patterns": []any{map[string]any{"pattern": "secret"}},
			"onMatch":  map[string]any{"inflightAction": "delete-the-user"},
		},
	}
	_, err := NewKeywordFilter(cfg)
	if err == nil {
		t.Fatal("invalid onMatch should be rejected at construction")
	}
}

func TestFlagsForCaseSensitive(t *testing.T) {
	if got := flagsForCaseSensitive(true); got != "" {
		t.Errorf("caseSensitive=true: got %q, want \"\"", got)
	}
	if got := flagsForCaseSensitive(false); got != "i" {
		t.Errorf("caseSensitive=false: got %q, want %q", got, "i")
	}
}

// TestKeywordFilter_LegacyOnMatchKeysMapToAction locks the back-compat
// deprecation window: a config written with the legacy inflightAction /
// storageAction pair is folded to the single action via ActionFromLegacy.
// approve-inflight + redacting-storage upgrades to redact, so the matched
// request becomes a Modify with action=redact. A clean input stamps nothing.
func TestKeywordFilter_LegacyOnMatchKeysMapToAction(t *testing.T) {
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "project-aurora", "category": "confidential", "severity": "hard"},
	}, false)
	cfg.Config["onMatch"] = map[string]any{"inflightAction": "approve", "storageAction": "redact"}
	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}
	input := &HookInput{Normalized: PayloadFromTextSegments([]string{"status of project-aurora please"})}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Modify {
		t.Errorf("decision = %s, want MODIFY (approve+redact storage → redact)", result.Decision)
	}
	if result.Action != ActionRedact {
		t.Errorf("result.Action = %q, want redact", result.Action)
	}

	// A clean input must stamp nothing.
	clean, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{"hello"})})
	if err != nil {
		t.Fatalf("Execute clean: %v", err)
	}
	if clean.Action != "" {
		t.Errorf("clean input stamped action %q", clean.Action)
	}
	if clean.Decision != Approve {
		t.Errorf("clean input decision = %s, want APPROVE", clean.Decision)
	}
}

// TestKeywordFilter_LegacyBlockHardMapsToBlock locks that a legacy
// inflightAction=block-hard config maps to the single block action
// (Decision=RejectHard).
func TestKeywordFilter_LegacyBlockHardMapsToBlock(t *testing.T) {
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "forbidden", "category": "blocked"},
	}, false)
	cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-hard"}
	hook, err := NewKeywordFilter(cfg)
	if err != nil {
		t.Fatalf("NewKeywordFilter: %v", err)
	}
	result, err := hook.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is forbidden"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("decision = %s, want REJECT_HARD (block-hard → block)", result.Decision)
	}
	if result.Action != ActionBlock {
		t.Errorf("result.Action = %q, want block", result.Action)
	}
}
