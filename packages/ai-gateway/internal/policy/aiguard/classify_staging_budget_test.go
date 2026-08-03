package aiguard

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
)

// Judge input contract: the default staging strategy is full_truncated
// (maximal bounded coverage — cross-turn violations stay visible), the
// judge prompt template is counted against the input budget, and the
// forwarded content is always bounded by shared budget enforcement
// rather than a local hard cut.

// TestApplyInputStaging_DefaultStrategy_IsFullTruncated: with no
// configured InputStrategy, every turn that fits must reach the judge —
// system_plus_last_user (the old default) would silently drop the middle
// turns, blinding the judge to cross-turn violation assembly.
func TestApplyInputStaging_DefaultStrategy_IsFullTruncated(t *testing.T) {
	req := Request{
		DetectorType: "pi",
		Messages: []inputstaging.Message{
			{Role: "user", Content: "step one of the payload"},
			{Role: "assistant", Content: "intermediate reply"},
			{Role: "user", Content: "step two of the payload"},
		},
	}
	cfg := &RuntimeConfig{ModelContextLimit: 4096}
	got := applyInputStaging(req, cfg)
	for _, want := range []string{"step one of the payload", "intermediate reply", "step two of the payload"} {
		if !strings.Contains(got, want) {
			t.Errorf("default staging dropped %q; full_truncated must keep every turn that fits\ngot: %q", want, got)
		}
	}
}

// TestApplyInputStaging_PromptTemplateCountedIntoBudget: the judge prompt
// wraps the staged content, so its tokens consume the same window. A big
// template must shrink the content budget accordingly.
func TestApplyInputStaging_PromptTemplateCountedIntoBudget(t *testing.T) {
	msg := strings.Repeat("a", 6000) // ≈1500 tokens
	req := Request{
		DetectorType: "pi",
		Messages:     []inputstaging.Message{{Role: "user", Content: msg}},
	}
	cfg := &RuntimeConfig{
		ModelContextLimit: 8192,
		PromptTemplate:    strings.Repeat("t", 28000), // ≈7000 tokens of template
	}
	got := applyInputStaging(req, cfg)
	// budget = 8192 − 7000 (template) − 512 (reserve) = 680; the 1500-token
	// message must be bounded to it. Without template accounting the old
	// budget (7680) would pass the message through untouched.
	if got == msg {
		t.Fatalf("content passed through untouched; template tokens were not counted into the budget")
	}
	if got == "" {
		t.Fatalf("bounded content must not be empty")
	}
	if est := inputstaging.EstimateTokens(got); est > 680 {
		t.Errorf("content estimates %d tokens, want <= 680 (window − template − reserve)", est)
	}
}

// TestApplyInputStaging_OversizedMessage_BoundedByEnforcement: shared
// budget enforcement (not a local cut) bounds a single oversized turn.
func TestApplyInputStaging_OversizedMessage_BoundedByEnforcement(t *testing.T) {
	msg := strings.Repeat("a", 8000) // ≈2000 tokens
	req := Request{
		DetectorType: "pi",
		Messages:     []inputstaging.Message{{Role: "user", Content: msg}},
	}
	cfg := &RuntimeConfig{ModelContextLimit: 1024}
	got := applyInputStaging(req, cfg)
	if got == "" {
		t.Fatalf("bounded content must not be empty")
	}
	// budget = 1024 − 512 reserve = 512.
	if est := inputstaging.EstimateTokens(got); est > 512 {
		t.Errorf("content estimates %d tokens, want <= 512", est)
	}
	if !strings.HasSuffix(msg, got) {
		t.Errorf("bounded content must keep the newest tail of the turn")
	}
}

// TestApplyInputStaging_TemplateEatsWindow_FloorsAndStaysBounded: when
// the judge prompt template consumes (nearly) the whole window, the
// content budget floors instead of collapsing to the zero-budget
// fail-open — the forwarded content stays bounded and non-empty, never
// the full unbounded conversation.
func TestApplyInputStaging_TemplateEatsWindow_FloorsAndStaysBounded(t *testing.T) {
	msg := strings.Repeat("a", 8000) // ≈2000 tokens
	req := Request{
		DetectorType: "pi",
		Messages:     []inputstaging.Message{{Role: "user", Content: msg}},
	}
	cfg := &RuntimeConfig{
		ModelContextLimit: 8192,
		PromptTemplate:    strings.Repeat("t", 31000), // ≈7750 tokens > 8192 − 512 − floor
	}
	got := applyInputStaging(req, cfg)
	if got == "" {
		t.Fatalf("floored budget must still forward some content")
	}
	if got == msg {
		t.Fatalf("template-eats-window must not fail open to the full unbounded conversation")
	}
	if est := inputstaging.EstimateTokens(got); est > 256 {
		t.Errorf("content estimates %d tokens, want <= 256 (floor)", est)
	}
}
