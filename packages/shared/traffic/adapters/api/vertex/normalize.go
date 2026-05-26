package vertex

import (
	"context"
	"strings"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// embeddingNormalizer is the shared Tier-1 normalizer for Vertex AI's
// :embedContent and :batchEmbedContents surfaces (same wire shape as Gemini).
var embeddingNormalizer = codecs.NewGeminiEmbeddingsNormalizer()

// Normalize implements normalize.Normalizer for the Vertex AI
// generateContent surface. Dispatches to the Gemini Embeddings normalizer
// when meta.EndpointPath is an embedding endpoint.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if isVertexEmbeddingPath(meta.EndpointPath) {
		return embeddingNormalizer.Normalize(ctx, raw, meta)
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "vertex",
		ReqSpecIDs:    []string{"gemini-generate"},
		RespSpecIDs:   []string{"gemini-generate-nonstream", "gemini-generate-sse"},
		MinConfidence: 0.5,
	})
}

// isVertexEmbeddingPath reports whether path targets a Vertex AI embedding
// endpoint (:embedContent or :batchEmbedContents).
func isVertexEmbeddingPath(path string) bool {
	return strings.Contains(path, ":embedContent") || strings.Contains(path, ":batchEmbedContents")
}
