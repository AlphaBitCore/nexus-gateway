package ingress_test

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
)

// A caller's reasoning intent has to survive leaving this wire.
//
// The extension carries the caller's exact figure back to an Anthropic target,
// but nothing else can read it. The canonical level is the only thing an
// OpenAI- or Gemini-shaped target sees, so a request that says nothing there
// has said nothing at all — and for `disabled` that is worse than silence: the
// caller turned reasoning OFF and would be billed for a model that reasons.
func TestMessagesRequestToCanonical_StatesTheReasoningAskInTheCanonicalVocabulary(t *testing.T) {
	const head = `{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]`

	for _, tc := range []struct {
		name       string
		thinking   string
		wantEffort string // "" = the field must be absent
	}{
		{"disabled is the one ask a level states exactly", `,"thinking":{"type":"disabled"}`, "none"},
		{"a small budget is a low effort", `,"thinking":{"type":"enabled","budget_tokens":1024}`, "low"},
		{"a mid budget is a medium effort", `,"thinking":{"type":"enabled","budget_tokens":5000}`, "medium"},
		{"a large budget is a high effort", `,"thinking":{"type":"enabled","budget_tokens":8000}`, "high"},
		{"enabled with no budget still asks to reason", `,"thinking":{"type":"enabled"}`, "medium"},
		{"no thinking block asks nothing", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(head+tc.thinking+`}`), "m")
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			got := gjson.GetBytes(out, "reasoning_effort")
			if tc.wantEffort == "" {
				if got.Exists() {
					t.Fatalf("reasoning_effort = %q, want absent — a level nobody asked for is a "+
						"request for reasoning the caller never agreed to pay for", got.String())
				}
				return
			}
			if got.String() != tc.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q", got.String(), tc.wantEffort)
			}
		})
	}
}

// The level is a translation, not a replacement: an Anthropic target must still
// get the caller's own figure rather than one bucketed and un-bucketed through
// four words.
func TestMessagesRequestToCanonical_KeepsTheCallersExactBudgetForItsOwnWire(t *testing.T) {
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(
		[]byte(`{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],`+
			`"thinking":{"type":"enabled","budget_tokens":5000}}`), "m")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if b := gjson.GetBytes(out, "nexus.ext.anthropic.thinking.budget_tokens").Int(); b != 5000 {
		t.Fatalf("extension budget = %d, want 5000 — bucketing to a level must not cost the "+
			"caller the number they chose", b)
	}
	if e := gjson.GetBytes(out, "reasoning_effort").String(); e != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium alongside the exact budget", e)
	}
}
