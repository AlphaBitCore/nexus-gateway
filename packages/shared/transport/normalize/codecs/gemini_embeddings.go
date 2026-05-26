package codecs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// GeminiEmbeddingsNormalizer handles Google's embedding surfaces:
//
//   - :embedContent     — single embedding request/response
//   - :batchEmbedContents — batch embedding request/response
//
// Both surfaces are used by Google AI Studio (Gemini API) and Vertex AI.
//
// Single request shape:
//
//	{"content": {"parts": [{"text": "..."}]}, "model": "models/text-embedding-004"}
//
// Batch request shape:
//
//	{"requests": [{"content": {"parts": [{"text": "..."}]}, "model": "..."}]}
//
// Response shapes (vectors intentionally NOT stored per SDD §T2.3):
//
//	Single:  {"embedding": {"values": [...]}}
//	Batch:   {"embeddings": [{"values": [...]}, ...]}
//
// Discriminating single vs batch is done by EndpointPath or body shape:
// the presence of "requests" key indicates batch; "content" key indicates single.
type GeminiEmbeddingsNormalizer struct{}

// NewGeminiEmbeddingsNormalizer returns a stateless normalizer instance.
func NewGeminiEmbeddingsNormalizer() *GeminiEmbeddingsNormalizer {
	return &GeminiEmbeddingsNormalizer{}
}

// ID is the metric / log label.
func (n *GeminiEmbeddingsNormalizer) ID() string { return "gemini-embeddings" }

// Normalize routes by Meta.Direction and detects single vs batch by body shape.
func (n *GeminiEmbeddingsNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: empty body: %w", core.ErrUnsupported)
	}

	isBatch := isGeminiBatchEmbedding(raw, meta.EndpointPath)

	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		if isBatch {
			p, err = n.normalizeBatchRequest(raw, meta)
		} else {
			p, err = n.normalizeSingleRequest(raw, meta)
		}
	case core.DirectionResponse:
		if isBatch {
			p, err = n.normalizeBatchResponse(raw, meta)
		} else {
			p, err = n.normalizeSingleResponse(raw, meta)
		}
	default:
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		var spec core.FieldSpec
		if isBatch {
			spec = geminiEmbeddingsFieldSpec(meta.Direction, true)
		} else {
			spec = geminiEmbeddingsFieldSpec(meta.Direction, false)
		}
		p.Confidence = core.ScoreTier1Confidence(raw, spec)
		if p.DetectedSpec == "" {
			p.DetectedSpec = "gemini-embeddings"
		}
	}
	return p, err
}

// isGeminiBatchEmbedding returns true when the wire body or endpoint path
// indicates a :batchEmbedContents call. Body-shape detection (looking for
// "requests" key) is the primary signal; endpoint path is a secondary hint.
func isGeminiBatchEmbedding(raw []byte, endpointPath string) bool {
	if strings.Contains(endpointPath, "batchEmbed") {
		return true
	}
	// Quick probe: check top-level keys.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, hasRequests := probe["requests"]
	return hasRequests
}

// geminiEmbeddingsFieldSpec returns the declared top-level wire keys for
// Gemini's embedding surfaces.
func geminiEmbeddingsFieldSpec(d core.Direction, batch bool) core.FieldSpec {
	if d == core.DirectionRequest {
		if batch {
			return core.FieldSpec{
				Required: []string{"requests"},
				Optional: []string{},
			}
		}
		return core.FieldSpec{
			Required: []string{"content"},
			Optional: []string{"model", "outputDimensionality", "taskType", "title"},
		}
	}
	// Response.
	if batch {
		return core.FieldSpec{
			Required: []string{"embeddings"},
			Optional: []string{},
		}
	}
	return core.FieldSpec{
		Required: []string{"embedding"},
		Optional: []string{},
	}
}

// Single :embedContent

type geminiEmbedContentRequest struct {
	Model   string         `json:"model,omitempty"`
	Content *geminiContent `json:"content,omitempty"`
}

func (n *GeminiEmbeddingsNormalizer) normalizeSingleRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req geminiEmbedContentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: single request unmarshal: %w", err)
	}
	if req.Content == nil {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: missing content: %w", core.ErrUnsupported)
	}

	inputs := geminiEmbedContentToInputs(req.Content)

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-embeddings",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Inputs:           inputs,
	}
	return out, nil
}

// geminiEmbedContentToInputs extracts text strings from a geminiContent
// object's parts[]. Non-text parts are silently skipped (images, function
// calls, etc. are not meaningful as embedding inputs in this surface).
func geminiEmbedContentToInputs(c *geminiContent) []string {
	var inputs []string
	for _, part := range c.Parts {
		if part.Text != nil && *part.Text != "" {
			inputs = append(inputs, *part.Text)
		}
	}
	return inputs
}

// geminiEmbedContentResponse wraps the single :embedContent response.
// The embedding values are intentionally not parsed per SDD §T2.3.
type geminiEmbedContentResponse struct {
	Embedding *struct {
		Values json.RawMessage `json:"values"`
	} `json:"embedding,omitempty"`
}

func (n *GeminiEmbeddingsNormalizer) normalizeSingleResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp geminiEmbedContentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: single response unmarshal: %w", err)
	}
	if resp.Embedding == nil {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: response missing embedding: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-embeddings",
		Model:            meta.Model,
		// Inputs intentionally nil on response side.
		// Embedding vectors intentionally not stored per SDD §T2.3.
	}
	return out, nil
}

// Batch :batchEmbedContents

type geminiBatchEmbedRequest struct {
	Requests []geminiEmbedContentRequest `json:"requests"`
}

func (n *GeminiEmbeddingsNormalizer) normalizeBatchRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req geminiBatchEmbedRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: batch request unmarshal: %w", err)
	}
	if len(req.Requests) == 0 {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: batch requests[] empty: %w", core.ErrUnsupported)
	}

	// Collect the model from the first sub-request that has one.
	model := meta.Model
	var inputs []string
	for _, subReq := range req.Requests {
		if model == "" && subReq.Model != "" {
			model = subReq.Model
		}
		if subReq.Content != nil {
			inputs = append(inputs, geminiEmbedContentToInputs(subReq.Content)...)
		}
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-embeddings",
		Model:            model,
		Inputs:           inputs,
	}
	return out, nil
}

// geminiBatchEmbedResponse wraps the :batchEmbedContents response.
// The embedding values are intentionally not parsed per SDD §T2.3.
type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values json.RawMessage `json:"values"`
	} `json:"embeddings"`
}

func (n *GeminiEmbeddingsNormalizer) normalizeBatchResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp geminiBatchEmbedResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: batch response unmarshal: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return zeroEmbeddingPayload("gemini-embeddings", meta), fmt.Errorf("gemini-embeddings: batch response missing embeddings[]: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIEmbedding,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-embeddings",
		Model:            meta.Model,
		// Inputs intentionally nil on response side.
		// Embedding vectors intentionally not stored per SDD §T2.3.
	}
	return out, nil
}
