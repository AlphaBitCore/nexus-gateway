package codecs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// GLMEmbeddingsNormalizer handles GLM's /api/paas/v4/embeddings surface for
// both request and response directions.
//
// GLM (Zhipu AI) exposes an OpenAI-compatible embedding endpoint at
// POST https://open.bigmodel.cn/api/paas/v4/embeddings.
//
// Request shape (identical to OpenAI /v1/embeddings, minus token arrays):
//
//	{"model": "embedding-3", "input": "..." | ["...", "..."]}
//
// The `input` field accepts:
//   - string     → Inputs[0]
//   - []string   → Inputs[...]
//
// Integer token arrays are NOT supported by GLM. The ai-gateway codec
// (spec_glm/codec) rejects token inputs at the gateway layer before they
// reach GLM. This normalizer produces the binary_input_token_array marker
// on the rare case where a body containing token inputs reaches the audit
// pipeline directly (e.g. via compliance proxy raw capture).
//
// Response shape (identical to OpenAI /v1/embeddings):
//
//	{"object": "list", "data": [...], "model": "...", "usage": {"prompt_tokens": N, "total_tokens": N}}
type GLMEmbeddingsNormalizer struct{}

// NewGLMEmbeddingsNormalizer returns a stateless normalizer instance.
func NewGLMEmbeddingsNormalizer() *GLMEmbeddingsNormalizer {
	return &GLMEmbeddingsNormalizer{}
}

// ID is the metric / log label.
func (n *GLMEmbeddingsNormalizer) ID() string { return "glm-embeddings" }

// Normalize routes by Meta.Direction to the request or response path.
func (n *GLMEmbeddingsNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroEmbeddingPayload("glm-embeddings", meta), fmt.Errorf("glm-embeddings: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroEmbeddingPayload("glm-embeddings", meta), fmt.Errorf("glm-embeddings: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, glmEmbeddingsFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "glm-embeddings"
		}
	}
	return p, err
}

// glmEmbeddingsFieldSpec returns the declared top-level wire keys for
// the GLM /api/paas/v4/embeddings surface in direction d.
// GLM's wire shape is identical to OpenAI /v1/embeddings.
func glmEmbeddingsFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "input"},
			Optional: []string{"encoding_format", "user"},
		}
	}
	return core.FieldSpec{
		Required: []string{"object", "data", "model", "usage"},
		Optional: []string{"id"},
	}
}

// glmEmbeddingsRequest mirrors the GLM /api/paas/v4/embeddings request body.
// input is stored as RawMessage because it can be string, []string, []int, or [][]int.
type glmEmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
}

func (n *GLMEmbeddingsNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req glmEmbeddingsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroEmbeddingPayload("glm-embeddings", meta), fmt.Errorf("glm-embeddings: request unmarshal: %w", err)
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return zeroEmbeddingPayload("glm-embeddings", meta), fmt.Errorf("glm-embeddings: missing input: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "glm-embeddings",
		Model:            firstNonEmpty(req.Model, meta.Model),
	}

	// Decode the polymorphic `input` field (same helper as OpenAI embeddings).
	inputs, tokenArray, err := decodeOpenAIEmbeddingInput(req.Input)
	if err != nil {
		return out, fmt.Errorf("glm-embeddings: input decode: %w", err)
	}
	if tokenArray {
		// Token-array inputs are rejected by GLM; mark with the shared rule ID
		// so audit consumers understand the omission. The ai-gateway GLM codec
		// rejects these before they reach the wire, but raw compliance-proxy
		// capture can surface them here.
		out.RuleIDs = []string{"binary_input_token_array"}
	} else {
		out.Inputs = inputs
	}

	return out, nil
}

// glmEmbeddingsResponse mirrors the GLM /api/paas/v4/embeddings response
// body. Embedding vectors are intentionally ignored per SDD §T2.3.
// The shape is identical to OpenAI /v1/embeddings.
type glmEmbeddingsResponse struct {
	Object string         `json:"object"`
	Model  string         `json:"model"`
	Usage  *glmEmbedUsage `json:"usage,omitempty"`
}

type glmEmbedUsage struct {
	PromptTokens int `json:"prompt_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

func (n *GLMEmbeddingsNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp glmEmbeddingsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroEmbeddingPayload("glm-embeddings", meta), fmt.Errorf("glm-embeddings: response unmarshal: %w", err)
	}
	if resp.Object == "" && resp.Model == "" && resp.Usage == nil {
		return zeroEmbeddingPayload("glm-embeddings", meta), fmt.Errorf("glm-embeddings: response missing required fields: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "glm-embeddings",
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
