package codecs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// VoyageEmbeddingsNormalizer handles Voyage AI's /v1/embeddings surface for
// both request and response directions.
//
// Request shape (Voyage AI):
//
//	{
//	    "model":            "voyage-3",
//	    "input":            "..." | ["...", "..."],
//	    "input_type":       "query" | "document",        // optional
//	    "truncation":       true | false,                // optional
//	    "output_dimension": 1024,                        // optional
//	    "output_dtype":     "float" | "int8" | "uint8" | "binary" | "ubinary" // optional
//	}
//
// Response shape (identical to OpenAI embeddings except usage only has total_tokens):
//
//	{
//	    "object": "list",
//	    "data":   [{"object": "embedding", "embedding": [...], "index": 0}],
//	    "model":  "voyage-3",
//	    "usage":  {"total_tokens": N}
//	}
//
// Embedding vectors are intentionally NOT stored per SDD §T2.3.
type VoyageEmbeddingsNormalizer struct{}

// NewVoyageEmbeddingsNormalizer returns a stateless normalizer instance.
func NewVoyageEmbeddingsNormalizer() *VoyageEmbeddingsNormalizer {
	return &VoyageEmbeddingsNormalizer{}
}

// ID is the metric / log label.
func (n *VoyageEmbeddingsNormalizer) ID() string { return "voyage-embeddings" }

// Normalize routes by Meta.Direction to the request or response path.
func (n *VoyageEmbeddingsNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: empty body: %w", core.ErrUnsupported)
	}
	// Voyage AI /v1/embeddings is JSON-only (no streaming surface).
	// SSE / chunked-text bytes arriving here are guaranteed misrouted —
	// returning ErrUnsupported lets the Registry walk fall through to
	// Tier 2 PatternNormalizer / Tier 3 GenericHTTP instead of dying on
	// a JSON unmarshal hard-error. #98 cross-service consistency:
	// every Tier 1 codec must soft-error on wrong wire shape.
	if meta.Stream {
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: streaming not supported: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, voyageEmbeddingsFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "voyage-embeddings"
		}
	}
	return p, err
}

// voyageEmbeddingsFieldSpec returns the declared top-level wire keys for
// the Voyage AI /v1/embeddings surface in direction d.
func voyageEmbeddingsFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "input"},
			Optional: []string{"input_type", "truncation", "output_dimension", "output_dtype"},
		}
	}
	// Response shape is identical to OpenAI embeddings except usage only
	// has total_tokens (no prompt/completion split).
	return core.FieldSpec{
		Required: []string{"object", "data", "model", "usage"},
		Optional: []string{},
	}
}

// voyageEmbeddingsRequest mirrors the Voyage AI /v1/embeddings request body.
// Input is stored as RawMessage because it can be a string or []string.
type voyageEmbeddingsRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	InputType       string          `json:"input_type,omitempty"`
	Truncation      *bool           `json:"truncation,omitempty"`
	OutputDimension *int            `json:"output_dimension,omitempty"`
	OutputDtype     string          `json:"output_dtype,omitempty"`
}

func (n *VoyageEmbeddingsNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req voyageEmbeddingsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: request unmarshal: %w", err)
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: missing input: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "voyage-embeddings",
		Model:            firstNonEmpty(req.Model, meta.Model),
	}

	// Voyage AI accepts string or []string. Token arrays are rejected upstream
	// by the codec; the normalizer only needs to handle string and []string.
	inputs, isTokenArray, err := decodeOpenAIEmbeddingInput(req.Input)
	if err != nil {
		return out, fmt.Errorf("voyage-embeddings: input decode: %w", err)
	}
	if isTokenArray {
		// Token-array inputs cannot be represented as strings; mark with a
		// rule ID so downstream consumers understand the omission. In practice
		// the ai-gateway codec rejects these before they reach the upstream,
		// but compliance-proxy intercepts may surface raw token inputs.
		out.RuleIDs = []string{"binary_input_token_array"}
	} else {
		out.Inputs = inputs
	}
	return out, nil
}

// voyageEmbeddingsResponse mirrors the Voyage AI /v1/embeddings response.
// Embedding vectors are intentionally ignored per SDD §T2.3.
// Note: Voyage AI only reports total_tokens; there is no prompt/completion split.
type voyageEmbeddingsResponse struct {
	Object string               `json:"object"`
	Model  string               `json:"model"`
	Usage  *voyageEmbedUsage    `json:"usage,omitempty"`
}

type voyageEmbedUsage struct {
	TotalTokens int `json:"total_tokens,omitempty"`
}

func (n *VoyageEmbeddingsNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp voyageEmbeddingsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: response unmarshal: %w", err)
	}
	// The Voyage response shape requires object:"list" and a model; if both
	// are absent this is probably not a Voyage embeddings response.
	if resp.Object == "" && resp.Model == "" && resp.Usage == nil {
		return zeroEmbeddingPayload("voyage-embeddings", meta), fmt.Errorf("voyage-embeddings: response missing required fields: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "voyage-embeddings",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		// Inputs is intentionally nil on the response side.
	}
	if resp.Usage != nil && resp.Usage.TotalTokens != 0 {
		tot := resp.Usage.TotalTokens
		out.Usage = &core.Usage{
			// Voyage AI only reports total_tokens; store in both PromptTokens
			// and TotalTokens to match canonical OpenAI convention (where
			// prompt_tokens == total_tokens for embedding-only calls).
			TotalTokens: &tot,
		}
		setIntPtr(&out.Usage.PromptTokens, resp.Usage.TotalTokens)
	}
	return out, nil
}
