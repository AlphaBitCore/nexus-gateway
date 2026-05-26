// Package spec_fireworks wires the Fireworks AI provider AdapterSpec.
// Fireworks exposes an OpenAI-compatible inference API at
// api.fireworks.ai/inference/v1/chat/completions; this codec reuses
// every spec_openai component and only changes the [provcore.Format]
// tag so vendor audit / metrics / policy can target Fireworks
// specifically.
package fireworks

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Fireworks [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatFireworks,
		Transport:       openai.NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
