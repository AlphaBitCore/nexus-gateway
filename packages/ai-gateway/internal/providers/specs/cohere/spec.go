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
		// Cohere natively serves both the chat-completions shape (v2/chat)
		// and the embeddings shape (v2/embed — requires canonical → Cohere
		// wire translation via canonicalToCohereEmbed in embed_codec.go).
		RequestShapes: []typology.WireShape{typology.WireShapeCohereChat, typology.WireShapeCohereEmbed},
	}
}
