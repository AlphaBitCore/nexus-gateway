// Package cohere — embedding canonicalization helpers.
//
// Cross-format embedding routing uses OpenAI /v1/embeddings as the
// canonical hub shape. These helpers translate Cohere /v1/embed (and v2,
// wire-equivalent for embeddings) requests into the canonical shape and
// canonical responses back into Cohere wire shape.
//
// Per-provider extensions ride under nexus.ext.cohere.<key> via [canonicalext].
package cohere

import (
	"encoding/json"
	"fmt"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EmbedRequestToCanonical translates a Cohere /v1/embed request body
// (texts, model, input_type, embedding_types, truncate) into the OpenAI
// canonical embedding shape:
//
//	{
//	    "model":           "<providerModelID or body.model>",
//	    "input":           ["..."] | "...",
//	    "nexus.ext.cohere.input_type":      "search_query",
//	    "nexus.ext.cohere.embedding_types": ["float"],
//	    "nexus.ext.cohere.truncate":        "END"
//	}
//
// Non-canonical fields are surfaced via the canonicalext namespace so
// downstream codecs that target Cohere can re-materialise them on the
// wire; targets that ignore the namespace simply drop them.
//
// providerModelID is used only when the body omits "model".
func EmbedRequestToCanonical(body []byte, providerModelID string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "cohere embed: invalid JSON request body",
		}
	}

	texts := gjson.GetBytes(body, "texts")
	if !texts.Exists() || !texts.IsArray() {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "cohere embed: missing or non-array 'texts' field",
		}
	}

	// Canonical input mirrors OpenAI: array of strings.
	inputs := make([]string, 0, 4)
	var parseErr error
	texts.ForEach(func(_, v gjson.Result) bool {
		if v.Type != gjson.String {
			parseErr = &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "cohere embed: non-string element in 'texts'",
			}
			return false
		}
		inputs = append(inputs, v.Str)
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}

	model := gjson.GetBytes(body, "model").Str
	if model == "" {
		model = providerModelID
	}
	if model == "" {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "cohere embed: missing 'model'",
		}
	}

	canonical := []byte(`{}`)
	canonical, _ = sjson.SetBytes(canonical, "model", model)
	// Single-element batch collapses to a bare string per OpenAI convention.
	if len(inputs) == 1 {
		canonical, _ = sjson.SetBytes(canonical, "input", inputs[0])
	} else {
		canonical, _ = sjson.SetBytes(canonical, "input", inputs)
	}

	// Cohere-specific fields ride under nexus.ext.cohere.*.
	if v := gjson.GetBytes(body, "input_type"); v.Exists() && v.Str != "" {
		c, err := canonicalext.Set(canonical, "cohere", "input_type", v.Str)
		if err != nil {
			return nil, fmt.Errorf("cohere embed: stamp input_type: %w", err)
		}
		canonical = c
	}
	if v := gjson.GetBytes(body, "embedding_types"); v.IsArray() {
		types := make([]string, 0)
		v.ForEach(func(_, t gjson.Result) bool {
			if t.Type == gjson.String {
				types = append(types, t.Str)
			}
			return true
		})
		if len(types) > 0 {
			c, err := canonicalext.Set(canonical, "cohere", "embedding_types", types)
			if err != nil {
				return nil, fmt.Errorf("cohere embed: stamp embedding_types: %w", err)
			}
			canonical = c
		}
	}
	if v := gjson.GetBytes(body, "truncate"); v.Exists() && v.Str != "" {
		c, err := canonicalext.Set(canonical, "cohere", "truncate", v.Str)
		if err != nil {
			return nil, fmt.Errorf("cohere embed: stamp truncate: %w", err)
		}
		canonical = c
	}

	return canonical, nil
}

// CanonicalToEmbedResponse converts a canonical OpenAI-shape embedding
// response into the Cohere wire shape:
//
//	{
//	    "id":            "<request-id-or-omitted>",
//	    "response_type": "embeddings_floats",
//	    "embeddings":    [[…], [...]],
//	    "meta":          {"billed_units": {"input_tokens": N}}
//	}
//
// The Cohere-native "texts" echo field is dropped because canonical
// responses do not carry the original input texts; clients that need it
// already have it locally.
func CanonicalToEmbedResponse(canonical []byte) ([]byte, error) {
	if !gjson.ValidBytes(canonical) {
		return nil, fmt.Errorf("cohere embed response: invalid canonical body")
	}

	data := gjson.GetBytes(canonical, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("cohere embed response: canonical body missing data[]")
	}

	embeddings := make([][]float64, 0, 4)
	var parseErr error
	data.ForEach(func(_, item gjson.Result) bool {
		vec := item.Get("embedding")
		if !vec.IsArray() {
			parseErr = fmt.Errorf("cohere embed response: data[].embedding missing or not array")
			return false
		}
		row := make([]float64, 0, vec.Get("#").Int())
		vec.ForEach(func(_, n gjson.Result) bool {
			row = append(row, n.Float())
			return true
		})
		embeddings = append(embeddings, row)
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}

	out := map[string]any{
		"response_type": "embeddings_floats",
		"embeddings":    embeddings,
	}
	if id := gjson.GetBytes(canonical, "id").Str; id != "" {
		out["id"] = id
	}
	if u := gjson.GetBytes(canonical, "usage.prompt_tokens"); u.Exists() {
		out["meta"] = map[string]any{
			"billed_units": map[string]any{
				"input_tokens": u.Int(),
			},
		}
	}
	return json.Marshal(out)
}
