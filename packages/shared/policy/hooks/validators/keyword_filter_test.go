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

func TestKeywordFilter_SoftReject(t *testing.T) {
	// Per-pattern severity is gone; operators express block-soft
	// via onMatch.inflightAction.
	cfg := makeKeywordConfig([]map[string]any{
		{"pattern": "maybe-bad", "category": "review"},
	}, false)
	cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-soft"}

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
	if result.Decision != BlockSoft {
		t.Errorf("expected BLOCK_SOFT, got %s", result.Decision)
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

// TestKeywordFilter_StampsStorageActionOnMatch: a keyword match must carry
// the hook's storageAction even when the inflight decision is approve — the
// pipeline stamps storage policy only for non-Approve decisions, and without
// the stamp an "approve inflight, redact/drop storage" policy would persist
// the matched content. Keyword matches carry no spans, so the audit writer
// degrades a redact storage policy to the drop-content placeholder.
func TestKeywordFilter_StampsStorageActionOnMatch(t *testing.T) {
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
	if result.Decision != Approve {
		t.Errorf("decision = %s, want APPROVE (inflight untouched)", result.Decision)
	}
	if string(result.StorageAction) != "redact" {
		t.Errorf("storageAction = %q, want self-stamped redact", result.StorageAction)
	}

	// A clean input must stamp nothing.
	clean, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{"hello"})})
	if err != nil {
		t.Fatalf("Execute clean: %v", err)
	}
	if clean.StorageAction != "" {
		t.Errorf("clean input stamped storageAction %q", clean.StorageAction)
	}
}
