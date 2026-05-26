// Package voyage — embedding canonicalization helpers.
//
// Cross-format embedding routing uses OpenAI /v1/embeddings as the
// canonical hub shape. These helpers translate Voyage AI /v1/embeddings
// requests into the canonical shape and canonical responses back into
// the Voyage AI wire shape.
//
// Per-provider extensions ride under nexus.ext.voyage.<key> via [canonicalext].
//
// Voyage AI provider-specific extension fields:
//   - nexus.ext.voyage.input_type    — string: "query" | "document"
//   - nexus.ext.voyage.output_dtype  — string: "float" | "int8" | "uint8" | "binary" | "ubinary"
//   - nexus.ext.voyage.output_dimension — int: model-dependent supported dimension
//   - nexus.ext.voyage.truncation    — bool
package voyage

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EmbedRequestToCanonical translates a Voyage AI /v1/embeddings request body
// into the OpenAI canonical embedding shape:
//
//	{
//	    "model": "<model>",
//	    "input": "..." | ["..."],
//	    "nexus.ext.voyage.input_type":        "query",
//	    "nexus.ext.voyage.output_dtype":      "float",
//	    "nexus.ext.voyage.output_dimension":  1024,
//	    "nexus.ext.voyage.truncation":        true
//	}
//
// providerModelID is used only when the body omits "model".
func EmbedRequestToCanonical(body []byte, providerModelID string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage embed: invalid JSON request body",
		}
	}

	inputVal := gjson.GetBytes(body, "input")
	if !inputVal.Exists() {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage embed: missing 'input' field",
		}
	}

	model := gjson.GetBytes(body, "model").Str
	if model == "" {
		model = providerModelID
	}
	if model == "" {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage embed: missing 'model'",
		}
	}

	canonical := []byte(`{}`)
	canonical, _ = sjson.SetBytes(canonical, "model", model)

	// Voyage AI accepts string or []string. Mirror to canonical input.
	switch {
	case inputVal.Type == gjson.String:
		canonical, _ = sjson.SetBytes(canonical, "input", inputVal.Str)
	case inputVal.IsArray():
		inputs := make([]string, 0)
		var parseErr error
		inputVal.ForEach(func(_, v gjson.Result) bool {
			if v.Type != gjson.String {
				parseErr = &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "voyage embed: non-string element in 'input' array",
				}
				return false
			}
			inputs = append(inputs, v.Str)
			return true
		})
		if parseErr != nil {
			return nil, parseErr
		}
		canonical, _ = sjson.SetBytes(canonical, "input", inputs)
	default:
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage embed: 'input' must be a string or array of strings",
		}
	}

	// Voyage-specific extensions → nexus.ext.voyage.*
	var err error
	if it := gjson.GetBytes(body, "input_type"); it.Exists() && it.Str != "" {
		canonical, err = canonicalext.Set(canonical, "voyage", "input_type", it.Str)
		if err != nil {
			return nil, fmt.Errorf("voyage embed: stamp input_type: %w", err)
		}
	}
	if od := gjson.GetBytes(body, "output_dtype"); od.Exists() && od.Str != "" {
		canonical, err = canonicalext.Set(canonical, "voyage", "output_dtype", od.Str)
		if err != nil {
			return nil, fmt.Errorf("voyage embed: stamp output_dtype: %w", err)
		}
	}
	if dim := gjson.GetBytes(body, "output_dimension"); dim.Exists() && dim.Type == gjson.Number {
		canonical, err = canonicalext.Set(canonical, "voyage", "output_dimension", dim.Int())
		if err != nil {
			return nil, fmt.Errorf("voyage embed: stamp output_dimension: %w", err)
		}
	}
	if tr := gjson.GetBytes(body, "truncation"); tr.Exists() && tr.Type == gjson.True || tr.Type == gjson.False {
		canonical, err = canonicalext.Set(canonical, "voyage", "truncation", tr.Bool())
		if err != nil {
			return nil, fmt.Errorf("voyage embed: stamp truncation: %w", err)
		}
	}

	return canonical, nil
}

// CanonicalToEmbedResponse converts a canonical OpenAI-shape embedding
// response into the Voyage AI wire shape:
//
//	{
//	    "object": "list",
//	    "data":   [{"object":"embedding","embedding":[...],"index":0}],
//	    "model":  "<model>",
//	    "usage":  {"total_tokens": N}
//	}
//
// Voyage AI's response shape is nearly identical to OpenAI's; the only
// field that differs is usage (Voyage uses total_tokens, no prompt/completion split).
func CanonicalToEmbedResponse(canonical []byte) ([]byte, error) {
	if !gjson.ValidBytes(canonical) {
		return nil, fmt.Errorf("voyage embed response: invalid canonical body")
	}

	data := gjson.GetBytes(canonical, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("voyage embed response: canonical body missing data[]")
	}

	type embItem struct {
		Object    string    `json:"object"`
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	}
	items := make([]embItem, 0, 4)
	var parseErr error
	data.ForEach(func(_, item gjson.Result) bool {
		vec := item.Get("embedding")
		if !vec.IsArray() {
			parseErr = fmt.Errorf("voyage embed response: data[].embedding missing or not array")
			return false
		}
		row := make([]float64, 0, int(vec.Get("#").Int()))
		vec.ForEach(func(_, n gjson.Result) bool {
			row = append(row, n.Float())
			return true
		})
		items = append(items, embItem{
			Object:    "embedding",
			Embedding: row,
			Index:     int(item.Get("index").Int()),
		})
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}

	totalTokens := gjson.GetBytes(canonical, "usage.total_tokens").Int()
	if totalTokens == 0 {
		// Fall back to prompt_tokens (canonical convention).
		totalTokens = gjson.GetBytes(canonical, "usage.prompt_tokens").Int()
	}

	out := map[string]any{
		"object": "list",
		"data":   items,
		"model":  gjson.GetBytes(canonical, "model").Str,
		"usage":  map[string]any{"total_tokens": totalTokens},
	}
	return json.Marshal(out)
}
