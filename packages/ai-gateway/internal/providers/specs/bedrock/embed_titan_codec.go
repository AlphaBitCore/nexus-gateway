package bedrock

import (
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// encodeTitanEmbedRequest translates a canonical OpenAI-shape embedding request
// into the Amazon Titan Embeddings V2 / V1 wire format:
//
//	POST /model/amazon.titan-embed-text-v2:0/invoke
//	{
//	    "inputText":      "...",                      // required, string only
//	    "dimensions":     1024,                        // optional, V2 only
//	    "normalize":      true,                        // optional, V2 only
//	    "embeddingTypes": ["float"]                    // optional, V2 only
//	}
//
// Rule 3 (provider-adapter-architecture.md §3a): Titan-specific request quirks
// (mandatory string input — no array support, model omitted from body) live here,
// not in the generic dispatcher.
//
// Rule 4: Bedrock-Titan per-model extensions ride under nexus.ext.bedrock.*:
//   - nexus.ext.bedrock.titan_normalize     → wire normalize (bool, V2 only)
//   - nexus.ext.bedrock.titan_embedding_types → wire embeddingTypes ([]string, V2 only)
func encodeTitanEmbedRequest(canonicalBody []byte, _ provcore.CallTarget) (provcore.EncodeResult, error) {
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-titan: invalid canonical JSON body",
		}
	}

	// Titan only accepts a single string input. Arrays and token arrays are
	// not supported (Bedrock Titan Embeddings API).
	inputVal := gjson.GetBytes(canonicalBody, "input")
	if !inputVal.Exists() {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-titan: canonical 'input' field is missing",
		}
	}

	var inputText string
	switch {
	case inputVal.Type == gjson.String:
		inputText = inputVal.Str
	case inputVal.IsArray():
		arr := inputVal.Array()
		if len(arr) == 0 {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "bedrock-titan: input array is empty — Titan Embeddings requires a non-empty string",
			}
		}
		first := arr[0]
		if first.Type == gjson.Number || first.IsArray() {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "bedrock-titan: token_array_unsupported — Titan Embeddings does not accept integer token inputs; use string inputs",
			}
		}
		if len(arr) > 1 {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "bedrock-titan: batch_unsupported — Titan Embeddings accepts only a single string input per request",
			}
		}
		inputText = first.Str
	default:
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-titan: 'input' must be a string or single-element array of strings",
		}
	}

	wire := []byte(`{}`)
	wire, _ = sjson.SetBytes(wire, "inputText", inputText)

	// Canonical `dimensions` → Titan V2 wire `dimensions`.
	if dim := gjson.GetBytes(canonicalBody, "dimensions"); dim.Exists() && dim.Type == gjson.Number {
		wire, _ = sjson.SetBytes(wire, "dimensions", dim.Int())
	} else if dim := canonicalext.Get(canonicalBody, "bedrock", "titan_dimensions"); dim.Type == gjson.Number {
		wire, _ = sjson.SetBytes(wire, "dimensions", dim.Int())
	}

	// nexus.ext.bedrock.titan_normalize → wire normalize (bool, V2 only).
	if norm := canonicalext.Get(canonicalBody, "bedrock", "titan_normalize"); norm.Type == gjson.True || norm.Type == gjson.False {
		wire, _ = sjson.SetBytes(wire, "normalize", norm.Bool())
	}

	// nexus.ext.bedrock.titan_embedding_types → wire embeddingTypes ([]string, V2 only).
	if et := canonicalext.Get(canonicalBody, "bedrock", "titan_embedding_types"); et.IsArray() {
		types := make([]string, 0, 2)
		et.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				types = append(types, v.Str)
			}
			return true
		})
		if len(types) > 0 {
			wire, _ = sjson.SetBytes(wire, "embeddingTypes", types)
		}
	}

	return provcore.EncodeResult{Body: wire, ContentType: "application/json"}, nil
}

// decodeTitanEmbedResponse translates a Titan Embeddings V2 / V1 response into
// the canonical OpenAI-shape embedding response:
//
//	{
//	    "embedding":            [0.1, 0.2, ...],  // float32 array
//	    "inputTextTokenCount":  N
//	}
//
// Canonical shape:
//
//	{
//	    "object": "list",
//	    "data":   [{"object":"embedding","embedding":[...],"index":0}],
//	    "model":  "<model>",
//	    "usage":  {"prompt_tokens": N, "total_tokens": N}
//	}
func decodeTitanEmbedResponse(nativeBody []byte, modelID string) (provcore.DecodeResult, error) {
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-titan: invalid JSON response body")
	}

	embVal := gjson.GetBytes(nativeBody, "embedding")
	var embedding []float64
	if embVal.IsArray() {
		embedding = make([]float64, 0, 256)
		embVal.ForEach(func(_, n gjson.Result) bool {
			embedding = append(embedding, n.Float())
			return true
		})
	}

	tokenCount := gjson.GetBytes(nativeBody, "inputTextTokenCount").Int()

	canonical := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{
				"object":    "embedding",
				"embedding": embedding,
				"index":     0,
			},
		},
		"model": modelID,
		"usage": map[string]any{
			"prompt_tokens": tokenCount,
			"total_tokens":  tokenCount,
		},
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-titan: marshal canonical: %w", err)
	}

	// Stamp the model in nexus.ext.bedrock.model for audit consumers.
	if modelID != "" {
		stamped, stampErr := canonicalext.Set(canonicalBytes, "bedrock", "model", modelID)
		if stampErr == nil {
			canonicalBytes = stamped
		}
	}

	usage := provcore.ExtractUsage(canonicalBytes, provcore.FormatOpenAI)
	return provcore.DecodeResult{CanonicalBody: canonicalBytes, Usage: usage}, nil
}

// isTitanEmbedModel reports whether the given modelID refers to an Amazon
// Titan Embeddings model. Per observed Bedrock model IDs:
//   - amazon.titan-embed-text-v2:0   (Titan Embeddings V2 — 256/512/1024 dims)
//   - amazon.titan-embed-text-v1     (Titan Embeddings V1 — 1536 dims)
func isTitanEmbedModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.HasPrefix(lower, "amazon.titan-embed")
}
