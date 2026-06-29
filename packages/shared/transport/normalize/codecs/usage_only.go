package codecs

// usage_only.go — fast-path usage extraction for providers.ExtractUsage.
//
// ExtractUsage previously ran the full Tier-1 Normalize and discarded everything
// but np.Usage, so it parsed and allocated the entire content projection
// (choices[]/content[]/message/tool_calls, including kilobyte-scale visible text)
// on every call — and ExtractUsage runs per response AND per usage-bearing stream
// chunk. The UsageOnlyExtractor fast path parses only the usage block (Anthropic
// additionally measures thinking-block length for its reasoning estimate) using a
// content-less slim struct, reusing each family's existing *UsageToCanonical logic
// so there is no second copy of the alias chains.
//
// Safety contract: ExtractUsageOnly returns (usage, true) ONLY when the slim path
// is certain to equal what full Normalize would produce; it returns (nil, false)
// to decline — for an SSE body (which full Normalize folds via the stream path) or
// any parse failure — so the caller falls back to the proven full Normalize.
// Correctness is therefore preserved by construction: the fast path either matches
// the full path or steps aside for it. The differential equivalence tests
// (usage_only_test.go) assert slim == full for every format across representative
// bodies; the producer-side ai-gateway smoke is the final cost/token gate.

import (
	json "github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// UsageOnlyExtractor is the optional fast path a normalizer implements to pull
// canonical Usage from a response body without the full content projection. A
// normalizer that does not implement it falls back to full Normalize in
// ExtractUsage.
type UsageOnlyExtractor interface {
	// ExtractUsageOnly returns (usage, true) when it confidently extracted Usage
	// via the slim path (usage may be nil when the body genuinely carries no usage
	// block — full Normalize would likewise yield nil). It returns (nil, false)
	// when it declines, so the caller must run full Normalize instead.
	ExtractUsageOnly(raw []byte) (*core.Usage, bool)
}

// ExtractUsageOnly — OpenAI-family (and Responses API). The usage block carries
// the explicit token counts (reasoning_tokens included, via
// completion_tokens_details / output_tokens_details). Additionally, when choices[]
// is present, the non-stream normalizer derives a ReasoningTokens estimate from
// message.reasoning_content length for providers (Moonshot/kimi) that ship
// reasoning text but no explicit reasoning_tokens — and only when the usage block
// did not already report one. This slim path reproduces both, parsing only the
// usage block and each message's reasoning_content/reasoning (the visible content
// and tool_calls are skipped, not allocated). It mirrors normalizeNonStreamResponse
// exactly, including that the estimate is NOT applied when choices[] is absent
// (the full path returns before that step). Declines SSE bodies.
func (n *OpenAIChatNormalizer) ExtractUsageOnly(raw []byte) (*core.Usage, bool) {
	if looksLikeOpenAIEventStream(raw) {
		return nil, false
	}
	var slim struct {
		Usage   *openAIUsage `json:"usage"`
		Choices []struct {
			Message *struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &slim); err != nil {
		return nil, false
	}
	var u *core.Usage
	if slim.Usage != nil {
		u = slim.Usage.extractCanonicalUsage()
	}
	// Reasoning-content estimate: the full path runs this only after confirming
	// choices[] is non-empty (it returns ErrUnsupported before this step when
	// choices are absent), so gate on len(Choices) > 0 to match — a usage-only or
	// Responses-shape body (no choices) gets the usage block verbatim, no estimate.
	if len(slim.Choices) > 0 {
		reasoningChars := 0
		for _, ch := range slim.Choices {
			if ch.Message != nil {
				reasoningChars += len(firstNonEmptyString(ch.Message.ReasoningContent, ch.Message.Reasoning))
			}
		}
		if reasoningChars > 0 {
			if u == nil {
				u = &core.Usage{}
			}
			if u.ReasoningTokens == nil {
				est := reasoningChars * 2 / 7 // chars / 3.5, integer-safe
				if est < 1 {
					est = 1
				}
				u.ReasoningTokens = &est
			}
		}
	}
	return u, true
}

// ExtractUsageOnly — Gemini/Vertex: usageMetadata (incl. thoughtsTokenCount) is
// self-contained. Unlike the other families, the full Gemini path extracts usage
// only AFTER the candidates gate (gemini_generate.go: it returns ErrUnsupported
// with nil Usage when len(candidates)==0), so a body with usageMetadata but no
// candidates — e.g. a safety-blocked / prompt-only generateContent that returns
// promptFeedback + usageMetadata and no candidates — yields zero Usage today.
// Decline that shape so the proven full path keeps producing zero; reporting its
// tokens would be a billing change, not a perf optimization. Also declines SSE.
// Candidates are counted via []struct{} (array length only, content not allocated).
func (n *GeminiGenerateNormalizer) ExtractUsageOnly(raw []byte) (*core.Usage, bool) {
	if looksLikeGeminiEventStream(raw) {
		return nil, false
	}
	var slim struct {
		Candidates    []struct{}   `json:"candidates"`
		UsageMetadata *geminiUsage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &slim); err != nil {
		return nil, false
	}
	if len(slim.Candidates) == 0 {
		return nil, false
	}
	if slim.UsageMetadata == nil {
		return nil, true
	}
	return geminiUsageToCanonical(slim.UsageMetadata), true
}

// ExtractUsageOnly — Cohere v2 chat: usage.tokens block, no cache/reasoning. The
// normalizer has no SSE fold (single JSON shape), so an SSE body fails the slim
// unmarshal and declines to the full path, which behaves the same.
func (n *CohereChatNormalizer) ExtractUsageOnly(raw []byte) (*core.Usage, bool) {
	var slim struct {
		Usage *cohereUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &slim); err != nil {
		return nil, false
	}
	if slim.Usage == nil {
		return nil, true
	}
	return cohereUsageToCanonical(slim.Usage), true
}

// ExtractUsageOnly — Replicate prediction: metrics.{input,output}_token_count.
// Mirrors the full path's guard exactly (Usage only when a count is non-zero).
func (n *ReplicateNormalizer) ExtractUsageOnly(raw []byte) (*core.Usage, bool) {
	var slim struct {
		Metrics *replicateMetrics `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &slim); err != nil {
		return nil, false
	}
	if slim.Metrics == nil || (slim.Metrics.InputTokenCount == 0 && slim.Metrics.OutputTokenCount == 0) {
		return nil, true
	}
	return replicateUsageToCanonical(slim.Metrics), true
}

// ExtractUsageOnly — Anthropic/Bedrock: the usage block carries tokens + cache
// (cost-complete), but ReasoningTokens is estimated from the thinking-block
// character length. The slim struct captures the usage block plus only the
// thinking blocks' text (visible text blocks are skipped, not allocated), then
// calls the same anthropicUsageToCanonical the full path uses with the same
// reasoningChars — so the result is identical, including the reasoning estimate.
// Declines SSE bodies (Normalize folds those via the streaming path).
func (n *AnthropicMessagesNormalizer) ExtractUsageOnly(raw []byte) (*core.Usage, bool) {
	if LooksLikeAnthropicSSE(raw) {
		return nil, false
	}
	var slim struct {
		Usage   *anthropicUsage `json:"usage"`
		Content []struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
			Text     string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &slim); err != nil {
		return nil, false
	}
	if slim.Usage == nil {
		return nil, true
	}
	// Mirror anthropicContentPart: only type=="thinking" blocks become
	// ContentReasoning, with the reasoning text under "thinking" or, when empty,
	// "text". Sum their lengths exactly as the full path sums len(b.Text).
	reasoningChars := 0
	for _, b := range slim.Content {
		if b.Type == "thinking" {
			s := b.Thinking
			if s == "" {
				s = b.Text
			}
			reasoningChars += len(s)
		}
	}
	return anthropicUsageToCanonical(slim.Usage, reasoningChars), true
}
