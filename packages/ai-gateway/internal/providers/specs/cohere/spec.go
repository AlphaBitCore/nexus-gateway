package cohere

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// NewSpec returns the Cohere [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:    provcore.FormatCohere,
		Transport: NewTransport(log),
		// Gated for the two limits the shared policy owns — the image formats
		// Cohere's own refusal enumerates, and an attachment whose type the
		// caller never declared. The document lift stays in codec_content.go.
		SchemaCodec:     specutil.GateContent(codec{}, contentPolicyFor),
		StreamDecoder:   NewStreamDecoder(log),
		ErrorNormalizer: errorNormalizer{},
		// Cohere natively serves the chat-completions shape (v2/chat), the
		// embeddings shape (v2/embed — canonical → Cohere wire via
		// canonicalToCohereEmbed), and reranking (v2/rerank — the canonical
		// rerank shape IS the Cohere shape; see rerank_codec.go).
		RequestShapes: []typology.WireShape{typology.WireShapeCohereChat, typology.WireShapeCohereEmbed, typology.WireShapeCohereRerank},
	}
}
