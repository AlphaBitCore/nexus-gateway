package openai

import (
	"context"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// embeddingNormalizer is the shared Tier-1 normalizer for the OpenAI
// /v1/embeddings surface. Initialized once at package load.
var embeddingNormalizer = codecs.NewOpenAIEmbeddingsNormalizer()

// Normalize implements normalize.Normalizer for the OpenAI Chat Completions
// wire format. Dispatches to the OpenAI Embeddings normalizer when
// meta.EndpointPath is an embeddings endpoint.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if isOpenAIEmbeddingPath(meta.EndpointPath) {
		return embeddingNormalizer.Normalize(ctx, raw, meta)
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "openai-compat",
		ReqSpecIDs:    []string{"openai-chat", "openai-completions-legacy"},
		RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
		MinConfidence: 0.5,
	})
}

// isOpenAIEmbeddingPath reports whether path targets an embeddings endpoint.
// Matches /v1/embeddings and sub-paths (Azure adds API version params).
func isOpenAIEmbeddingPath(path string) bool {
	return strings.HasSuffix(path, "/embeddings")
}
