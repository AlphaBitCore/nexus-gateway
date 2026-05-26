package cohere

import (
	"context"
	"strings"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// embeddingNormalizer is the shared Tier-1 normalizer for Cohere's
// /v1/embed and /v2/embed surfaces.
var embeddingNormalizer = codecs.NewCohereEmbeddingsNormalizer()

// Normalize implements normalize.Normalizer. Dispatches to the Cohere
// Embeddings normalizer when meta.EndpointPath is an embed endpoint.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if isCohereEmbeddingPath(meta.EndpointPath) {
		return embeddingNormalizer.Normalize(ctx, raw, meta)
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "cohere",
		ReqSpecIDs:    []string{"openai-chat"},
		RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
		MinConfidence: 0.5,
	})
}

// isCohereEmbeddingPath reports whether path targets a Cohere embed endpoint.
// Handles both /v1/embed and /v2/embed.
func isCohereEmbeddingPath(path string) bool {
	return strings.HasSuffix(path, "/embed")
}
