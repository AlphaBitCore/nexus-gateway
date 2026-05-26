package codecs

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// RegisterDefaultAIBuiltins registers the AI normalizers that ship with
// Nexus today. Routing keys are wire-format adapter types
// (`providers.Format` in ai-gateway), NOT user-named provider strings —
// so any provider an operator named "groq-east" or "gemini-prod" still
// resolves correctly as long as its `adapter_type` row maps to one of
// the canonical formats below.
//
// Coverage:
//   - openai-style adapters (openai, deepseek, groq, perplexity, mistral,
//     glm, xai, huggingface, replicate, together, fireworks, moonshot,
//     minimax, azure-openai, cohere, bedrock): all use the OpenAI Chat
//     normalizer because their /v1/chat/completions surface is wire-compatible.
//   - anthropic adapter: AnthropicMessagesNormalizer.
//   - gemini / vertex adapters: GeminiGenerateNormalizer (native
//     generateContent surface). When an operator routes Gemini through
//     an OpenAI-compatible facade (`/v1/chat/completions` over a Gemini
//     credential), the model row's adapter_type is "openai" and the
//     OpenAI normalizer handles it — no special case needed.
//   - embedding surfaces: OpenAIEmbeddingsNormalizer (/v1/embeddings for
//     openai-compatible family + Azure), CohereEmbeddingsNormalizer
//     (/v1/embed, /v2/embed), GeminiEmbeddingsNormalizer
//     (:embedContent, :batchEmbedContents).
//
// The registry is left unfrozen on return so callers can extend it with
// service-specific normalizers before Freezing.
func RegisterDefaultAIBuiltins(reg *core.Registry) {
	oai := NewOpenAIChatNormalizer()
	// Wire-compatible OpenAI Chat adapters. Each routes to the same
	// normalizer because their wire format on /v1/chat/completions is
	// the OpenAI Chat schema — only credentialing and host differ.
	openAICompatible := []string{
		"openai",
		// #72 — the traffic.Adapter for api.openai.com (and OpenAI-wire
		// siblings: Mistral, DeepSeek, etc.) is registered in builtins.go
		// under id "openai-compat" and openai.Adapter.ID() returns the
		// same value. Without this alias, agent + compliance-proxy
		// runtimeNormalize calls reg.Normalize(adapterType="openai-compat",
		// ...) miss every Tier-1 entry → ErrUnsupported → Tier 2/3 fall-
		// through emits http-text/generic-http instead of ai-chat, so
		// agent SQLite stamps normalizedResponse=NULL on every openai-
		// shape SSE the agent intercepts. The Hub-side normalize
		// runs OK because it reads the same map and could pick either
		// key, but the agent-local Tier-1 path must match the live
		// traffic.Adapter ID byte-for-byte.
		"openai-compat",
		"azure-openai",
		"deepseek",
		"glm",
		"groq",
		"perplexity",
		"mistral",
		"xai",
		"huggingface",
		"replicate",
		"together",
		"fireworks",
		"moonshot",
		"minimax",
		"cohere",
	}
	for _, key := range openAICompatible {
		reg.Register(key, oai)
		reg.Register(key+"::/v1/chat/completions", oai)
	}

	// OpenAI Responses API (/v1/responses) is a DIFFERENT wire schema
	// from /v1/chat/completions — same vendor, different body shapes
	// (input[] with content[].type=input_text/image, output[] with
	// type=reasoning/message). Path-keyed dispatch picks the right
	// normalizer; without a dedicated entry the audit pipeline falls
	// through to Tier-3 generic-http for every Responses-API row.
	resp := NewOpenAIResponsesNormalizer()
	for _, key := range openAICompatible {
		// Same adapter family — provider that wins routing might be any
		// of the OpenAI-compat siblings (an admin could route /v1/responses
		// to a non-OpenAI vendor whose adapter speaks the same schema).
		reg.Register(key+"::/v1/responses", resp)
	}

	// Embedding normalizers — per-path registrations are MORE SPECIFIC
	// than the chat normalizers registered above (the registry picks the
	// most specific key first: adapterType+path beats adapterType-only),
	// so an embedding URL reaching the registry resolves to the embedding
	// normalizer even though the adapter-only key points to the chat
	// normalizer.
	//
	// OpenAI Embeddings — openai-compatible family share the same
	// /v1/embeddings shape. Path-keyed entries override the adapter-only
	// chat entry so the correct normalizer is selected.
	oaiEmb := NewOpenAIEmbeddingsNormalizer()
	for _, key := range openAICompatible {
		reg.Register(key+"::/v1/embeddings", oaiEmb)
	}
	// Path-only fallback for /v1/embeddings — compliance-proxy + agent
	// intercept embedding traffic without an adapter_type hint.
	reg.Register("::/v1/embeddings", oaiEmb)

	// GLM Embeddings — /api/paas/v4/embeddings (Zhipu AI native path).
	// The glm adapter-only entry above maps to the OpenAI chat normalizer
	// (GLM chat is OpenAI-compatible). The path-specific entry below
	// overrides it for GLM's embedding endpoint. The GLMEmbeddingsNormalizer
	// is structurally identical to OpenAIEmbeddingsNormalizer but carries
	// the "glm-embeddings" protocol label for audit differentiation and
	// records the binary_input_token_array marker when token inputs reach
	// the compliance-proxy pipeline (the ai-gateway GLM codec rejects them
	// upstream, but raw capture may surface them).
	glmEmb := NewGLMEmbeddingsNormalizer()
	reg.Register("glm::/api/paas/v4/embeddings", glmEmb)
	// Path-only fallback for compliance-proxy + agent traffic intercepting
	// GLM embedding calls without a resolved adapter_type hint.
	reg.Register("::/api/paas/v4/embeddings", glmEmb)

	// Cohere Embeddings — v1 and v2 embed paths.
	cohEmb := NewCohereEmbeddingsNormalizer()
	// The cohere adapter-only entry above maps to the chat normalizer;
	// path-specific entries below override it for embedding paths.
	reg.Register("cohere::/v1/embed", cohEmb)
	reg.Register("cohere::/v2/embed", cohEmb)
	// Path-only fallbacks for compliance-proxy + agent.
	reg.Register("::/v1/embed", cohEmb)
	reg.Register("::/v2/embed", cohEmb)

	// Gemini Embeddings — :embedContent and :batchEmbedContents.
	// The gemini adapter-only entry below maps to the generate normalizer;
	// path-specific entries override it for embedding paths.
	// The GeminiEmbeddingsNormalizer internally discriminates single vs
	// batch by body shape (presence of "requests" key), so a single
	// normalizer instance covers both endpoint variants. Per-path
	// registration ensures the more-specific lookup wins over the
	// adapter-only entry that resolves to GeminiGenerateNormalizer.
	gemEmb := NewGeminiEmbeddingsNormalizer()
	// Google AI Studio model-path pattern: /v1beta/models/{model}:embedContent
	// Register the two most common text-embedding model prefixes explicitly.
	// Compliance-proxy and agent traffic uses the path-only fallback below.
	reg.Register("gemini::/v1beta/models/text-embedding-004:embedContent", gemEmb)
	reg.Register("gemini::/v1beta/models/text-embedding-005:embedContent", gemEmb)
	reg.Register("gemini::/v1beta/models/text-multilingual-embedding-002:embedContent", gemEmb)
	reg.Register("gemini::/v1beta/models/text-embedding-004:batchEmbedContents", gemEmb)
	reg.Register("gemini::/v1beta/models/text-embedding-005:batchEmbedContents", gemEmb)
	reg.Register("gemini::/v1beta/models/text-multilingual-embedding-002:batchEmbedContents", gemEmb)
	reg.Register("vertex::/v1beta/models/text-embedding-004:embedContent", gemEmb)
	reg.Register("vertex::/v1beta/models/text-embedding-005:embedContent", gemEmb)
	reg.Register("vertex::/v1beta/models/text-multilingual-embedding-002:embedContent", gemEmb)
	reg.Register("vertex::/v1beta/models/text-embedding-004:batchEmbedContents", gemEmb)
	reg.Register("vertex::/v1beta/models/text-embedding-005:batchEmbedContents", gemEmb)
	reg.Register("vertex::/v1beta/models/text-multilingual-embedding-002:batchEmbedContents", gemEmb)
	// Path-only fallbacks for compliance-proxy + agent traffic. The normalizer
	// itself dispatches on body shape (single vs batch), so each path points
	// to the same instance.
	reg.Register("::/v1beta/models/text-embedding-004:embedContent", gemEmb)
	reg.Register("::/v1beta/models/text-embedding-005:embedContent", gemEmb)
	reg.Register("::/v1beta/models/text-multilingual-embedding-002:embedContent", gemEmb)
	reg.Register("::/v1beta/models/text-embedding-004:batchEmbedContents", gemEmb)
	reg.Register("::/v1beta/models/text-embedding-005:batchEmbedContents", gemEmb)
	reg.Register("::/v1beta/models/text-multilingual-embedding-002:batchEmbedContents", gemEmb)

	anth := NewAnthropicMessagesNormalizer()
	reg.Register("anthropic", anth)
	reg.Register("anthropic::/v1/messages", anth)
	// Bedrock currently fronts Anthropic Messages — bytes flowing
	// through the audit pipeline are Anthropic-shaped invokeModel
	// payloads. Route to the Anthropic normalizer until a dedicated
	// Bedrock normalizer is needed (Titan / Cohere on Bedrock would
	// require their own).
	reg.Register("bedrock", anth)

	gem := NewGeminiGenerateNormalizer()
	reg.Register("gemini", gem)
	reg.Register("vertex", gem)

	// Path-only fallbacks. Critical for compliance-proxy + agent
	// traffic where the adapter `Provider` field carries a host name
	// ("api.anthropic.com", "api.openai.com") or a tool identifier
	// ("cursor", "claude-web") rather than a wire-format key.
	// Resolution falls through to these when no adapter-keyed entry
	// matched the body shape (or one rejected with ErrUnsupported);
	// see core.Registry.Normalize.
	reg.Register("::/v1/messages", anth)
	reg.Register("::/v1/chat/completions", oai)
	// Path-only fallback for /v1/responses — compliance-proxy + agent
	// path through here when intercepting client traffic to an OpenAI
	// Responses-API endpoint without an attached adapter_type hint.
	reg.Register("::/v1/responses", resp)

	// Voyage AI Embeddings — Voyage serves only /v1/embeddings. The
	// adapter key "voyage" is used both for the adapter-specific entry
	// and a path-only fallback ("/v1/embeddings" is shared with the
	// openai-compatible family, but "voyage::/v1/embeddings" is more
	// specific and wins for any request routed through the voyage adapter).
	voyEmb := NewVoyageEmbeddingsNormalizer()
	reg.Register("voyage", voyEmb)
	reg.Register("voyage::/v1/embeddings", voyEmb)

	// Catch-all for traffic that didn't match any AI adapter (cp/agent
	// intercepting plain HTTP, ai-gateway audit rows without a routed
	// adapter type). Registered under the "*:*:*" generic key so the
	// resolver's final lookup step lands here. Without this, generic
	// HTTP traffic falls through with ErrUnsupported and the sidecar
	// row records status="failed".
	reg.Register("*:*:*", NewGenericHTTPNormalizer())

	// Tier 2 (pattern-based extraction) is wired by the binaries
	// (nexus-hub, ai-gateway, compliance-proxy) AFTER this function
	// returns, by calling extract.WireTier2(reg). The wiring happens at
	// the binary level to avoid a normalize → extract import cycle
	// (extract imports normalize.NormalizedPayload). See each
	// cmd/<service>/main.go.
}
