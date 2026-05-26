package bedrock

import (
	"fmt"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/tidwall/sjson"
)

// anthropicVersion is the Bedrock-pinned Anthropic body schema version.
// Documented at https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-anthropic-claude-messages.html
// — Bedrock requires this exact string in the request body.
const anthropicVersion = "bedrock-2023-05-31"

// NewCodec returns the Bedrock SchemaCodec. The Bedrock Claude family on
// InvokeModel accepts the Anthropic Messages body shape verbatim, except:
// (1) `model` is replaced by the URL `modelId`, so the body must omit
// it, and (2) the body must carry `anthropic_version`. We delegate to
// the Anthropic codec so tool-use / multimodal / tool_choice (incl.
// disable_parallel_tool_use) all flow through automatically.
func NewCodec(log *slog.Logger) provcore.SchemaCodec {
	if log == nil {
		log = slog.Default()
	}
	return codec{anthropic: anthropic.NewSpec(log).SchemaCodec}
}

type codec struct {
	anthropic provcore.SchemaCodec
}

// EncodeRequest translates canonical OpenAI → Bedrock wire body.
// - EndpointChatCompletions: delegates to the Anthropic codec + post-processes.
// - EndpointEmbeddings: dispatches to the Titan or Cohere embed codec via
//   embeddingEncodeRequest based on CallTarget.ProviderModelID prefix.
func (c codec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if endpoint == typology.WireShapeBedrockEmbeddings {
		return embeddingEncodeRequest(canonicalBody, target)
	}
	if endpoint != typology.WireShapeBedrockConverse {
		return provcore.EncodeResult{}, fmt.Errorf("bedrock: unsupported endpoint %q", endpoint)
	}
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("bedrock: empty canonical body")
	}
	// Delegate canonical → Anthropic-Messages translation to the Anthropic
	// codec (Bedrock Claude wire body == Anthropic Messages shape modulo
	// the model/anthropic_version edits below). Pass the Anthropic
	// wire-shape so the codec's shape gate accepts it.
	res, err := c.anthropic.EncodeRequest(typology.WireShapeAnthropicMessages, canonicalBody, target)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	body := res.Body
	body, err = sjson.DeleteBytes(body, "model")
	if err != nil {
		return provcore.EncodeResult{}, fmt.Errorf("bedrock: strip model: %w", err)
	}
	body, err = sjson.SetBytes(body, "anthropic_version", anthropicVersion)
	if err != nil {
		return provcore.EncodeResult{}, fmt.Errorf("bedrock: set anthropic_version: %w", err)
	}
	res.Body = body
	return res, nil
}

// DecodeResponse dispatches by endpoint:
// - EndpointEmbeddings: dispatches to Titan or Cohere embed decoder by
//   probing the response body shape. Titan returns `embedding` (singular,
//   a flat float array) while Cohere returns `embeddings` (plural, an array
//   of arrays). This shape-based dispatch avoids the need for a modelID in
//   the codec interface (SchemaCodec.DecodeResponse does not carry CallTarget).
// - EndpointChatCompletions: delegates to the Anthropic codec.
func (c codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, contentType string) (provcore.DecodeResult, error) {
	if endpoint == typology.WireShapeBedrockEmbeddings {
		return decodeBedrockEmbedResponseByShape(nativeBody)
	}
	// Bedrock Claude response body == Anthropic Messages shape; delegate
	// decode with the Anthropic wire-shape so the codec's shape gate
	// accepts it (mirrors EncodeRequest's same translation upstream).
	return c.anthropic.DecodeResponse(typology.WireShapeAnthropicMessages, nativeBody, contentType)
}
