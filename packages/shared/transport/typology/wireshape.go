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
	// WireShapeOpenAIVideos is the async video-generation submit body
	// (multipart/form-data on POST /v1/videos — prompt/model/seconds/size
	// + optional input_reference file). The poll / content / delete
	// siblings carry no request body and classify as WireShapeNone.
	WireShapeOpenAIVideos  WireShape = "openai-videos"
	WireShapeOpenAIBatches WireShape = "openai-batches"

	// Anthropic.
	WireShapeAnthropicMessages WireShape = "anthropic-messages"

	// Google Gemini (Google AI Studio).
	WireShapeGeminiGenerateContent WireShape = "gemini-generate-content"
	WireShapeGeminiEmbedContent    WireShape = "gemini-embed-content"
	// WireShapeGeminiImagesGenerateContent is the image leg of
	// :generateContent (responseModalities:["IMAGE"], Nano Banana models).
	// Target-side only: it is resolved per call by the cross-shape image
	// bridge (canonical = OpenAI images), never by ingress classification —
	// there is no Gemini-native image ingress, so defaults.go carries no
	// rule for it. The distinct constant is what lets the Gemini codec
	// dispatch image-kind encode/decode on a wire whose URL is shared with
	// chat.
	WireShapeGeminiImagesGenerateContent WireShape = "gemini-images-generate-content"

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
	// WireShapeCohereRerank is BOTH the canonical rerank ingress shape
	// (/v1/rerank carries a Cohere-shaped body — see defaults.go) AND the
	// Cohere /v2/rerank wire shape; the two coincide, like WireShapeOpenAIImages
	// is both canonical and OpenAI-wire. Cross-provider rerank translates this
	// canonical to WireShapeVoyageRerank per target.
	WireShapeCohereRerank WireShape = "cohere-rerank"

	// Voyage AI.
	WireShapeVoyageEmbeddings WireShape = "voyage-embeddings"
	// WireShapeVoyageRerank is target-side only (Voyage /v1/rerank): resolved
	// per call by the rerank bridge when routing a canonical rerank request to
	// a Voyage target. There is no Voyage-native rerank ingress, so defaults.go
	// carries no rule for it.
	WireShapeVoyageRerank WireShape = "voyage-rerank"

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
	WireShapeOpenAIVideos,
	WireShapeOpenAIBatches,
	WireShapeAnthropicMessages,
	WireShapeGeminiGenerateContent,
	WireShapeGeminiEmbedContent,
	WireShapeGeminiImagesGenerateContent,
	WireShapeVertexGenerateContent,
	WireShapeVertexEmbedContent,
	WireShapeBedrockConverse,
	WireShapeBedrockInvoke,
	WireShapeBedrockEmbeddings,
	WireShapeCohereChat,
	WireShapeCohereEmbed,
	WireShapeCohereRerank,
	WireShapeVoyageEmbeddings,
	WireShapeVoyageRerank,
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
	case WireShapeOpenAIImages,
		WireShapeGeminiImagesGenerateContent:
		return EndpointKindImageGeneration
	case WireShapeOpenAIVideos:
		return EndpointKindVideoGeneration
	case WireShapeCohereRerank,
		WireShapeVoyageRerank:
		return EndpointKindRerank
	case WireShapeOpenAIBatches:
		return EndpointKindBatch
	}
	return ""
}
