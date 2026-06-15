// Package spec_deepseek wires the DeepSeek AdapterSpec. DeepSeek
// exposes an OpenAI-compatible `/v1/*` surface, so the Transport and
// StreamDecoder are functionally identical to [spec_openai]; the
// SchemaCodec is identity because the on-the-wire shape already
// matches OpenAI.
package deepseek

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the DeepSeek [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:             provcore.FormatDeepSeek,
		Transport:          openai.NewTransport(log),
		SchemaCodec:        openai.IdentityCodec(),
		StreamDecoder:      openai.NewStreamDecoder(log),
		ErrorNormalizer:    openai.ErrorNormalizerInstance(),
		PassthroughRewrite: ApplyRewrites,
	}
}
