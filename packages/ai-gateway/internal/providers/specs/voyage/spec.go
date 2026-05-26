// Package voyage wires the Voyage AI AdapterSpec.
//
// Voyage AI is an embedding-only provider — no chat-completions surface.
// The gateway serves it at POST /v1/embeddings and forwards the canonical
// OpenAI-shape embedding request to https://api.voyageai.com/v1/embeddings
// using a Bearer token for auth.
//
// Architecture references:
//   - docs/dev/architecture/provider-adapter-architecture.md §3a Rules 1-7
//
// Wire format (Voyage AI API):
//
//	POST https://api.voyageai.com/v1/embeddings
//	Authorization: Bearer <api_key>
//	Request:  {model, input: string|[]string, input_type?, truncation?,
//	           output_dimension?, output_dtype?}
//	Response: {object:"list", data:[{object:"embedding",embedding:[...],index}],
//	           model, usage:{total_tokens:N}}
package voyage

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// NewSpec returns the Voyage AI [provcore.AdapterSpec].
// Voyage only serves embeddings — no chat-completions, no streaming.
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatVoyage,
		Transport:       NewTransport(log),
		SchemaCodec:     newCodec(),
		StreamDecoder:   newStreamDecoder(log),
		ErrorNormalizer: errorNormalizer{},
		// Voyage AI serves only the embeddings endpoint; chat-completions
		// is not available (https://docs.voyageai.com).
		RequestShapes: []typology.WireShape{typology.WireShapeVoyageEmbeddings},
	}
}
