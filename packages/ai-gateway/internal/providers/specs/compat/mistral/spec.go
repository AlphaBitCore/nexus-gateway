// Package spec_mistral wires the Mistral La Plateforme provider
// AdapterSpec. Mistral exposes an OpenAI-compatible chat completions
// API at api.mistral.ai/v1/chat/completions; this codec reuses every
// spec_openai component (transport, codec, stream decoder, error
// normalizer) and only changes the [provcore.Format] tag so vendor
// audit / metrics / policy can target Mistral specifically.
package mistral

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Mistral [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatMistral,
		Transport:       openai.NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
