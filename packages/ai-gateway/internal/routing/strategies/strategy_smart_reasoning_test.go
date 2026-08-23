package strategies

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// reasons builds a catalogue row that declares the reasoning capability.
func reasons(code string, features ...string) core.SmartModelRow {
	return core.SmartModelRow{
		ModelID: code, ModelCode: code, ProviderID: "p1",
		InputModalities: []string{"text"},
		Features:        features,
	}
}

// TestReasoning_ARequestThatAsksToReasonDoesNotGetAModelThatCannot.
//
// A caller who asked a model to think and was answered by one that cannot has
// not been given a cheaper result — they have been given a different KIND of
// result, silently, and the traffic row looks identical to one that was served
// properly. That makes it an ELIGIBILITY constraint and not a preference: the
// request declares the intent, the model has to declare the capability.
func TestReasoning_ARequestThatAsksToReasonDoesNotGetAModelThatCannot(t *testing.T) {
	pool := []core.SmartModelRow{
		reasons("plain", "function_calling"),
		reasons("thinker", "function_calling", "reasoning"),
	}
	kept, dropped, skipped := filterByCapability(pool, false, true, false, reqText, modsOf(reqText))

	if len(kept) != 1 || kept[0].ModelCode != "thinker" {
		t.Fatalf("kept = %v, want only the model that declares reasoning — the caller asked "+
			"for a reasoned answer and would have been handed one produced without any",
			codes(kept))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none — a candidate survived, so the dimension had an "+
			"opinion and must not have been waived", skipped)
	}
}

// The fail-open half, and it matters more here than for most dimensions: a
// deployment whose catalogue predates the reasoning flag declares it nowhere,
// and enforcing a dimension the catalogue has no opinion on would refuse every
// reasoning request rather than route it.
func TestReasoning_ADimensionNoCandidateDeclaresIsWaived(t *testing.T) {
	pool := []core.SmartModelRow{
		reasons("a", "function_calling"),
		reasons("b", "function_calling"),
	}
	kept, dropped, skipped := filterByCapability(pool, false, true, false, reqText, modsOf(reqText))

	if len(kept) != 2 || dropped != 0 {
		t.Fatalf("kept = %v dropped = %d — no row in this pool declares reasoning, so the "+
			"uniform absence is about the CATALOGUE and not about any row; enforcing it "+
			"refuses every reasoning request on a deployment that never set the flag",
			codes(kept), dropped)
	}
	if len(skipped) != 1 || skipped[0] != "reasoning" {
		t.Errorf("skipped = %v, want [reasoning] — a waived dimension has to reach the trace, "+
			"or an operator reading it cannot tell an unenforced rule from a satisfied one",
			skipped)
	}
}

// A row that declares nothing at all passes, the same as every other dimension:
// an empty feature list is absent metadata, not a claim of incapability.
func TestReasoning_AnUndeclaredRowIsNotAClaimOfIncapability(t *testing.T) {
	pool := []core.SmartModelRow{reasons("undeclared"), reasons("thinker", "reasoning")}
	kept, _, _ := filterByCapability(pool, false, true, false, reqText, modsOf(reqText))
	if len(kept) != 2 {
		t.Errorf("kept = %v — a row with no features declared is silent, and reading silence "+
			"as 'cannot reason' takes capability away with nothing in the catalogue saying so",
			codes(kept))
	}
}

// TestReasoning_TheIntentIsReadFromTheCanonical.
//
// The same intent arrives as `reasoning_effort`, `thinking.budget_tokens` or a
// `thinkingConfig` depending on which ingress the caller used. Reading a wire's
// own spelling would filter correctly for one set of callers and not the rest,
// which is the failure the canonical exists to prevent.
func TestReasoning_TheIntentIsReadFromTheCanonical(t *testing.T) {
	budget := 8192
	include := true
	for _, tc := range []struct {
		name string
		p    *normcore.NormalizedPayload
		want bool
	}{
		{"no payload", nil, false},
		{"no params", &normcore.NormalizedPayload{}, false},
		{"params but nothing said", &normcore.NormalizedPayload{Params: &normcore.SamplingParam{}}, false},
		{"a level, as OpenAI callers write it", &normcore.NormalizedPayload{
			Params: &normcore.SamplingParam{Reasoning: &normcore.Reasoning{Effort: "high"}}}, true},
		{"a budget, as Anthropic callers write it", &normcore.NormalizedPayload{
			Params: &normcore.SamplingParam{Reasoning: &normcore.Reasoning{BudgetTokens: &budget}}}, true},
		{"only asking for the thoughts back, as Gemini callers may", &normcore.NormalizedPayload{
			Params: &normcore.SamplingParam{Reasoning: &normcore.Reasoning{IncludeThoughts: &include}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestAsksToReason(tc.p); got != tc.want {
				t.Errorf("requestAsksToReason = %v, want %v", got, tc.want)
			}
		})
	}
}
