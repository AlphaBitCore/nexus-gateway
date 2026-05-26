package gemini

import (
	"context"
	"strings"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// embeddingNormalizer is the shared Tier-1 normalizer for Gemini's
// :embedContent and :batchEmbedContents surfaces.
var embeddingNormalizer = codecs.NewGeminiEmbeddingsNormalizer()

// Normalize implements normalize.Normalizer for the Google Gemini
// generateContent / streamGenerateContent surfaces. Dispatches to the
// Gemini Embeddings normalizer when meta.EndpointPath is an embedding path.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if isGeminiEmbeddingPath(meta.EndpointPath) {
		return embeddingNormalizer.Normalize(ctx, raw, meta)
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "gemini",
		ReqSpecIDs:    []string{"gemini-generate"},
		RespSpecIDs:   []string{"gemini-generate-nonstream", "gemini-generate-sse"},
		MinConfidence: 0.5,
	})
}

// isGeminiEmbeddingPath reports whether path targets a Gemini embedding
// endpoint (:embedContent or :batchEmbedContents).
func isGeminiEmbeddingPath(path string) bool {
	return strings.Contains(path, ":embedContent") || strings.Contains(path, ":batchEmbedContents")
}
