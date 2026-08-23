// Named failure modes for the wire-reported thinking split:
//   - non-stream: usage.output_tokens_details.thinking_tokens is the exact
//     count and beats the character-length estimate
//   - stream: the same field on a message_delta reaches the running usage —
//     a stream that drops it reports a reasoning turn as unreasoned
package codecs

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestAnthropicUsage_WireThinkingCountBeatsEstimate(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-5",
	 "content":[{"type":"thinking","thinking":"a fairly long stretch of reasoning text used by the estimator"},
	            {"type":"text","text":"27"}],
	 "usage":{"input_tokens":30,"output_tokens":43,"output_tokens_details":{"thinking_tokens":38}}}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.Usage == nil || got.Usage.ReasoningTokens == nil {
		t.Fatal("reasoning tokens missing entirely")
	}
	if *got.Usage.ReasoningTokens != 38 {
		t.Fatalf("reasoning tokens = %d, want the wire's exact 38 (not the estimate)",
			*got.Usage.ReasoningTokens)
	}
}

func TestAnthropicUsage_EstimateStillCoversWiresWithoutTheSplit(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-4-6",
	 "content":[{"type":"thinking","thinking":"` + string(make([]byte, 0)) + `seventy characters of reasoning text to give the estimator something real"},
	            {"type":"text","text":"27"}],
	 "usage":{"input_tokens":30,"output_tokens":43}}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.Usage == nil || got.Usage.ReasoningTokens == nil || *got.Usage.ReasoningTokens < 1 {
		t.Fatal("estimate fallback lost")
	}
}

func TestAnthropicStreamUsage_ThinkingCountReachesRunningUsage(t *testing.T) {
	var u *core.Usage
	u = MergeAnthropicEventUsage(u, []byte(`{"type":"message_start","message":{"usage":{"input_tokens":30}}}`))
	u = MergeAnthropicEventUsage(u, []byte(`{"type":"message_delta","usage":{"output_tokens":43,"output_tokens_details":{"thinking_tokens":38}}}`))
	if u == nil || u.ReasoningTokens == nil {
		t.Fatal("stream usage dropped the wire's thinking count — the turn reads as unreasoned")
	}
	if *u.ReasoningTokens != 38 {
		t.Fatalf("reasoning tokens = %d, want 38", *u.ReasoningTokens)
	}
	if u.CompletionTokens == nil || *u.CompletionTokens != 43 {
		t.Fatal("output tokens lost alongside")
	}
}
