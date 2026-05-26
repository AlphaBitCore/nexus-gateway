// Package spec_gemini wires the Google Gemini AdapterSpec. The wire
// protocol is REST+SSE on `https://generativelanguage.googleapis.com/v1beta`
// with the model embedded in the URL path and the API key passed as a
// query parameter or `x-goog-api-key` header.
package gemini

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	gcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/ingress"
	gstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/stream"
)

// NewSpec returns the Gemini [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatGemini,
		Transport:       NewTransport(log),
		SchemaCodec:     gcodec.NewCodec(),
		StreamDecoder:   gstream.NewStreamDecoder(log),
		ErrorNormalizer: specerrors.ErrorNormalizer{},
		// Gemini natively serves both the chat-completions shape
		// (generateContent) and the embeddings shape (embedContent /
		// batchEmbedContents). The codec selects single vs batch via
		// EncodeResult.URLOverride based on canonical input shape.
		RequestShapes: []typology.WireShape{typology.WireShapeGeminiGenerateContent, typology.WireShapeGeminiEmbedContent, typology.WireShapeVertexGenerateContent, typology.WireShapeVertexEmbedContent},
	}
}

// NewCodec returns the Gemini SchemaCodec so siblings (Vertex) can
// reuse the exact encode/decode logic without duplicating it.
func NewCodec() provcore.SchemaCodec { return gcodec.NewCodec() }

// NewErrorNormalizer returns the shared Google API error normaliser
// used by both Gemini and Vertex.
func NewErrorNormalizer() provcore.ErrorNormalizer { return specerrors.ErrorNormalizer{} }

// NewStreamDecoder returns a StreamDecoder for Gemini SSE streams.
func NewStreamDecoder(log *slog.Logger) *gstream.StreamDecoder {
	return gstream.NewStreamDecoder(log)
}

// GenerateContentRequestToOpenAIChatCompletion converts a Gemini
// generateContent body into canonical OpenAI chat.completions JSON.
// Used by canonicalbridge (hub-ingress path).
func GenerateContentRequestToOpenAIChatCompletion(native []byte, model string) ([]byte, error) {
	return ingress.GenerateContentRequestToOpenAIChatCompletion(native, model)
}

// OpenAIChatCompletionToGenerateContentResponse converts a canonical OpenAI
// chat.completion JSON body into a Gemini generateContent response envelope.
// Used by canonicalbridge (hub-egress path).
func OpenAIChatCompletionToGenerateContentResponse(openaiBody []byte) ([]byte, error) {
	return ingress.OpenAIChatCompletionToGenerateContentResponse(openaiBody)
}
