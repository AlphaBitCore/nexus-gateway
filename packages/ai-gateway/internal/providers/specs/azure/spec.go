// Package spec_azure_openai wires the Azure OpenAI AdapterSpec. Azure
// exposes OpenAI's `/chat/completions` style but behind a deployment
// path: `/openai/deployments/{deployment}/chat/completions?api-version=…`.
// Auth is the `api-key` header rather than `Authorization: Bearer`.
package azure

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Azure OpenAI AdapterSpec.
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:             provcore.FormatAzureOpenAI,
		Transport:          NewTransport(log),
		SchemaCodec:        openai.IdentityCodec(),
		StreamDecoder:      openai.NewStreamDecoder(log),
		ErrorNormalizer:    openai.ErrorNormalizerInstance(),
		PassthroughRewrite: openai.ApplyReasoningRewrites,
		// Azure OpenAI mirrors OpenAI's embeddings endpoint via deployment
		// URL path: /openai/deployments/{deployment}/embeddings?api-version=…
		// The IdentityCodec applies the same per-model rules as OpenAI
		// (ada-002 dimension strip, text-embedding-3-* passthrough).
		RequestShapes: []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIEmbeddings},
	}
}
