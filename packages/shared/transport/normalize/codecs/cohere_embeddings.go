package codecs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// CohereEmbeddingsNormalizer handles Cohere's /v1/embed and /v2/embed
// surfaces for both request and response directions.
//
// Request shape (v1 and v2 share the same top-level structure):
//
//	{"texts": ["..."], "model": "embed-v4.0", "input_type": "search_document", "embedding_types": ["float"]}
//
// Response shape (vectors intentionally NOT stored per SDD §T2.3):
//
//	{
//	  "embeddings": {...},
//	  "meta": {"billed_units": {"input_tokens": N}},
//	  "model": "embed-v4.0",
//	  "id": "...",
//	  "texts": [...],
//	  "response_type": "embeddings_floats"
//	}
type CohereEmbeddingsNormalizer struct{}

// NewCohereEmbeddingsNormalizer returns a stateless normalizer instance.
func NewCohereEmbeddingsNormalizer() *CohereEmbeddingsNormalizer {
	return &CohereEmbeddingsNormalizer{}
}

// ID is the metric / log label.
func (n *CohereEmbeddingsNormalizer) ID() string { return "cohere-embeddings" }

// Normalize routes by Meta.Direction to the request or response path.
func (n *CohereEmbeddingsNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroEmbeddingPayload("cohere-embeddings", meta), fmt.Errorf("cohere-embeddings: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroEmbeddingPayload("cohere-embeddings", meta), fmt.Errorf("cohere-embeddings: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, cohereEmbeddingsFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "cohere-embeddings"
		}
	}
	return p, err
}

// cohereEmbeddingsFieldSpec returns the declared top-level wire keys for
// the Cohere /v1/embed and /v2/embed surfaces in direction d.
func cohereEmbeddingsFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"texts", "model"},
			Optional: []string{"input_type", "embedding_types", "truncate"},
		}
	}
	return core.FieldSpec{
		Required: []string{"embeddings", "meta", "model"},
		Optional: []string{"id", "texts", "response_type"},
	}
}

type cohereEmbeddingsRequest struct {
	Texts          []string        `json:"texts"`
	Model          string          `json:"model"`
	InputType      string          `json:"input_type,omitempty"`
	EmbeddingTypes []string        `json:"embedding_types,omitempty"`
	Truncate       json.RawMessage `json:"truncate,omitempty"`
}

func (n *CohereEmbeddingsNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req cohereEmbeddingsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroEmbeddingPayload("cohere-embeddings", meta), fmt.Errorf("cohere-embeddings: request unmarshal: %w", err)
	}
	if len(req.Texts) == 0 {
		return zeroEmbeddingPayload("cohere-embeddings", meta), fmt.Errorf("cohere-embeddings: missing texts[]: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "cohere-embeddings",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Inputs:           req.Texts,
	}
	return out, nil
}

// cohereEmbeddingsResponse mirrors the Cohere /v1/embed and /v2/embed
// response body. Embedding vectors are intentionally ignored per SDD §T2.3.
type cohereEmbeddingsResponse struct {
	ID           string              `json:"id,omitempty"`
	Model        string              `json:"model,omitempty"`
	ResponseType string              `json:"response_type,omitempty"`
	Meta         *cohereEmbedMeta    `json:"meta,omitempty"`
}

type cohereEmbedMeta struct {
	BilledUnits *struct {
		InputTokens int `json:"input_tokens,omitempty"`
	} `json:"billed_units,omitempty"`
}

func (n *CohereEmbeddingsNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp cohereEmbeddingsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroEmbeddingPayload("cohere-embeddings", meta), fmt.Errorf("cohere-embeddings: response unmarshal: %w", err)
	}
	// Cohere embed response must have either embeddings field or meta — check
	// that the body is actually a Cohere embed response (not a chat response).
	// We do this by checking for the absence of all recognised response fields.
	if resp.ID == "" && resp.Model == "" && resp.Meta == nil {
		return zeroEmbeddingPayload("cohere-embeddings", meta), fmt.Errorf("cohere-embeddings: response missing required fields: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "cohere-embeddings",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		// Inputs intentionally nil on response side.
	}
	if resp.Meta != nil && resp.Meta.BilledUnits != nil && resp.Meta.BilledUnits.InputTokens != 0 {
		out.Usage = &core.Usage{}
		setIntPtr(&out.Usage.PromptTokens, resp.Meta.BilledUnits.InputTokens)
		tot := resp.Meta.BilledUnits.InputTokens
		out.Usage.TotalTokens = &tot
	}
	return out, nil
}
