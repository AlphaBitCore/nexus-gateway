package typology

// WireShape identifies the request body / response body wire format —
// Axis 2 of the typology. Used by AI Gateway codec selection and by
// Compliance Proxy + Agent body extraction.
//
// One EndpointKind can be served over multiple WireShapes (chat over
// openai-chat, openai-responses, anthropic-messages, gemini-generate-content,
// bedrock-converse, …). One WireShape can ride over multiple ingress
// paths (openai-chat over /v1/chat/completions on api.openai.com OR
// /openai/deployments/.../chat/completions on Azure OR
// /api/paas/v4/chat/completions on GLM — same wire format, different
// upstream URL conventions).
//
// Naming convention: <vendor>-<shape> using kebab-case. The vendor
// prefix is the request body's protocol family ("openai" covers every
// OpenAI-shape-compatible provider; "anthropic" is Anthropic Messages
// only; "gemini" is Google AI Studio; "bedrock" is AWS Bedrock; etc.).
type WireShape string

// WireShape constants. As with EndpointKind values, never rename a
// constant value without coordinating across DB columns, Prometheus
// labels, MQ wire formats, and downstream analytics SQL.
//
// The enumeration covers every provider adapter under
// packages/ai-gateway/internal/providers/specs/, plus the
// CP/Agent classifier rules registered in defaults.go.
const (
	// OpenAI family.
	WireShapeOpenAIChat                WireShape = "openai-chat"
	WireShapeOpenAIResponses           WireShape = "openai-responses"
	WireShapeOpenAICompletionsLegacy   WireShape = "openai-completions-legacy"
	WireShapeOpenAIEmbeddings          WireShape = "openai-embeddings"
	WireShapeOpenAIAudioSpeech         WireShape = "openai-audio-speech"
	WireShapeOpenAIAudioTranscriptions WireShape = "openai-audio-transcriptions"
	WireShapeOpenAIImages              WireShape = "openai-images"
	WireShapeOpenAIBatches             WireShape = "openai-batches"

	// Anthropic.
	WireShapeAnthropicMessages WireShape = "anthropic-messages"

	// Google Gemini (Google AI Studio).
	WireShapeGeminiGenerateContent WireShape = "gemini-generate-content"
	WireShapeGeminiEmbedContent    WireShape = "gemini-embed-content"

	// Google Vertex AI.
	WireShapeVertexGenerateContent WireShape = "vertex-generate-content"
	WireShapeVertexEmbedContent    WireShape = "vertex-embed-content"

	// AWS Bedrock.
	WireShapeBedrockConverse   WireShape = "bedrock-converse"
	WireShapeBedrockInvoke     WireShape = "bedrock-invoke"
	WireShapeBedrockEmbeddings WireShape = "bedrock-embeddings"

	// Cohere.
	WireShapeCohereChat  WireShape = "cohere-chat"
	WireShapeCohereEmbed WireShape = "cohere-embed"

	// Voyage AI.
	WireShapeVoyageEmbeddings WireShape = "voyage-embeddings"

	// WireShapeNone is the sentinel for endpoints that carry no
	// request body (e.g. EndpointKindModels: GET /v1/models). Callers
	// that need to test "is there a body to parse?" check against this
	// sentinel.
	WireShapeNone WireShape = ""
)

// AllWireShapes is the closed enumeration of every defined WireShape
// constant excluding the sentinel WireShapeNone. Tests assert
// exhaustiveness against this slice.
var AllWireShapes = []WireShape{
	WireShapeOpenAIChat,
	WireShapeOpenAIResponses,
	WireShapeOpenAICompletionsLegacy,
	WireShapeOpenAIEmbeddings,
	WireShapeOpenAIAudioSpeech,
	WireShapeOpenAIAudioTranscriptions,
	WireShapeOpenAIImages,
	WireShapeOpenAIBatches,
	WireShapeAnthropicMessages,
	WireShapeGeminiGenerateContent,
	WireShapeGeminiEmbedContent,
	WireShapeVertexGenerateContent,
	WireShapeVertexEmbedContent,
	WireShapeBedrockConverse,
	WireShapeBedrockInvoke,
	WireShapeBedrockEmbeddings,
	WireShapeCohereChat,
	WireShapeCohereEmbed,
	WireShapeVoyageEmbeddings,
}

// IsValid reports whether w is one of the defined WireShape constants
// (excluding the WireShapeNone sentinel — callers that want to accept
// "no body" check for the sentinel separately).
func (w WireShape) IsValid() bool {
	for _, valid := range AllWireShapes {
		if w == valid {
			return true
		}
	}
	return false
}

// String makes WireShape satisfy fmt.Stringer trivially.
func (w WireShape) String() string { return string(w) }

// KindFromWireShape returns the EndpointKind that owns this WireShape.
// Inverse direction of the WireShape constants — the canonical mapping
// from "body wire-shape" back to "semantic endpoint kind".
//
// Used by callers that hold a WireShape (e.g. the resolved ingress) but
// need the canonical kind string for audit / Prometheus / persistence.
// Convert with string(KindFromWireShape(shape)).
//
// Returns the empty EndpointKind ("") for WireShapeNone — the sentinel
// for body-less endpoints (e.g. /v1/models). Callers needing a non-empty
// kind for the body-less case should check for WireShapeNone first.
func KindFromWireShape(w WireShape) EndpointKind {
	switch w {
	case WireShapeOpenAIChat,
		WireShapeOpenAIResponses,
		WireShapeOpenAICompletionsLegacy,
		WireShapeAnthropicMessages,
		WireShapeGeminiGenerateContent,
		WireShapeVertexGenerateContent,
		WireShapeBedrockConverse,
		WireShapeBedrockInvoke,
		WireShapeCohereChat:
		return EndpointKindChat
	case WireShapeOpenAIEmbeddings,
		WireShapeGeminiEmbedContent,
		WireShapeVertexEmbedContent,
		WireShapeBedrockEmbeddings,
		WireShapeCohereEmbed,
		WireShapeVoyageEmbeddings:
		return EndpointKindEmbeddings
	case WireShapeOpenAIAudioSpeech:
		return EndpointKindTTS
	case WireShapeOpenAIAudioTranscriptions:
		return EndpointKindSTT
	case WireShapeOpenAIImages:
		return EndpointKindImageGeneration
	case WireShapeOpenAIBatches:
		return EndpointKindBatch
	}
	return ""
}
