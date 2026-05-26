// Package spec_vertex wires the Google Vertex AI AdapterSpec. Vertex
// shares the Gemini wire schema (generateContent / streamGenerateContent)
// but requires GCP OAuth2 authentication using a service-account JSON
// or an already-minted bearer token exchanged from it.
package vertex

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
)

// NewSpec returns the Vertex AdapterSpec.
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatVertex,
		Transport:       NewTransport(log),
		SchemaCodec:     gemini.NewCodec(),
		StreamDecoder:   gemini.NewStreamDecoder(log),
		ErrorNormalizer: gemini.NewErrorNormalizer(),
	}
}
