package bedrock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// encodeCohereEmbedRequest translates a canonical OpenAI-shape embedding request
// into the Cohere Embed on Bedrock wire format:
//
//	POST /model/cohere.embed-english-v3/invoke
//	{
//	    "texts":           ["...", "..."],            // required, []string
//	    "input_type":      "search_document",         // required
//	    "truncate":        "NONE" | "START" | "END",  // optional
//	    "embedding_types": ["float"]                  // optional
//	}
//
// Rule 3 (provider-adapter-architecture.md §3a): Cohere-on-Bedrock request quirks
// (mandatory input_type, texts array only, model omitted from body) live here,
// not in the generic dispatcher.
//
// Rule 4: Cohere-on-Bedrock per-model extensions ride under nexus.ext.bedrock.*:
//   - nexus.ext.bedrock.cohere_input_type     → wire input_type (required)
//   - nexus.ext.bedrock.cohere_truncate        → wire truncate ("NONE"|"START"|"END")
//   - nexus.ext.bedrock.cohere_embedding_types → wire embedding_types ([]string)
func encodeCohereEmbedRequest(canonicalBody []byte, _ provcore.CallTarget) (provcore.EncodeResult, error) {
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-cohere: invalid canonical JSON body",
		}
	}

	inputVal := gjson.GetBytes(canonicalBody, "input")
	if !inputVal.Exists() {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-cohere: canonical 'input' field is missing",
		}
	}

	var texts []string
	switch {
	case inputVal.Type == gjson.String:
		texts = []string{inputVal.Str}
	case inputVal.IsArray():
		arr := inputVal.Array()
		if len(arr) == 0 {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "bedrock-cohere: input array is empty",
			}
		}
		first := arr[0]
		if first.Type == gjson.Number || first.IsArray() {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "bedrock-cohere: token_array_unsupported — Cohere Embed on Bedrock does not accept integer token inputs",
			}
		}
		texts = make([]string, 0, len(arr))
		for _, el := range arr {
			if el.Type != gjson.String {
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "bedrock-cohere: mixed-type input array — all elements must be strings",
				}
			}
			texts = append(texts, el.Str)
		}
	default:
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-cohere: 'input' must be a string or array of strings",
		}
	}

	wire := []byte(`{}`)
	wire, _ = sjson.SetBytes(wire, "texts", texts)

	// input_type is required by Cohere Embed on Bedrock. Check nexus.ext
	// first, then fall back to the canonical representation if set.
	inputType := ""
	if it := canonicalext.Get(canonicalBody, "bedrock", "cohere_input_type"); it.Type == gjson.String && it.Str != "" {
		inputType = it.Str
	}
	// Default to "search_document" when no explicit input_type provided —
	// Cohere Embed on Bedrock requires this field; without it the API
	// returns HTTP 400 (observed Bedrock behavior).
	if inputType == "" {
		inputType = "search_document"
	}
	wire, _ = sjson.SetBytes(wire, "input_type", inputType)

	// nexus.ext.bedrock.cohere_truncate → wire truncate ("NONE"|"START"|"END").
	if tr := canonicalext.Get(canonicalBody, "bedrock", "cohere_truncate"); tr.Type == gjson.String && tr.Str != "" {
		wire, _ = sjson.SetBytes(wire, "truncate", tr.Str)
	}

	// nexus.ext.bedrock.cohere_embedding_types → wire embedding_types ([]string).
	if et := canonicalext.Get(canonicalBody, "bedrock", "cohere_embedding_types"); et.IsArray() {
		types := make([]string, 0, 2)
		et.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				types = append(types, v.Str)
			}
			return true
		})
		if len(types) > 0 {
			wire, _ = sjson.SetBytes(wire, "embedding_types", types)
		}
	}

	return provcore.EncodeResult{Body: wire, ContentType: "application/json"}, nil
}

// decodeCohereEmbedResponse translates a Cohere Embed on Bedrock response into
// the canonical OpenAI-shape embedding response.
//
// Cohere Embed on Bedrock response:
//
//	{
//	    "embeddings":    [[0.1, 0.2, ...], ...],
//	    "id":            "...",
//	    "response_type": "embeddings_floats",
//	    "texts":         ["...", "..."]
//	}
//
// Note: Cohere Embed on Bedrock does NOT include a usage field — token counts
// are not returned. Usage is set to zeros in the canonical response.
func decodeCohereEmbedResponse(nativeBody []byte, modelID string) (provcore.DecodeResult, error) {
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-cohere: invalid JSON response body")
	}

	embVal := gjson.GetBytes(nativeBody, "embeddings")
	var data []map[string]any
	if embVal.IsArray() {
		arr := embVal.Array()
		data = make([]map[string]any, 0, len(arr))
		for i, item := range arr {
			var row []float64
			if item.IsArray() {
				row = make([]float64, 0, 256)
				item.ForEach(func(_, n gjson.Result) bool {
					row = append(row, n.Float())
					return true
				})
			}
			data = append(data, map[string]any{
				"object":    "embedding",
				"embedding": row,
				"index":     i,
			})
		}
	}

	canonical := map[string]any{
		"object": "list",
		"data":   data,
		"model":  modelID,
		// Cohere Embed on Bedrock does not return token usage; zeros satisfy
		// the canonical shape without misleading downstream consumers.
		"usage": map[string]any{
			"prompt_tokens": 0,
			"total_tokens":  0,
		},
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-cohere: marshal canonical: %w", err)
	}

	// Stamp the model in nexus.ext.bedrock.model for audit consumers.
	if modelID != "" {
		stamped, stampErr := canonicalext.Set(canonicalBytes, "bedrock", "model", modelID)
		if stampErr == nil {
			canonicalBytes = stamped
		}
	}

	// No meaningful usage to extract — Cohere on Bedrock omits token counts.
	return provcore.DecodeResult{CanonicalBody: canonicalBytes}, nil
}

// isCohereEmbedModel reports whether the given modelID refers to a Cohere
// Embed model on Bedrock. Per observed Bedrock model IDs:
//   - cohere.embed-english-v3       (1024 dims)
//   - cohere.embed-multilingual-v3  (1024 dims)
func isCohereEmbedModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.HasPrefix(lower, "cohere.embed")
}
