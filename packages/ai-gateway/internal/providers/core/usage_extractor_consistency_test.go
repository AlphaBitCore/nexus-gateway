package core_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestExtractUsage_CrossComponentConsistency asserts that the canonical
// Usage extracted via the ai-gateway path (core.ExtractUsage) is
// byte-identical to the Usage extracted via the Hub audit / compliance
// proxy path (shared/normalize Tier-1 normalizer directly) for every
// fixture below.
//
// This is the structural invariant: one parser, one source of
// Usage truth. If a future contributor inlines extraction logic in one
// path and forgets the other, this test catches the drift before it
// reaches prod and produces inconsistent traffic_event rows across
// consumer paths.
func TestExtractUsage_CrossComponentConsistency(t *testing.T) {
	cases := []struct {
		name       string
		wireFormat core.Format
		normalizer normalize.Normalizer
		body       string
	}{
		// OpenAI canonical (post-2024-09 prompt_tokens_details).
		{
			name:       "openai_canonical_chat",
			wireFormat: core.FormatOpenAI,
			normalizer: normcodecs.NewOpenAIChatNormalizer(),
			body: `{
				"model":"gpt-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
				"usage":{
					"prompt_tokens":1234,
					"completion_tokens":567,
					"total_tokens":1801,
					"prompt_tokens_details":{"cached_tokens":800},
					"completion_tokens_details":{"reasoning_tokens":300}
				}
			}`,
		},

		// Kimi K2 flat cached_tokens alias.
		{
			name:       "openai_compat_kimi_flat_cache",
			wireFormat: core.FormatMoonshot,
			normalizer: normcodecs.NewOpenAIChatNormalizer(),
			body: `{
				"model":"kimi-k2",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"cached_tokens":600}
			}`,
		},

		// DeepSeek prompt_cache_hit_tokens alias.
		{
			name:       "openai_compat_deepseek_cache_hit",
			wireFormat: core.FormatDeepSeek,
			normalizer: normcodecs.NewOpenAIChatNormalizer(),
			body: `{
				"model":"deepseek-chat",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_cache_hit_tokens":700,"prompt_cache_miss_tokens":300}
			}`,
		},

		// Anthropic — cache_read_input_tokens + cache_creation_input_tokens.
		// PromptTokens normalized to uncached + cache_read + cache_creation.
		{
			name:       "anthropic_with_cache",
			wireFormat: core.FormatAnthropic,
			normalizer: normcodecs.NewAnthropicMessagesNormalizer(),
			body: `{
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"Sure."}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":4000,"cache_creation_input_tokens":1000}
			}`,
		},

		// Gemini — cachedContentTokenCount + thoughtsTokenCount.
		// CompletionTokens normalized to candidates + thoughts.
		{
			name:       "gemini_with_thinking",
			wireFormat: core.FormatGemini,
			normalizer: normcodecs.NewGeminiGenerateNormalizer(),
			body: `{
				"modelVersion":"gemini-2.5-pro",
				"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],
				"usageMetadata":{
					"promptTokenCount":100,
					"candidatesTokenCount":50,
					"totalTokenCount":250,
					"cachedContentTokenCount":80,
					"thoughtsTokenCount":100
				}
			}`,
		},

		// Edge case: usage-only body (no choices). Each path must still
		// surface the Usage instead of returning zero.
		{
			name:       "openai_usage_only_no_choices",
			wireFormat: core.FormatOpenAI,
			normalizer: normcodecs.NewOpenAIChatNormalizer(),
			body:       `{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
		},

		// Cohere v2 chat — usage.tokens.{input,output}_tokens.
		{
			name:       "cohere_v2_chat",
			wireFormat: core.FormatCohere,
			normalizer: normcodecs.NewCohereChatNormalizer(),
			body: `{
				"id":"cmd-123",
				"model":"command-r-plus",
				"finish_reason":"COMPLETE",
				"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},
				"usage":{"tokens":{"input_tokens":42,"output_tokens":17}}
			}`,
		},

		// Replicate prediction-result — metrics.{input,output}_token_count.
		{
			name:       "replicate_prediction",
			wireFormat: core.FormatReplicate,
			normalizer: normcodecs.NewReplicateNormalizer(),
			body: `{
				"id":"pred-abc",
				"version":"meta/llama-3-70b",
				"status":"succeeded",
				"output":"hello there",
				"metrics":{"input_token_count":120,"output_token_count":40}
			}`,
		},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			// Path A: ai-gateway codec layer (core.ExtractUsage).
			gotA := core.ExtractUsage([]byte(tc.body), tc.wireFormat)

			// Path B: Hub audit / compliance-proxy path (shared/normalize Tier-1).
			np, err := tc.normalizer.Normalize(context.Background(), []byte(tc.body), normalize.Meta{
				Direction: normalize.DirectionResponse,
			})
			// ErrUnsupported is tolerated — usage-only bodies trip the
			// "choices missing" guard but Usage is still extracted.
			_ = err
			var gotB core.Usage
			if np.Usage != nil {
				gotB = *np.Usage
			}

			if diff := cmp.Diff(gotA, gotB); diff != "" {
				t.Errorf("ai-gateway path vs shared/normalize path Usage drift (-A +B):\n%s", diff)
			}

			// Also sanity-check: at least one field must be populated. A
			// totally-empty Usage means BOTH paths missed extraction —
			// the test fixture is bad, not the consistency.
			if gotA.PromptTokens == nil && gotA.CompletionTokens == nil &&
				gotA.TotalTokens == nil && gotA.CacheReadTokens == nil &&
				gotA.CacheCreationTokens == nil && gotA.ReasoningTokens == nil {
				t.Errorf("both paths returned empty Usage — fixture %q may be malformed", tc.name)
			}
		})
	}
}

// TestExtractUsage_DispatchBranches covers the ExtractUsage dispatch arms that the
// usage-only fast path otherwise hides: the empty-input guard, the SSE body that
// makes the fast path decline and fall back to full Normalize (which folds the
// stream and extracts usage from the terminal chunk), and a non-stream body with
// no usage block (fast path returns (nil, true) → zero Usage).
func TestExtractUsage_DispatchBranches(t *testing.T) {
	isZero := func(u core.Usage) bool {
		return u.PromptTokens == nil && u.CompletionTokens == nil && u.TotalTokens == nil &&
			u.CacheReadTokens == nil && u.CacheCreationTokens == nil && u.ReasoningTokens == nil
	}

	// 1. Empty input → zero Usage (early guard, no normalizer built).
	if got := core.ExtractUsage(nil, core.FormatOpenAI); !isZero(got) {
		t.Errorf("empty raw: got %+v, want zero Usage", got)
	}

	// 2. SSE body → fast path declines (looksLikeOpenAIEventStream) → full Normalize
	// stream fold extracts usage from the terminal usage-bearing chunk.
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n")
	u := core.ExtractUsage(sse, core.FormatOpenAI)
	if u.PromptTokens == nil || *u.PromptTokens != 10 || u.CompletionTokens == nil || *u.CompletionTokens != 5 {
		t.Errorf("SSE fallback: got %+v, want prompt=10 completion=5 via full Normalize stream path", u)
	}

	// 3. Non-stream body with no usage block → fast path returns (nil, true) → zero.
	noUsage := []byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	if got := core.ExtractUsage(noUsage, core.FormatOpenAI); !isZero(got) {
		t.Errorf("no-usage body: got %+v, want zero Usage", got)
	}
}

// TestExtractUsage_AnthropicPromptTokensNormalization explicitly verifies
// GAP-C1 fix: Anthropic's raw input_tokens (uncached only) is
// normalized to OpenAI canonical PromptTokens (= uncached + cache_read +
// cache_creation) so downstream cost math is symmetric across core.
func TestExtractUsage_AnthropicPromptTokensNormalization(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"x"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":4000,"cache_creation_input_tokens":1000}
	}`
	got := core.ExtractUsage([]byte(body), core.FormatAnthropic)
	// PromptTokens = 50 + 4000 + 1000 = 5050.
	if got.PromptTokens == nil || *got.PromptTokens != 5050 {
		t.Errorf("PromptTokens not normalized to OpenAI convention: got %v, want 5050", got.PromptTokens)
	}
	if got.CacheReadTokens == nil || *got.CacheReadTokens != 4000 {
		t.Errorf("CacheReadTokens not surfaced: got %v", got.CacheReadTokens)
	}
	if got.CacheCreationTokens == nil || *got.CacheCreationTokens != 1000 {
		t.Errorf("CacheCreationTokens not surfaced: got %v", got.CacheCreationTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != 20 {
		t.Errorf("CompletionTokens: got %v, want 20", got.CompletionTokens)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 5070 {
		t.Errorf("TotalTokens: got %v, want 5070 (PromptTokens + CompletionTokens)", got.TotalTokens)
	}
}

// TestExtractUsage_GeminiCompletionIncludesThinking verifies the
// GAP-D fix: CompletionTokens follows OpenAI convention of including
// reasoning tokens (Gemini's candidatesTokenCount is disjoint from
// thoughtsTokenCount; canonical CompletionTokens sums them).
func TestExtractUsage_GeminiCompletionIncludesThinking(t *testing.T) {
	body := `{
		"modelVersion":"gemini-2.5-pro",
		"candidates":[{"content":{"parts":[{"text":"hi"}]}}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"totalTokenCount":250,"thoughtsTokenCount":100}
	}`
	got := core.ExtractUsage([]byte(body), core.FormatGemini)
	if got.CompletionTokens == nil || *got.CompletionTokens != 150 {
		t.Errorf("CompletionTokens should be candidates + thoughts = 150: got %v", got.CompletionTokens)
	}
	if got.ReasoningTokens == nil || *got.ReasoningTokens != 100 {
		t.Errorf("ReasoningTokens should be thoughts = 100: got %v", got.ReasoningTokens)
	}
}
