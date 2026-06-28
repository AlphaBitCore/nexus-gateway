package codecs

import (
	"context"
	"reflect"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// usageNormalizer is implemented by every normalizer that has both the full
// Normalize and the usage-only fast path, so the differential test can drive both
// through one interface.
type usageNormalizer interface {
	Normalize(context.Context, []byte, core.Meta) (core.NormalizedPayload, error)
	ExtractUsageOnly(raw []byte) (*core.Usage, bool)
}

// TestExtractUsageOnly_EquivalentToFullNormalize is the load-bearing correctness
// proof for the usage-only fast path: for every format and a spread of realistic response bodies, the
// usage-only fast path must return EXACTLY what full Normalize would put on
// np.Usage — otherwise the gateway would stamp wrong tokens/cost. SSE bodies (and
// parse failures) must DECLINE so the caller falls back to the full path. A
// declined case asserts only the decline, since the full path then runs unchanged.
func TestExtractUsageOnly_EquivalentToFullNormalize(t *testing.T) {
	cases := []struct {
		name        string
		n           usageNormalizer
		body        string
		wantDecline bool // SSE / unparseable → fast path must step aside
	}{
		// ── OpenAI family ──
		{
			name: "openai_full_usage_reasoning_cache",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Paris."},"finish_reason":"stop"}],"usage":{"prompt_tokens":24,"completion_tokens":78,"total_tokens":102,"prompt_tokens_details":{"cached_tokens":16},"completion_tokens_details":{"reasoning_tokens":40}}}`,
		},
		{
			name: "openai_deepseek_cache_hit_alias",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"deepseek-chat","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":64}}`,
		},
		{
			name: "openai_prompt_cache_creation_tokens",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":80,"completion_tokens":12,"total_tokens":92,"prompt_tokens_details":{"cached_tokens":16,"cache_creation_tokens":24}}}`,
		},
		{
			name: "openai_responses_shape_input_output_tokens",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"gpt-5","output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}],"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`,
		},
		{
			name: "openai_no_usage_block",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
		},
		{
			// Moonshot/kimi: reasoning_content present, usage has NO reasoning_tokens →
			// the non-stream normalizer estimates ReasoningTokens from content length.
			// The slim path must reproduce the estimate exactly.
			name: "openai_moonshot_reasoning_content_estimate",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"kimi-k2","choices":[{"index":0,"message":{"role":"assistant","content":"The answer is 42.","reasoning_content":"Let me think step by step about what the user is really asking here."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":50,"total_tokens":80}}`,
		},
		{
			// Explicit reasoning_tokens in usage must WIN over the content estimate.
			name: "openai_explicit_reasoning_beats_content_estimate",
			n:    NewOpenAIChatNormalizer(),
			body: `{"model":"kimi-k2","choices":[{"message":{"role":"assistant","content":"x","reasoning_content":"some long reasoning text here that would estimate higher"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		},
		{
			name:        "openai_sse_declines",
			n:           NewOpenAIChatNormalizer(),
			body:        "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantDecline: true,
		},
		// ── Gemini ──
		{
			name: "gemini_usage_with_thoughts",
			n:    NewGeminiGenerateNormalizer(),
			body: `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":12,"totalTokenCount":50,"thoughtsTokenCount":8}}`,
		},
		{
			name: "gemini_no_usage",
			n:    NewGeminiGenerateNormalizer(),
			body: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
		},
		{
			// Thought parts present but NO thoughtsTokenCount: proves Gemini derives
			// reasoning ONLY from the usage block, not from thought-part content (slim
			// == full == no reasoning estimate).
			name: "gemini_thought_parts_no_thoughts_count",
			n:    NewGeminiGenerateNormalizer(),
			body: `{"candidates":[{"content":{"parts":[{"text":"thinking out loud","thought":true},{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`,
		},
		{
			// usageMetadata present but NO candidates (safety-blocked / prompt-only):
			// the full path gates usage behind candidates and yields nil, so the slim
			// MUST decline rather than bill the prompt tokens. This is the
			// BLOCK case — without the candidates gate the slim over-reports usage.
			name:        "gemini_usage_no_candidates_declines",
			n:           NewGeminiGenerateNormalizer(),
			body:        `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":12,"totalTokenCount":12}}`,
			wantDecline: true,
		},
		{
			name:        "gemini_empty_candidates_array_declines",
			n:           NewGeminiGenerateNormalizer(),
			body:        `{"candidates":[],"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`,
			wantDecline: true,
		},
		{
			name: "gemini_cached_content_tokens",
			n:    NewGeminiGenerateNormalizer(),
			body: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":12,"totalTokenCount":50,"cachedContentTokenCount":20}}`,
		},
		{
			name:        "gemini_sse_declines",
			n:           NewGeminiGenerateNormalizer(),
			body:        "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n",
			wantDecline: true,
		},
		// ── Cohere ──
		{
			name: "cohere_usage",
			n:    NewCohereChatNormalizer(),
			body: `{"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"COMPLETE","usage":{"tokens":{"input_tokens":40,"output_tokens":15}}}`,
		},
		{
			name: "cohere_no_usage",
			n:    NewCohereChatNormalizer(),
			body: `{"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		},
		{
			// tool_plan projects to a ContentReasoning block, but Cohere derives no
			// reasoning estimate from it — usage comes only from the usage block.
			name: "cohere_tool_plan_no_reasoning_estimate",
			n:    NewCohereChatNormalizer(),
			body: `{"message":{"role":"assistant","tool_plan":"I will call the weather tool then summarise.","content":[{"type":"text","text":"hi"}]},"usage":{"tokens":{"input_tokens":40,"output_tokens":15}}}`,
		},
		// ── Replicate ──
		{
			name: "replicate_metrics",
			n:    NewReplicateNormalizer(),
			body: `{"version":"v1","output":["hello"],"metrics":{"input_token_count":33,"output_token_count":9}}`,
		},
		{
			name: "replicate_metrics_zero_counts",
			n:    NewReplicateNormalizer(),
			body: `{"version":"v1","output":["hello"],"metrics":{"input_token_count":0,"output_token_count":0}}`,
		},
		{
			name: "replicate_no_metrics",
			n:    NewReplicateNormalizer(),
			body: `{"version":"v1","output":["hello"]}`,
		},
		// ── Anthropic (the option-B path: usage + thinking-block estimate) ──
		{
			name: "anthropic_usage_with_thinking_blocks",
			n:    NewAnthropicMessagesNormalizer(),
			body: `{"model":"claude-3-7","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"Let me reason carefully about the user's question here in some depth."},{"type":"text","text":"The answer is 42."}],"usage":{"input_tokens":50,"output_tokens":120,"cache_read_input_tokens":16,"cache_creation_input_tokens":8}}`,
		},
		{
			name: "anthropic_thinking_text_fallback",
			n:    NewAnthropicMessagesNormalizer(),
			body: `{"model":"claude-3-7","stop_reason":"end_turn","content":[{"type":"thinking","text":"reasoning under text key"},{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":20}}`,
		},
		{
			name: "anthropic_no_thinking",
			n:    NewAnthropicMessagesNormalizer(),
			body: `{"model":"claude-3-5","stop_reason":"end_turn","content":[{"type":"text","text":"plain answer"}],"usage":{"input_tokens":10,"output_tokens":20}}`,
		},
		{
			// cache_creation only (no plain input_tokens) + redacted_thinking present:
			// redacted_thinking must NOT count toward the reasoning estimate (only
			// type=="thinking" does), and PromptTokens = cacheCreation alone.
			name: "anthropic_cache_creation_only_with_redacted_thinking",
			n:    NewAnthropicMessagesNormalizer(),
			body: `{"model":"claude-3-7","stop_reason":"end_turn","content":[{"type":"redacted_thinking","data":"xxxx"},{"type":"text","text":"ok"}],"usage":{"input_tokens":0,"output_tokens":15,"cache_creation_input_tokens":32}}`,
		},
		{
			name: "anthropic_no_usage",
			n:    NewAnthropicMessagesNormalizer(),
			body: `{"model":"claude-3-5","stop_reason":"end_turn","content":[{"type":"text","text":"x"}]}`,
		},
		{
			name:        "anthropic_sse_declines",
			n:           NewAnthropicMessagesNormalizer(),
			body:        "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n",
			wantDecline: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ExtractUsage uses np.Usage even when Normalize returns ErrUnsupported
			// (a usage-only / no-choices body extracts usage before bailing on the
			// missing structure), so the differential compares against full.Usage
			// regardless of err — mirroring the real caller.
			full, _ := tc.n.Normalize(context.Background(), []byte(tc.body), core.Meta{Direction: core.DirectionResponse})
			slim, ok := tc.n.ExtractUsageOnly([]byte(tc.body))

			if tc.wantDecline {
				if ok {
					t.Fatalf("expected fast path to DECLINE (ok=false) for %s, but it fired with %+v", tc.name, slim)
				}
				return
			}
			if !ok {
				t.Fatalf("expected fast path to fire (ok=true) for %s, but it declined", tc.name)
			}
			if !reflect.DeepEqual(slim, full.Usage) {
				t.Errorf("usage mismatch for %s\n slim=%s\n full=%s", tc.name, fmtUsage(slim), fmtUsage(full.Usage))
			}
		})
	}
}

func fmtUsage(u *core.Usage) string {
	if u == nil {
		return "<nil>"
	}
	d := func(p *int) string {
		if p == nil {
			return "nil"
		}
		return itoa(*p)
	}
	return "{prompt=" + d(u.PromptTokens) + " completion=" + d(u.CompletionTokens) +
		" total=" + d(u.TotalTokens) + " cacheRead=" + d(u.CacheReadTokens) +
		" cacheCreate=" + d(u.CacheCreationTokens) + " reasoning=" + d(u.ReasoningTokens) + "}"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
