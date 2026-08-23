package strategies

import (
	"reflect"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// Structured output is an ELIGIBILITY constraint, the same shape as reasoning:
// a caller who asked for a schema-constrained answer and got prose has not been
// given a cheaper result, they have been given a different KIND of result — and
// silently, because the wire reports success.
//
// It matters here and nowhere else. When the CALLER names a model, that model's
// limits are the model's: gpt-4-turbo answers 400 "'response_format' of type
// 'json_schema' is not supported" and DeepSeek answers 400 "This response_format
// type is unavailable now", both loud and honest, and a 4xx is
// classNoFailoverNoRetry so it cannot slide onto a worse target. When WE pick
// the model, a mismatch caused by our pick is ours — and one candidate does not
// fail loudly at all: probed 2026-08-19, kimi-k2.5 accepts the field, returns
// HTTP 200 with finish_reason=stop, and answers in prose. Passthrough cannot
// make that honest, so the router must not choose it for such a request.
func withFeatures(code string, features ...string) core.SmartModelRow {
	return core.SmartModelRow{
		ModelID: code, ModelCode: code, ProviderID: "p1",
		InputModalities: []string{"text"}, Features: features,
	}
}

func TestFilterByCapability_StructuredOutputDropsAModelThatCannotHonourIt(t *testing.T) {
	kept, dropped, skipped := filterByCapability(
		[]core.SmartModelRow{
			withFeatures("claude-sonnet-4-6", "streaming", "structured_outputs"),
			withFeatures("kimi-k2.5", "streaming", "function_calling"),
		},
		false, false, true, reqText, modsOf(reqText))

	if len(kept) != 1 || kept[0].ModelCode != "claude-sonnet-4-6" {
		t.Fatalf("kept = %v, want only claude-sonnet-4-6", codes(kept))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none — the pool did not empty", skipped)
	}
}

// A request that did NOT ask for structured output must not have its pool
// narrowed. The dimension is about honouring a stated requirement, not a
// preference for models that happen to carry the feature.
func TestFilterByCapability_StructuredOutputIgnoredWhenNotAsked(t *testing.T) {
	kept, dropped, _ := filterByCapability(
		[]core.SmartModelRow{
			withFeatures("claude-sonnet-4-6", "streaming", "structured_outputs"),
			withFeatures("kimi-k2.5", "streaming", "function_calling"),
		},
		false, false, false, reqText, modsOf(reqText))
	if len(kept) != 2 || dropped != 0 {
		t.Errorf("kept = %v dropped = %d, want the pool untouched", codes(kept), dropped)
	}
}

// The pool rule the other dimensions already follow, applied here too: a row
// that declares NO features at all is undescribed rather than incapable, and a
// dimension that would empty the pool is skipped instead of emptying it. Both
// are deliberate — see filterByCapability. Diverging here for one model's sake
// would make this dimension the odd one out.
func TestFilterByCapability_StructuredOutputKeepsAnUndescribedRow(t *testing.T) {
	kept, dropped, _ := filterByCapability(
		[]core.SmartModelRow{withFeatures("undescribed")},
		false, false, true, reqText, modsOf(reqText))
	if len(kept) != 1 || dropped != 0 {
		t.Errorf("kept = %v dropped = %d, want the undescribed row kept", codes(kept), dropped)
	}
}

func TestFilterByCapability_StructuredOutputSkippedWhenItWouldEmptyThePool(t *testing.T) {
	kept, _, skipped := filterByCapability(
		[]core.SmartModelRow{
			withFeatures("a", "streaming"),
			withFeatures("b", "function_calling"),
		},
		false, false, true, reqText, modsOf(reqText))
	if len(kept) != 2 {
		t.Fatalf("kept = %v, want both — the dimension must yield rather than empty the pool", codes(kept))
	}
	var found bool
	for _, s := range skipped {
		if s == featureStructuredOutputs {
			found = true
		}
	}
	if !found {
		t.Errorf("skipped = %v, want it to name %q so the trace records why", skipped, featureStructuredOutputs)
	}
}

// The dimensions must stay independent: asking for structured output must not
// silently also demand tools or reasoning.
//
// The pool needs a row the dimension MUST drop, or the test cannot tell the two
// explanations apart. Written with only `schema-only` in it, `len(kept) != 1`
// was equally satisfied by a filter that enforced nothing at all — the row
// survives either way — so it passed under the mutant that deletes the
// structured-output pass outright, which is the mutant it exists to catch.
//
// `tools-only` is that row: it declares a feature and not this one, so it must
// go. And the surviving set is compared exactly rather than counted, because a
// count cannot see a substitution.
func TestFilterByCapability_StructuredOutputDoesNotImplyOtherFeatures(t *testing.T) {
	kept, _, skipped := filterByCapability(
		[]core.SmartModelRow{
			withFeatures("schema-only", "structured_outputs"),
			withFeatures("schema-and-reasoning", "structured_outputs", "reasoning"),
			withFeatures("tools-only", "function_calling"),
		},
		false, false, true, reqText, modsOf(reqText))

	if len(skipped) != 0 {
		t.Fatalf("the dimension yielded (%v) — two rows declare the tag, so it had no "+
			"reason to, and a yielded dimension restores the whole pool", skipped)
	}
	want := []string{"schema-only", "schema-and-reasoning"}
	if got := codes(kept); !reflect.DeepEqual(got, want) {
		t.Errorf("kept = %v, want %v.\n"+
			"  schema-only proves the request did not also start demanding tools or reasoning;\n"+
			"  tools-only proves the dimension is doing anything at all", got, want)
	}
}
