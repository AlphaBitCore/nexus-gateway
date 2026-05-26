package embeddings

// Request is the minimal embedding call input used by the cache/semantic L2
// layer's read+write paths. It is intentionally narrower than the canonical
// provcore.EmbeddingsRequest (packages/ai-gateway/internal/providers/core/types.go)
// because the L2 layer embeds exactly one text per request — the canonical
// type's discriminator (String / Strings / Tokens) collapses to the String arm
// here. Cross-format batch embedding goes via the canonical bridge.
type Request struct {
	// Model is the model code sent to the provider on the wire.
	Model string
	// Input is the text to embed.  Single-string by design (see type
	// docstring above): the cache layer fingerprints exactly one
	// (model, text) pair per singleflight key.
	Input string
	// Dimensions, if > 0, is forwarded to the provider (OpenAI
	// text-embedding-3-* supports this to truncate output vectors).
	// Zero means "use the model default".
	Dimensions int
	// EncodingFormat is "float" (default) or "base64".  Empty string
	// is treated as "float" by both EncodeOpenAIRequest and
	// DecodeOpenAIResponse.
	EncodingFormat string
}

// Response is the minimal embedding result for the L2 cache layer.  Matches
// the single-input arm of the canonical [provcore.EmbeddingsResponse]: one
// vector + prompt-token count.  Callers that need the full batch envelope go
// through the canonical bridge instead.
type Response struct {
	// Embedding is the float32 vector returned by the provider.
	Embedding []float32
	// Model is the model string echoed back by the provider in the
	// response body (may differ from the requested model code when
	// the provider aliases or snaps to a specific version).
	Model string
	// PromptTokens is the number of input tokens consumed.
	PromptTokens int
}
