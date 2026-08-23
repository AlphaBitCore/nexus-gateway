package codecs

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The three vendors express one intent three ways, and each parameter used to
// reach only its own wire — so a caller posting Anthropic-shaped `thinking`
// whose `auto` selected an OpenAI model lost the intent entirely. These pin the
// DECODE half: what the caller said reaches the canonical, in their own terms,
// with nothing derived.

func TestReasoning_OpenAIEffortReachesTheCanonical(t *testing.T) {
	body := []byte(`{"model":"o3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	out, err := (&OpenAIChatNormalizer{}).normalizeRequest(body, core.Meta{})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Params == nil || !out.Params.Reasoning.Asked() {
		t.Fatalf("reasoning_effort did not reach the canonical: %+v", out.Params)
	}
	if got := out.Params.Reasoning.Effort; got != "high" {
		t.Errorf("Effort = %q, want the caller's own word %q", got, "high")
	}
	if out.Params.Reasoning.BudgetTokens != nil {
		t.Errorf("a budget was invented from a level: %v — deriving is the codec's job on the "+
			"wire that needs it, and doing it here loses what the caller actually said",
			*out.Params.Reasoning.BudgetTokens)
	}
}

func TestReasoning_AnthropicBudgetReachesTheCanonical(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":10000}}`)
	out, err := (&AnthropicMessagesNormalizer{}).normalizeRequest(body, core.Meta{})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Params == nil || !out.Params.Reasoning.Asked() {
		t.Fatalf("thinking did not reach the canonical: %+v", out.Params)
	}
	if got := out.Params.Reasoning.BudgetTokens; got == nil || *got != 10000 {
		t.Fatalf("BudgetTokens = %v, want the caller's own 10000", got)
	}
	if out.Params.Reasoning.Effort != "" {
		t.Errorf("a level was invented from a budget: %q", out.Params.Reasoning.Effort)
	}
}

// `thinking: {type: "disabled"}` is the caller saying NOT to reason. It carries
// no budget, so it is the one case that cannot round-trip as a quantity — a nil
// budget is indistinguishable from "said nothing". It lands as the one word
// every wire can express.
func TestReasoning_AnthropicDisabledIsAnExpressedIntent(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"disabled"}}`)
	out, err := (&AnthropicMessagesNormalizer{}).normalizeRequest(body, core.Meta{})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Params == nil || !out.Params.Reasoning.Asked() {
		t.Fatalf("a disabled thinking block reached the canonical as nothing, so it is "+
			"indistinguishable from a caller who never mentioned reasoning: %+v", out.Params)
	}
	if got := out.Params.Reasoning.Effort; got != "none" {
		t.Errorf("Effort = %q, want \"none\"", got)
	}
}

func TestReasoning_GeminiBudgetAndVisibilityReachTheCanonical(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"thinkingConfig":{"thinkingBudget":2048,"includeThoughts":true}}}`)
	out, err := (&GeminiGenerateNormalizer{}).normalizeRequest(body, core.Meta{})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Params == nil || !out.Params.Reasoning.Asked() {
		t.Fatalf("thinkingConfig did not reach the canonical: %+v", out.Params)
	}
	if got := out.Params.Reasoning.BudgetTokens; got == nil || *got != 2048 {
		t.Fatalf("BudgetTokens = %v, want 2048", got)
	}
	if got := out.Params.Reasoning.IncludeThoughts; got == nil || !*got {
		t.Fatalf("IncludeThoughts = %v, want true — no other vendor states it, so dropping it "+
			"here loses the only place it is expressed", got)
	}
}

// -1 is Gemini for "you decide", a value neither other vendor has. It is a real
// expression, not an absent one, and translating it is the receiving wire's
// problem — a decoder that normalised it away would delete the request.
func TestReasoning_GeminiDynamicBudgetSurvivesDecoding(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`)
	out, err := (&GeminiGenerateNormalizer{}).normalizeRequest(body, core.Meta{})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Params == nil || out.Params.Reasoning == nil {
		t.Fatalf("a dynamic thinking budget was dropped: %+v", out.Params)
	}
	if got := out.Params.Reasoning.BudgetTokens; got == nil || *got != -1 {
		t.Fatalf("BudgetTokens = %v, want -1 preserved", got)
	}
}

// A request that mentions nothing must leave the field nil — the routing
// constraint reads Asked(), and a struct allocated with nothing in it would
// read as a request that asked to reason.
func TestReasoning_ARequestThatSaysNothingAsksNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		norm func([]byte, core.Meta) (core.NormalizedPayload, error)
	}{
		{"openai", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`,
			(&OpenAIChatNormalizer{}).normalizeRequest},
		{"anthropic", `{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			(&AnthropicMessagesNormalizer{}).normalizeRequest},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`,
			(&GeminiGenerateNormalizer{}).normalizeRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.norm([]byte(tc.body), core.Meta{})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if out.Params != nil && out.Params.Reasoning.Asked() {
				t.Fatalf("a request that mentioned no reasoning reads as one that asked: %+v",
					out.Params.Reasoning)
			}
		})
	}
}
