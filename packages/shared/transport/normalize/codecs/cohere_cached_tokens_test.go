package codecs_test

import (
	"context"
	"testing"

	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Cohere reports prompt-cache reads as usage.cached_tokens, a sibling of
// tokens and billed_units rather than a member of either. Nothing parsed it,
// so every Cohere turn reported zero cache reads and the Traffic drawer showed
// no cache benefit on a provider that has one. Observed live: 992 cached of
// 1431 input tokens on a single call.
func TestCohereChat_CachedTokensReachTheCanonical(t *testing.T) {
	body := []byte(`{
	  "id":"c1","finish_reason":"COMPLETE",
	  "message":{"role":"assistant","content":[{"type":"text","text":"Paris."}]},
	  "usage":{
	    "billed_units":{"input_tokens":31,"output_tokens":14},
	    "tokens":{"input_tokens":1431,"output_tokens":24},
	    "cached_tokens":992
	  }
	}`)

	n := normcodecs.NewCohereChatNormalizer()
	out, err := n.Normalize(context.Background(), body, normcore.Meta{Direction: normcore.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Usage == nil {
		t.Fatal("no usage extracted")
	}
	if out.Usage.CacheReadTokens == nil {
		t.Fatal("cached_tokens dropped — a cached Cohere turn reports no cache benefit at all")
	}
	if *out.Usage.CacheReadTokens != 992 {
		t.Errorf("CacheReadTokens=%d want 992", *out.Usage.CacheReadTokens)
	}
	// PromptTokens carries the BILLED basis, not the parser's total. Cohere
	// documents billed_units as "the tokens that you're actually billed for",
	// excluding tokens it adds under the hood, and chatCostFormula prices
	// PromptTokens directly — so this fixture's 1431-vs-31 spread is the
	// difference between charging the caller for what they used and charging
	// them 46x that.
	if out.Usage.PromptTokens == nil || *out.Usage.PromptTokens != 31 {
		t.Errorf("PromptTokens=%v want 31 (billed_units), not the 1431 parser total", out.Usage.PromptTokens)
	}
	if out.Usage.CompletionTokens == nil || *out.Usage.CompletionTokens != 14 {
		t.Errorf("CompletionTokens=%v want 14 (billed_units)", out.Usage.CompletionTokens)
	}
	// The cache read is still a SUBSET marker rather than a deduction — it is
	// reported, not subtracted, and chatCostFormula does not price it.
}

// A response that omits billed_units must still yield counts. The billed
// basis is the preference, not a requirement.
func TestCohereChat_NoBilledUnits_FallsBackToTokens(t *testing.T) {
	body := []byte(`{
	  "id":"c3","finish_reason":"COMPLETE",
	  "message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},
	  "usage":{"tokens":{"input_tokens":40,"output_tokens":7}}
	}`)
	n := normcodecs.NewCohereChatNormalizer()
	out, err := n.Normalize(context.Background(), body, normcore.Meta{Direction: normcore.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Usage == nil || out.Usage.PromptTokens == nil || *out.Usage.PromptTokens != 40 {
		t.Errorf("PromptTokens=%v want 40 from the tokens fallback", out.Usage)
	}
}

// A turn with no cache read must not invent one.
func TestCohereChat_NoCachedTokens_LeavesCacheReadUnset(t *testing.T) {
	body := []byte(`{
	  "id":"c2","finish_reason":"COMPLETE",
	  "message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},
	  "usage":{"tokens":{"input_tokens":10,"output_tokens":2}}
	}`)
	n := normcodecs.NewCohereChatNormalizer()
	out, err := n.Normalize(context.Background(), body, normcore.Meta{Direction: normcore.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Usage == nil {
		t.Fatal("no usage extracted")
	}
	if out.Usage.CacheReadTokens != nil {
		t.Errorf("CacheReadTokens=%d, want unset when the response reports none", *out.Usage.CacheReadTokens)
	}
}
