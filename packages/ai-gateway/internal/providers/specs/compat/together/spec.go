// Package spec_together wires the Together AI provider AdapterSpec.
// Together exposes an OpenAI-compatible chat completions API at
// api.together.xyz/v1/chat/completions and api.together.ai/v1/chat/completions
// (both hostnames resolve to the same backend); this codec reuses every
// spec_openai component and only changes the [provcore.Format] tag so
// vendor audit / metrics / policy can target Together specifically.
package together

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Together [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatTogether,
		Transport:       openai.NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
