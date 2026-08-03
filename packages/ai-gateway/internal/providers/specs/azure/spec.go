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
		Format:    provcore.FormatAzureOpenAI,
		Transport: NewTransport(log),
		// Azure serves the same model families as OpenAI behind deployment
		// URLs and returns the same wire 400s, so its identity codec
		// carries the OpenAI wire-rule contract verbatim (reasoning
		// max_tokens rename + sampling strips, ada-002 embedding strips).
		// Deployments named after something other than their model simply
		// match no rule gate, exactly as before.
		SchemaCodec:     openai.NewIdentityCodec(openai.OpenAIContract()),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
		// Azure OpenAI mirrors OpenAI's embeddings endpoint via deployment
		// URL path: /openai/deployments/{deployment}/embeddings?api-version=…
		RequestShapes: []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIEmbeddings},
	}
}
