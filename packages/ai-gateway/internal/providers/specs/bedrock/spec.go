// Package spec_bedrock wires the AWS Bedrock AdapterSpec. Bedrock
// exposes `Invoke*` endpoints that accept vendor-specific bodies; we
// speak Anthropic-format bodies against
// `bedrock-runtime.<region>.amazonaws.com/model/<modelId>/invoke[-with-response-stream]`
// and rely on the Anthropic SchemaCodec for payload shaping.
//
// Streaming: Bedrock streaming is not supported by this adapter. Bedrock's
// InvokeModelWithResponseStream uses AWS EventStream binary framing rather
// than SSE, which this adapter does not decode; a stream request returns a
// typed `bedrock_stream_unsupported` error from the stream decoder rather
// than incorrect output. Non-streaming invocations work end-to-end.
//
// Authentication is AWS SigV4.
package bedrock

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// NewSpec returns the Bedrock AdapterSpec.
// Bedrock serves both chat-completions (Anthropic Claude family) and
// embeddings (Amazon Titan Embed, Cohere Embed) via InvokeModel.
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatBedrock,
		Transport:       NewTransport(log),
		SchemaCodec:     NewCodec(log),
		StreamDecoder:   newBedrockStreamDecoder(log),
		ErrorNormalizer: errorNormalizer{},
		// Bedrock serves chat-completions (Claude) and embeddings (Titan, Cohere).
		RequestShapes: []typology.WireShape{typology.WireShapeBedrockConverse, typology.WireShapeBedrockEmbeddings},
	}
}
