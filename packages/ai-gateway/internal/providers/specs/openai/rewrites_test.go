package openai

import "testing"

// TestApplyReasoningRewrites_ReasoningModel asserts that calling the
// helper on a reasoning model (gpt-5) with max_tokens present:
//  1. renames max_tokens to max_completion_tokens in the payload
//  2. returns the rewrite descriptor "max_tokens→max_completion_tokens"
func TestApplyReasoningRewrites_ReasoningModel(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"model":      "gpt-5",
		"max_tokens": float64(50),
	}
	rewrites := ApplyReasoningRewrites(payload, "gpt-5")

	if len(rewrites) != 1 || rewrites[0] != "max_tokens→max_completion_tokens" {
		t.Errorf("rewrites: got %v, want [\"max_tokens→max_completion_tokens\"]", rewrites)
	}
	if _, ok := payload["max_tokens"]; ok {
		t.Error("max_tokens must be removed from payload")
	}
	if got := payload["max_completion_tokens"]; got != float64(50) {
		t.Errorf("max_completion_tokens: got %v, want 50", got)
	}
}

// TestApplyReasoningRewrites_NonReasoningModel asserts that a
// non-reasoning model (gpt-4o) produces no rewrites and leaves the
// payload unmodified.
func TestApplyReasoningRewrites_NonReasoningModel(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"model":      "gpt-4o",
		"max_tokens": float64(100),
	}
	rewrites := ApplyReasoningRewrites(payload, "gpt-4o")

	if len(rewrites) != 0 {
		t.Errorf("rewrites: got %v, want empty for non-reasoning model", rewrites)
	}
	if _, ok := payload["max_tokens"]; !ok {
		t.Error("max_tokens must NOT be removed for a non-reasoning model")
	}
}

// TestIsReasoningModel pins the prefix-list of OpenAI reasoning model
// families so adding a new family (e.g. o5, gpt-6) is an explicit
// edit-and-test rather than silent drift.
func TestIsReasoningModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5", true},
		{"gpt-5.4", true},
		{"gpt-5.5-preview", true},
		{"o1", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"o9", true},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4-turbo", false},
		{"gpt-4.1", false},
		{"openai-translator", false}, // looks like "o..." but next char is a letter, not digit
		{"", false},
	}
	for _, tc := range cases {
		got := IsReasoningModel(tc.model)
		if got != tc.want {
			t.Errorf("IsReasoningModel(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}
