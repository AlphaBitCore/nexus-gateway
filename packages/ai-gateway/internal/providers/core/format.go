// Format is the provider wire-format enum: which upstream dialect an
// adapter speaks. Split out of types.go so the request/response and
// call-target types there stay readable on their own.
package core

// Format is the provider wire format. The set is one-to-one with the
// non-fallback IDs in shared/traffic/adapters. The traffic adapter
// named "generic-jsonpath" is a traffic-side fallback only; it has no
// provider-side counterpart and is intentionally absent here.
type Format string

const (
	FormatOpenAI      Format = "openai"
	FormatDeepSeek    Format = "deepseek"
	FormatGLM         Format = "glm"
	FormatAzureOpenAI Format = "azure-openai"
	FormatAnthropic   Format = "anthropic"
	FormatGemini      Format = "gemini"
	FormatMiniMax     Format = "minimax"
	FormatBedrock     Format = "bedrock"
	FormatVertex      Format = "vertex"
	FormatCohere      Format = "cohere"
	FormatHuggingFace Format = "huggingface"
	FormatReplicate   Format = "replicate"
	// OpenAI-compat re-users — distinct Format constants so vendor-scoped
	// audit, metrics, and rate-limit policies can target them without
	// pattern-matching on Provider name. Each ships as a thin spec_X
	// package that delegates wire encoding/decoding to openai.
	FormatMistral    Format = "mistral"
	FormatXai        Format = "xai"
	FormatGroq       Format = "groq"
	FormatPerplexity Format = "perplexity"
	FormatTogether   Format = "together"
	FormatFireworks  Format = "fireworks"
	FormatMoonshot   Format = "moonshot"
	FormatVoyage     Format = "voyage"
	// FormatOpenAIResponses is OpenAI's /v1/responses wire format, the
	// distinct request/response shape for reasoning models + built-in tools
	// + server-side conversation state. Treated as a sibling ingress format,
	// NOT a new canonical: the canonical bus remains OpenAI chat-completions
	// shape per provider-adapter-architecture.md §3a Rule 1. The /v1/responses
	// codec under spec_openai translates in both directions; same-shape
	// passthrough is gated by the target adapter's RequestShapes containing
	// "responses-api".
	FormatOpenAIResponses Format = "openai-responses"
)

// AllFormats returns every provider wire [Format] backed by its own
// builtin spec package, in stable order. This is the registry / codec /
// normalizer coverage set: registry seeding, builtin SchemaCodec and
// normalizer coverage checks, the canonical-bridge self-check, and the
// cross-pair matrix tests all iterate it.
//
// Membership is "has a standalone spec package", NOT "is a chat-completions
// format". Embeddings-only providers belong here — FormatVoyage ships
// spec_voyage and needs codec + normalizer coverage like any other format.
// Chat-routability is a separate predicate decided by the canonical bridge
// (Bridge.ChatRoutable, via its formatSupportsChat helper), which excludes
// Voyage because it serves only embeddings. FormatOpenAIResponses is the
// one declared format intentionally NOT returned here: it has no standalone
// spec, being folded into spec_openai as a sibling ingress format; it is
// still .Valid() and still detected at the route layer.
func AllFormats() []Format {
	return []Format{
		FormatOpenAI,
		FormatDeepSeek,
		FormatGLM,
		FormatAzureOpenAI,
		FormatAnthropic,
		FormatGemini,
		FormatMiniMax,
		FormatBedrock,
		FormatVertex,
		FormatCohere,
		FormatHuggingFace,
		FormatReplicate,
		FormatMistral,
		FormatXai,
		FormatGroq,
		FormatPerplexity,
		FormatTogether,
		FormatFireworks,
		FormatMoonshot,
		FormatVoyage,
	}
}

// Valid reports whether f is a known format.
func (f Format) Valid() bool {
	switch f {
	case FormatOpenAI, FormatDeepSeek, FormatGLM, FormatAzureOpenAI,
		FormatAnthropic, FormatGemini, FormatMiniMax, FormatBedrock, FormatVertex,
		FormatCohere, FormatHuggingFace, FormatReplicate,
		FormatMistral, FormatXai, FormatGroq, FormatPerplexity,
		FormatTogether, FormatFireworks, FormatMoonshot, FormatVoyage,
		FormatOpenAIResponses:
		return true
	}
	return false
}

// IsOpenAIFamily reports whether bodies in this format share the
// canonical OpenAI chat completions JSON schema — model at the JSON
// root, messages array, etc. — so that simple `payload["model"] = X`
// substitution works as a passthrough rewrite.
//
// Must stay in sync with the set of formats that wire spec_openai's
// IdentityCodec as their SchemaCodec; the dispatch native-leg triage uses
// this method as its OpenAI-family key, which routes the body to the
// codec's RewriteNative differential (the model stamp among it). Keeping
// the list here (rather than open-coded per call site) is what makes
// routing to a Moonshot/Mistral/Groq/... target carry the target's
// ProviderModelID instead of the originator's model code.
func (f Format) IsOpenAIFamily() bool {
	switch f {
	case FormatOpenAI, FormatDeepSeek, FormatGLM, FormatAzureOpenAI,
		FormatMoonshot, FormatMiniMax, FormatHuggingFace,
		FormatMistral, FormatXai, FormatGroq, FormatPerplexity,
		FormatTogether, FormatFireworks:
		return true
	}
	return false
}
