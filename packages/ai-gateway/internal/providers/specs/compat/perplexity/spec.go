// Package spec_perplexity wires the Perplexity Sonar provider
// AdapterSpec. Perplexity exposes an OpenAI-compatible chat
// completions API at api.perplexity.ai/chat/completions; this codec
// reuses every spec_openai component and only changes the
// [provcore.Format] tag so vendor audit / metrics / policy can
// target Perplexity specifically.
package perplexity

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Perplexity [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatPerplexity,
		Transport:       openai.NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
