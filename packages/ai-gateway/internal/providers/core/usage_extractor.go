// ExtractUsage is the single Usage-extraction path for ai-gateway codecs.
// Each spec_*/codec.go's DecodeResponse delegates here instead of carrying
// its own alias-chain logic — so the gateway, the compliance proxy, the
// agent, and the Hub audit pipeline all see byte-identical Usage for the
// same upstream response.
//
// See docs/developers/architecture/services/ai-gateway/normalization-architecture.md
// for the full contract.
//
// File lives in the providers/core package (not canonicalbridge) because
// canonicalbridge imports spec_* and putting ExtractUsage there would
// create a circular import (spec_* → canonicalbridge → spec_*). The
// providers/core package is the natural home — the helper returns Usage,
// which is the type alias defined in types.go.

package core

import (
	"context"

	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// ExtractUsage parses raw response bytes via the shared/normalize Tier-1
// normalizer for the given wire format and returns the canonical Usage.
//
// Returns zero-value Usage when:
//   - raw is empty;
//   - the wire format has no Tier-1 normalizer for Usage (caller falls
//     back to its own legacy extraction if needed);
//   - parsing fails with ErrUnsupported (body wasn't recognised as this
//     wire format — likely an error response or partial body).
//
// PromptTokens follows the OpenAI canonical convention (= uncached_input
// + cache_read + cache_creation). The Anthropic Tier-1 normalizer
// normalizes its raw input_tokens (uncached only) to this convention
// before populating Usage.PromptTokens — callers must not subtract
// cache tokens again at the call site.
//
// Concurrency: stateless; safe to call concurrently. Tier-1 normalizer
// instances are constructed per call (cheap; struct{} with no fields).
func ExtractUsage(raw []byte, wireFormat Format) Usage {
	if len(raw) == 0 {
		return Usage{}
	}

	var n normcore.Normalizer
	switch wireFormat {
	case FormatOpenAI,
		FormatDeepSeek,
		FormatGLM,
		FormatAzureOpenAI,
		FormatMoonshot,
		FormatMistral,
		FormatXai,
		FormatGroq,
		FormatPerplexity,
		FormatTogether,
		FormatFireworks,
		FormatMiniMax,
		FormatHuggingFace:
		// All OpenAI-compatible wire formats share the same response
		// parser. The shared normalizer handles the full alias chain
		// (Kimi flat cached_tokens, DeepSeek prompt_cache_hit_tokens,
		// Moonshot prompt_cache_tokens, OpenAI Responses-API top-level
		// input_tokens / output_tokens) automatically.
		n = normcodecs.NewOpenAIChatNormalizer()
	case FormatAnthropic, FormatBedrock:
		// Bedrock wraps Anthropic responses in an AWS envelope; the
		// envelope strip happens upstream of this call in bedrock.
		// By the time ExtractUsage runs, the body is plain Anthropic.
		n = normcodecs.NewAnthropicMessagesNormalizer()
	case FormatGemini, FormatVertex:
		n = normcodecs.NewGeminiGenerateNormalizer()
	case FormatCohere:
		// Cohere v2 chat — usage.tokens.{input,output}_tokens. No cache
		// or reasoning telemetry exposed by the provider as of 2026-05.
		n = normcodecs.NewCohereChatNormalizer()
	case FormatReplicate:
		// Replicate prediction-result — metrics.{input,output}_token_count.
		// No cache or reasoning telemetry.
		n = normcodecs.NewReplicateNormalizer()
	default:
		return Usage{}
	}

	np, err := n.Normalize(context.Background(), raw, normcore.Meta{
		Direction: normcore.DirectionResponse,
	})
	// Whether the parse succeeded or returned ErrUnsupported (body has
	// no choices[] / candidates[] / content[]), the normalizer may have
	// extracted Usage from a top-level `usage` block before bailing on
	// the missing structure. Take whatever Usage was assembled. Only
	// when np.Usage is nil do we return zero.
	_ = err
	if np.Usage == nil {
		return Usage{}
	}
	return *np.Usage
}
