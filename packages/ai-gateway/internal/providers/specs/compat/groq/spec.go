// Package spec_groq wires the Groq provider AdapterSpec. Groq exposes
// an OpenAI-compatible chat completions API at
// api.groq.com/openai/v1/chat/completions; this codec reuses every
// spec_openai component and only changes the [provcore.Format] tag so
// vendor audit / metrics / policy can target Groq specifically.
package groq

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Groq [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatGroq,
		Transport:       openai.NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
