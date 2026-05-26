package typology

// defaultRules is the built-in (method, path) → (EndpointKind, WireShape)
// rule table iterated by ClassifyPath. Rules are listed in priority order;
// the first match wins, so place more-specific patterns before less-specific
// ones. The table covers:
//
//   - Every path registered by AI Gateway's HTTP mux (see
//     packages/ai-gateway/cmd/ai-gateway/wiring/routes.go) — chat completions,
//     responses, messages, embeddings, models, Gemini generateContent /
//     embedContent / batchEmbedContents, Azure deployment-suffixed paths,
//     GLM PAAS-v4 paths.
//   - Every upstream-provider path the Compliance Proxy + Agent
//     classifier intercepts (currently OpenAI, Azure OpenAI, Cohere,
//     Gemini AI Studio, Vertex AI for embeddings; expandable to chat
//     when a provider's chat path is observed in production).
//   - Every additional canonical OpenAI endpoint that may be intercepted
//     transparently (audio, images, batches, legacy completions).
//
// To add a new endpoint kind or wire shape, prepend the rule here and
// add the corresponding constant to endpointkind.go / wireshape.go.
// See the contributor guide in docs/developers/specs/e87-endpoint-typology-unification.md.
var defaultRules = []Rule{
	// ── Chat completions ────────────────────────────────────────────
	// OpenAI canonical chat (also covers every OpenAI-compatible
	// provider that mounts on this path: vLLM, Together, Anyscale,
	// OpenRouter, …).
	{Method: "POST", PathPattern: "/v1/chat/completions", Kind: EndpointKindChat, Shape: WireShapeOpenAIChat},
	// Azure OpenAI deployment-suffixed chat.
	{Method: "POST", PathPattern: "/openai/deployments/*/chat/completions", Kind: EndpointKindChat, Shape: WireShapeOpenAIChat},
	// Zhipu GLM PAAS v4 chat.
	{Method: "POST", PathPattern: "/api/paas/v4/chat/completions", Kind: EndpointKindChat, Shape: WireShapeOpenAIChat},

	// OpenAI Responses API (newer event-typed shape).
	{Method: "POST", PathPattern: "/v1/responses", Kind: EndpointKindChat, Shape: WireShapeOpenAIResponses},

	// Anthropic Messages.
	{Method: "POST", PathPattern: "/v1/messages", Kind: EndpointKindChat, Shape: WireShapeAnthropicMessages},

	// OpenAI legacy text-completions.
	{Method: "POST", PathPattern: "/v1/completions", Kind: EndpointKindChat, Shape: WireShapeOpenAICompletionsLegacy},

	// Vertex AI generateContent (project-scoped path) — listed BEFORE
	// Gemini AI Studio because the Gemini pattern "/v1*/models/*:..."
	// can otherwise match Vertex paths via star-cross-slash. First match
	// wins; the more specific Vertex pattern must come first.
	{Method: "POST", PathPattern: "/v1*/projects/*/locations/*/publishers/*/models/*:generateContent", Kind: EndpointKindChat, Shape: WireShapeVertexGenerateContent},
	{Method: "POST", PathPattern: "/v1*/projects/*/locations/*/publishers/*/models/*:streamGenerateContent", Kind: EndpointKindChat, Shape: WireShapeVertexGenerateContent},

	// Gemini (Google AI Studio) generateContent — covers both
	// :generateContent and :streamGenerateContent on /v1beta/models/*.
	{Method: "POST", PathPattern: "/v1*/models/*:generateContent", Kind: EndpointKindChat, Shape: WireShapeGeminiGenerateContent},
	{Method: "POST", PathPattern: "/v1*/models/*:streamGenerateContent", Kind: EndpointKindChat, Shape: WireShapeGeminiGenerateContent},

	// ── Embeddings ──────────────────────────────────────────────────
	// OpenAI canonical embeddings (also covers OpenAI-shape-compatible providers).
	{Method: "POST", PathPattern: "/v1/embeddings", Kind: EndpointKindEmbeddings, Shape: WireShapeOpenAIEmbeddings},
	// Azure OpenAI deployment-suffixed embeddings.
	{Method: "POST", PathPattern: "/openai/deployments/*/embeddings", Kind: EndpointKindEmbeddings, Shape: WireShapeOpenAIEmbeddings},
	// Zhipu GLM PAAS v4 embeddings.
	{Method: "POST", PathPattern: "/api/paas/v4/embeddings", Kind: EndpointKindEmbeddings, Shape: WireShapeOpenAIEmbeddings},

	// Cohere embed (v1 and v2 share the same wire format).
	{Method: "POST", PathPattern: "/v1/embed", Kind: EndpointKindEmbeddings, Shape: WireShapeCohereEmbed},
	{Method: "POST", PathPattern: "/v2/embed", Kind: EndpointKindEmbeddings, Shape: WireShapeCohereEmbed},

	// Vertex AI embedContent — listed BEFORE Gemini AI Studio for the
	// same star-cross-slash specificity reason as the chat rules above.
	{Method: "POST", PathPattern: "/v1*/projects/*/locations/*/publishers/*/models/*:embedContent", Kind: EndpointKindEmbeddings, Shape: WireShapeVertexEmbedContent},
	{Method: "POST", PathPattern: "/v1*/projects/*/locations/*/publishers/*/models/*:batchEmbedContents", Kind: EndpointKindEmbeddings, Shape: WireShapeVertexEmbedContent},

	// Gemini (AI Studio) embedContent — single and batch.
	{Method: "POST", PathPattern: "/v1*/models/*:embedContent", Kind: EndpointKindEmbeddings, Shape: WireShapeGeminiEmbedContent},
	{Method: "POST", PathPattern: "/v1*/models/*:batchEmbedContents", Kind: EndpointKindEmbeddings, Shape: WireShapeGeminiEmbedContent},

	// ── Audio (STT) ─────────────────────────────────────────────────
	{Method: "POST", PathPattern: "/v1/audio/transcriptions", Kind: EndpointKindSTT, Shape: WireShapeOpenAIAudioTranscriptions},
	{Method: "POST", PathPattern: "/v1/audio/translations", Kind: EndpointKindSTT, Shape: WireShapeOpenAIAudioTranscriptions},

	// ── Audio (TTS) ─────────────────────────────────────────────────
	{Method: "POST", PathPattern: "/v1/audio/speech", Kind: EndpointKindTTS, Shape: WireShapeOpenAIAudioSpeech},

	// ── Image generation ────────────────────────────────────────────
	{Method: "POST", PathPattern: "/v1/images/generations", Kind: EndpointKindImageGeneration, Shape: WireShapeOpenAIImages},
	{Method: "POST", PathPattern: "/v1/images/edits", Kind: EndpointKindImageGeneration, Shape: WireShapeOpenAIImages},
	{Method: "POST", PathPattern: "/v1/images/variations", Kind: EndpointKindImageGeneration, Shape: WireShapeOpenAIImages},

	// ── Batch ───────────────────────────────────────────────────────
	{Method: "POST", PathPattern: "/v1/batches", Kind: EndpointKindBatch, Shape: WireShapeOpenAIBatches},

	// ── Catalog / models ────────────────────────────────────────────
	// /v1/models is GET-only and carries no body, so WireShape is the
	// WireShapeNone sentinel. Two patterns: list + single-model detail.
	{Method: "GET", PathPattern: "/v1/models", Kind: EndpointKindModels, Shape: WireShapeNone},
	{Method: "GET", PathPattern: "/v1/models/*", Kind: EndpointKindModels, Shape: WireShapeNone},
}
