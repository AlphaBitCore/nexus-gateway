package cohere

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// NewSpec returns the Cohere [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatCohere,
		Transport:       NewTransport(log),
		SchemaCodec:     codec{},
		StreamDecoder:   NewStreamDecoder(log),
		ErrorNormalizer: errorNormalizer{},
		// Cohere natively serves the chat-completions shape (v2/chat), the
		// embeddings shape (v2/embed — canonical → Cohere wire via
		// canonicalToCohereEmbed), and reranking (v2/rerank — the canonical
		// rerank shape IS the Cohere shape; see rerank_codec.go).
		RequestShapes: []typology.WireShape{typology.WireShapeCohereChat, typology.WireShapeCohereEmbed, typology.WireShapeCohereRerank},
	}
}
