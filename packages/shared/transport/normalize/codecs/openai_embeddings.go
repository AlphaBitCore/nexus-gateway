package codecs

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// OpenAIEmbeddingsNormalizer handles OpenAI's /v1/embeddings surface for
// both request and response directions. It is also used for Azure OpenAI
// embeddings, which share the same wire shape.
//
// Request shape:
//
//	{"model": "text-embedding-3-small", "input": "...", "dimensions": 1536, "encoding_format": "float"}
//
// The `input` field is polymorphic:
//   - string     → Inputs[0]
//   - []string   → Inputs[...]
//   - []int      → token array (binary_input_token_array marker, Inputs=nil)
//   - [][]int    → batch token arrays (binary_input_token_array marker, Inputs=nil)
//
// Response shape (vectors intentionally NOT stored per SDD §T2.3):
//
//	{"object": "list", "data": [...], "model": "...", "usage": {"prompt_tokens": N, "total_tokens": N}}
type OpenAIEmbeddingsNormalizer struct{}

// NewOpenAIEmbeddingsNormalizer returns a stateless normalizer instance.
func NewOpenAIEmbeddingsNormalizer() *OpenAIEmbeddingsNormalizer {
	return &OpenAIEmbeddingsNormalizer{}
}

// ID is the metric / log label.
func (n *OpenAIEmbeddingsNormalizer) ID() string { return "openai-embeddings" }

// Normalize routes by Meta.Direction to the request or response path.
func (n *OpenAIEmbeddingsNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroEmbeddingPayload("openai-embeddings", meta), fmt.Errorf("openai-embeddings: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroEmbeddingPayload("openai-embeddings", meta), fmt.Errorf("openai-embeddings: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, openAIEmbeddingsFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "openai-embeddings"
		}
	}
	return p, err
}

// openAIEmbeddingsFieldSpec returns the declared top-level wire keys for
// the OpenAI Embeddings surface in direction d.
func openAIEmbeddingsFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "input"},
			Optional: []string{"dimensions", "encoding_format", "user"},
		}
	}
	return core.FieldSpec{
		Required: []string{"object", "data", "model", "usage"},
		Optional: []string{"id"},
	}
}

// openAIEmbeddingsRequest mirrors the OpenAI /v1/embeddings request body.
// input is stored as RawMessage because it can be string, []string, []int,
// or [][]int.
type openAIEmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
}

func (n *OpenAIEmbeddingsNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req openAIEmbeddingsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroEmbeddingPayload("openai-embeddings", meta), fmt.Errorf("openai-embeddings: request unmarshal: %w", err)
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return zeroEmbeddingPayload("openai-embeddings", meta), fmt.Errorf("openai-embeddings: missing input: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-embeddings",
		Model:            firstNonEmpty(req.Model, meta.Model),
	}

	// Decode the polymorphic `input` field.
	inputs, tokenArray, err := decodeOpenAIEmbeddingInput(req.Input)
	if err != nil {
		return out, fmt.Errorf("openai-embeddings: input decode: %w", err)
	}
	if tokenArray {
		// Token-array inputs cannot be represented as strings; mark with
		// a rule ID so downstream consumers understand the omission.
		out.RuleIDs = []string{"binary_input_token_array"}
	} else {
		out.Inputs = inputs
	}

	return out, nil
}

// decodeOpenAIEmbeddingInput handles the four valid shapes of OpenAI's
// `input` field:
//
//   - string    → (["<s>"], false, nil)
//   - []string  → (["<s1>","<s2>",...], false, nil)
//   - []int     → (nil, true, nil)  — token array, not representable
//   - [][]int   → (nil, true, nil)  — batch token arrays, not representable
//
// Returns (inputs, isTokenArray, err).
func decodeOpenAIEmbeddingInput(raw json.RawMessage) ([]string, bool, error) {
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, false, nil
	}

	// Try []string.
	var ss []string
	if err := json.Unmarshal(raw, &ss); err == nil {
		return ss, false, nil
	}

	// Try []int (single token array).
	var ints []int
	if err := json.Unmarshal(raw, &ints); err == nil {
		return nil, true, nil
	}

	// Try [][]int (batch token arrays).
	var intss [][]int
	if err := json.Unmarshal(raw, &intss); err == nil {
		return nil, true, nil
	}

	return nil, false, fmt.Errorf("unrecognised input shape: %w", core.ErrUnsupported)
}

// openAIEmbeddingsResponse mirrors the OpenAI /v1/embeddings response body.
// We only parse metadata; the data[].embedding float vectors are intentionally
// ignored per SDD §T2.3.
type openAIEmbeddingsResponse struct {
	Object string            `json:"object"`
	Model  string            `json:"model"`
	Usage  *openAIEmbedUsage `json:"usage,omitempty"`
}

type openAIEmbedUsage struct {
	PromptTokens int `json:"prompt_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

func (n *OpenAIEmbeddingsNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp openAIEmbeddingsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroEmbeddingPayload("openai-embeddings", meta), fmt.Errorf("openai-embeddings: response unmarshal: %w", err)
	}
	if resp.Object == "" && resp.Model == "" && resp.Usage == nil {
		return zeroEmbeddingPayload("openai-embeddings", meta), fmt.Errorf("openai-embeddings: response missing required fields: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-embeddings",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		// Inputs is intentionally nil on the response side.
	}
	if resp.Usage != nil && (resp.Usage.PromptTokens != 0 || resp.Usage.TotalTokens != 0) {
		out.Usage = &core.Usage{}
		setIntPtr(&out.Usage.PromptTokens, resp.Usage.PromptTokens)
		setIntPtr(&out.Usage.TotalTokens, resp.Usage.TotalTokens)
	}
	return out, nil
}

// zeroEmbeddingPayload returns a minimal zero-value embedding payload for
// error return paths. The protocol label helps identify the source in logs.
func zeroEmbeddingPayload(protocol string, meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         protocol,
		Model:            meta.Model,
	}
}
