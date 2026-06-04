package azure

import (
	"context"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// embeddingNormalizer is the shared Tier-1 normalizer for the Azure OpenAI
// /openai/deployments/*/embeddings surface (same wire shape as OpenAI).
var embeddingNormalizer = codecs.NewOpenAIEmbeddingsNormalizer()

// Normalize implements normalize.Normalizer. Dispatches to the OpenAI
// Embeddings normalizer when meta.EndpointPath is an embeddings endpoint.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if isAzureEmbeddingPath(meta.EndpointPath) {
		return embeddingNormalizer.Normalize(ctx, raw, meta)
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "azure-openai",
		ReqSpecIDs:    []string{"openai-chat"},
		RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
		MinConfidence: 0.5,
	})
}

// isAzureEmbeddingPath reports whether path targets an Azure OpenAI
// embeddings endpoint. Azure path shape:
//
//	/openai/deployments/<name>/embeddings
func isAzureEmbeddingPath(path string) bool {
	return strings.HasSuffix(path, "/embeddings")
}
