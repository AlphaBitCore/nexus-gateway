package bedrock

import (
	"fmt"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
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
//   - EndpointChatCompletions: delegates to the Anthropic codec + post-processes.
//   - EndpointEmbeddings: dispatches to the Titan or Cohere embed codec via
//     embeddingEncodeRequest based on CallTarget.ProviderModelID prefix.
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
//   - EndpointEmbeddings: dispatches to Titan or Cohere embed decoder by
//     probing the response body shape. Titan returns `embedding` (singular,
//     a flat float array) while Cohere returns `embeddings` (plural, an array
//     of arrays). It then asserts the decoded vector count matches the
//     request input count using the wire request body in
//     reqCtx.
//   - EndpointChatCompletions: delegates to the Anthropic codec.
func (c codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, contentType string, reqCtx provcore.DecodeContext) (provcore.DecodeResult, error) {
	if endpoint == typology.WireShapeBedrockEmbeddings {
		res, err := decodeBedrockEmbedResponseByShape(nativeBody)
		if err != nil {
			return provcore.DecodeResult{}, err
		}
		// Guard against a provider silently dropping or reordering items:
		// the canonical data[] is re-indexed by upstream position, so a
		// count mismatch means the vectors no longer align with the request
		// inputs. expected = request inputs (Titan single
		// `inputText`→1, Cohere `texts`→len).
		if err := specutil.ValidateEmbeddingRowCount(
			bedrockEmbedInputCount(reqCtx.RequestBody),
			int(gjson.GetBytes(res.CanonicalBody, "data.#").Int()),
		); err != nil {
			return provcore.DecodeResult{}, fmt.Errorf("bedrock embed response: %w", err)
		}
		return res, nil
	}
	// Bedrock Claude response body == Anthropic Messages shape; delegate
	// decode with the Anthropic wire-shape so the codec's shape gate
	// accepts it (mirrors EncodeRequest's same translation upstream).
	return c.anthropic.DecodeResponse(typology.WireShapeAnthropicMessages, nativeBody, contentType, reqCtx)
}

// bedrockEmbedInputCount returns the number of inputs in a Bedrock embedding
// wire request: Titan carries a single `inputText` (count 1); Cohere-on-
// Bedrock carries `texts`[] (count len). Returns 0 when the body is absent
// or neither field is present (disables the count guard rather than
// rejecting).
func bedrockEmbedInputCount(reqBody []byte) int {
	if len(reqBody) == 0 {
		return 0
	}
	if texts := gjson.GetBytes(reqBody, "texts"); texts.IsArray() {
		return len(texts.Array())
	}
	if gjson.GetBytes(reqBody, "inputText").Exists() {
		return 1
	}
	return 0
}
