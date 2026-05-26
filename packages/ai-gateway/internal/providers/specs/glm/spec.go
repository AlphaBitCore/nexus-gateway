// Package spec_glm wires the GLM (Zhipu AI) AdapterSpec. GLM exposes an
// OpenAI-compatible surface:
//   - Chat completions: /api/paas/v4/chat/completions — wire body identical
//     to OpenAI; the GLM SchemaCodec passes it through unchanged.
//   - Embeddings:       /api/paas/v4/embeddings — OpenAI-compatible shape
//     ({model, input} → {object, data, usage}) BUT GLM does NOT accept
//     integer token arrays; the GLM codec rejects them with a 400.
//
// Authentication is a JWT signed from a `<api_id>.<api_secret>` secret pair
// (per Zhipu docs) transported as a standard Bearer token; the Resolver
// hands us the already-minted JWT via CallTarget.APIKey.
package glm

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	glmcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/glm/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the GLM AdapterSpec.
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:        provcore.FormatGLM,
		Transport:     NewTransport(log),
		SchemaCodec:   glmcodec.New(),
		StreamDecoder: openai.NewStreamDecoder(log),
		// GLM emits OpenAI-style error envelopes — reuse the shared normalizer.
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
		// GLM natively serves chat-completions and embeddings shapes.
		RequestShapes: []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIEmbeddings},
	}
}
