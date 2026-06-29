// Package cohere — embedding codec helpers (canonical ↔ Cohere /v1/embed wire).
//
// Architecture references:
//   - docs/dev/architecture/provider-adapter-architecture.md §3a Rules 1-7
//   - docs/dev/architecture/endpoint-typology-architecture.md §2
//
// Cohere v3 models (embed-english-v3.0, embed-multilingual-v3.0) require
// the input_type field. Observed 400 "invalid_request_error: input_type is
// required for Cohere embed-english-v3.0" (Cohere docs, observed behavior).
//
// Cohere v2 models do not require input_type; it is optional but recommended.
// The embed-english-light-v2.0 and embed-multilingual-v2.0 lines are fixed-
// dimension and do not accept a "dimensions" parameter.
package cohere

import (
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"regexp"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// cohereV3Regex matches Cohere v3 model families that require input_type.
// Observed 400 "input_type is required" for embed-english-v3.0 and
// embed-multilingual-v3.0 (Cohere API docs, observed behavior).
var cohereV3Regex = regexp.MustCompile(`^embed-(english|multilingual)-v3`)

// canonicalToCohereEmbed translates a canonical OpenAI-shape embedding request
// into the Cohere /v1/embed wire body.
//
// Mapping (per SDD §T3.2):
//   - canonical input (string)   → wire texts: ["..."]
//   - canonical input ([]string) → wire texts: [...]
//   - canonical input (tokens)   → safety-net 400 (Cohere does not support token arrays)
//   - canonical model            → wire model
//   - canonical dimensions       → ignored (Cohere models are fixed-dimension)
//   - canonical encoding_format  → if "float" → embedding_types: ["float"]; if "base64" → 400
//   - nexus.ext.cohere.input_type → wire input_type (required for v3 models)
//   - nexus.ext.cohere.embedding_types → wire embedding_types (overrides encoding_format derivation)
//   - nexus.ext.cohere.truncate   → wire truncate (NONE/START/END; default END)
func canonicalToCohereEmbed(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{ContentType: "application/json"}, nil
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "cohere embed: invalid canonical JSON body",
		}
	}

	// -- Model --
	model := target.ProviderModelID
	if model == "" {
		model = gjson.GetBytes(canonicalBody, "model").Str
	}

	// -- Input → texts --
	inputVal := gjson.GetBytes(canonicalBody, "input")
	if !inputVal.Exists() {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "cohere embed: canonical 'input' field is missing",
		}
	}

	var texts []string
	switch {
	case inputVal.Type == gjson.String:
		// Single string → wrap into a single-element array.
		texts = []string{inputVal.Str}
	case inputVal.IsArray():
		arr := inputVal.Array()
		if len(arr) == 0 {
			texts = []string{}
		} else {
			first := arr[0]
			switch {
			case first.Type == gjson.String:
				// Array of strings.
				texts = make([]string, 0, len(arr))
				for _, el := range arr {
					if el.Type != gjson.String {
						return provcore.EncodeResult{}, &provcore.ProviderError{
							Status:  http.StatusBadRequest,
							Code:    provcore.CodeInvalidRequest,
							Message: "cohere embed: mixed-type input array",
						}
					}
					texts = append(texts, el.Str)
				}
			case first.Type == gjson.Number:
				// Array of integers → token array, unsupported by Cohere.
				// Cohere does not expose a tokenized embedding endpoint.
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "cohere embed: token_array_unsupported_by_cohere — Cohere /v1/embed does not accept integer token inputs; use string inputs instead",
				}
			case first.IsArray():
				// Array of arrays → batch token input, unsupported.
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "cohere embed: token_array_unsupported_by_cohere — Cohere /v1/embed does not accept token array inputs",
				}
			default:
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "cohere embed: unsupported input array element type",
				}
			}
		}
	default:
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "cohere embed: 'input' must be a string or array of strings",
		}
	}

	// -- Start building the wire body --
	wire := []byte(`{}`)
	if model != "" {
		wire, _ = sjson.SetBytes(wire, "model", model)
	}
	wire, _ = sjson.SetBytes(wire, "texts", texts)

	// -- encoding_format → embedding_types (before ext override) --
	var embeddingTypes []string
	if ef := gjson.GetBytes(canonicalBody, "encoding_format"); ef.Exists() {
		switch ef.Str {
		case "float", "":
			embeddingTypes = []string{"float"}
		case "base64":
			// Cohere has no direct base64 embedding type equivalent.
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "cohere embed: encoding_format 'base64' has no Cohere wire equivalent; use 'float' or omit the field",
			}
		}
	}

	// -- nexus.ext.cohere.embedding_types (overrides encoding_format derivation) --
	if extTypes := canonicalext.Get(canonicalBody, "cohere", "embedding_types"); extTypes.IsArray() {
		overrideTypes := make([]string, 0)
		extTypes.ForEach(func(_, t gjson.Result) bool {
			if t.Type == gjson.String {
				overrideTypes = append(overrideTypes, t.Str)
			}
			return true
		})
		if len(overrideTypes) > 0 {
			embeddingTypes = overrideTypes
		}
	}

	if len(embeddingTypes) > 0 {
		wire, _ = sjson.SetBytes(wire, "embedding_types", embeddingTypes)
	}

	// -- nexus.ext.cohere.input_type --
	inputType := ""
	if extInputType := canonicalext.Get(canonicalBody, "cohere", "input_type"); extInputType.Type == gjson.String {
		inputType = extInputType.Str
	}

	// v3 models require input_type — observed 400 "input_type is required for
	// Cohere embed-english-v3.0" (Cohere API docs, observed behavior). Default
	// to "search_document" when the caller omits nexus.ext.cohere.input_type,
	// matching the Bedrock-Cohere codec (embed_cohere_codec.go) so the two
	// Cohere adapters agree on the missing-input_type default instead of one
	// rejecting where the capability filter (filter.go Rule 4 only checks a
	// non-empty input_type) already admitted the request.
	if inputType == "" && model != "" && cohereV3Regex.MatchString(model) {
		inputType = "search_document"
	}

	if inputType != "" {
		wire, _ = sjson.SetBytes(wire, "input_type", inputType)
	}

	// -- nexus.ext.cohere.truncate (default: END) --
	truncate := "END"
	if extTruncate := canonicalext.Get(canonicalBody, "cohere", "truncate"); extTruncate.Type == gjson.String && extTruncate.Str != "" {
		truncate = extTruncate.Str
	}
	wire, _ = sjson.SetBytes(wire, "truncate", truncate)

	return provcore.EncodeResult{Body: wire, ContentType: "application/json"}, nil
}

// cohereEmbedResponseToCanonical converts a Cohere /v1/embed response into
// the canonical OpenAI embeddings shape.
//
// Cohere wire response shapes:
//
//	Single embedding_type (float):
//	  {"id":"…","embeddings":[[0.1,0.2,…],[…]],"texts":["…"],"meta":{…}}
//
//	Multi embedding_types:
//	  {"id":"…","embeddings":{"float":[[…],[…]],"int8":[[…],[…]]},"texts":["…"],"meta":{…}}
//
// Canonical output:
//
//	{"object":"list","data":[{"object":"embedding","embedding":[…],"index":0},…],
//	 "model":"<from response or empty>","usage":{"prompt_tokens":N,"total_tokens":N}}
func cohereEmbedResponseToCanonical(nativeBody, reqBody []byte) (provcore.DecodeResult, error) {
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("cohere embed response: invalid JSON body")
	}

	embeddingsVal := gjson.GetBytes(nativeBody, "embeddings")

	var floatRows [][]float64
	// returnedEmbeddingType records which embedding type key was selected when
	// the Cohere response carries a multi-type object (Case 2 below). It is
	// stamped into nexus.ext.cohere.returned_embedding_type on the canonical
	// body so audit consumers can see which representation was forwarded without
	// inspecting the raw Cohere response. It is NOT emitted on the OpenAI wire
	// shape returned to the caller — it is metadata only.
	var returnedEmbeddingType string

	if embeddingsVal.IsArray() {
		// Case 1: flat array of float arrays — single embedding type.
		embeddingsVal.ForEach(func(_, row gjson.Result) bool {
			if row.IsArray() {
				vec := make([]float64, 0, 256)
				row.ForEach(func(_, n gjson.Result) bool {
					vec = append(vec, n.Float())
					return true
				})
				floatRows = append(floatRows, vec)
			}
			return true
		})
	} else if embeddingsVal.IsObject() {
		// Case 2: object with per-type keys — prefer "float", else first key.
		// Record the selected type name for audit metadata.
		floatKey := embeddingsVal.Get("float")
		if floatKey.IsArray() {
			returnedEmbeddingType = "float"
		} else {
			// Fall back to first key.
			embeddingsVal.ForEach(func(k, v gjson.Result) bool {
				if v.IsArray() {
					floatKey = v
					returnedEmbeddingType = k.Str
					return false // stop after first
				}
				return true
			})
		}
		if floatKey.IsArray() {
			floatKey.ForEach(func(_, row gjson.Result) bool {
				if row.IsArray() {
					vec := make([]float64, 0, 256)
					row.ForEach(func(_, n gjson.Result) bool {
						vec = append(vec, n.Float())
						return true
					})
					floatRows = append(floatRows, vec)
				}
				return true
			})
		}
	}

	// Guard against a provider silently dropping or reordering items: a
	// count mismatch means the position-indexed vectors no longer align
	// with the request `texts`. Fail the decode (→ 502)
	// rather than serve misaligned vectors.
	if err := specutil.ValidateEmbeddingRowCount(int(gjson.GetBytes(reqBody, "texts.#").Int()), len(floatRows)); err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("cohere embed response: %w", err)
	}

	// Build canonical data[] array.
	data := make([]map[string]any, 0, len(floatRows))
	for i, vec := range floatRows {
		data = append(data, map[string]any{
			"object":    "embedding",
			"embedding": vec,
			"index":     i,
		})
	}

	// Extract usage from meta.billed_units.input_tokens.
	var promptTokens int64
	if bt := gjson.GetBytes(nativeBody, "meta.billed_units.input_tokens"); bt.Exists() {
		promptTokens = bt.Int()
	}

	// Build canonical response.
	canonical := map[string]any{
		"object": "list",
		"data":   data,
		"model":  gjson.GetBytes(nativeBody, "model").Str,
		"usage": map[string]any{
			"prompt_tokens": promptTokens,
			"total_tokens":  promptTokens,
		},
	}

	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("cohere embed response: marshal canonical: %w", err)
	}

	// Stamp the returned embedding type into nexus.ext.cohere.returned_embedding_type
	// when a multi-type response object was present. This field is for audit and
	// downstream metadata only — it is NOT forwarded to the OpenAI-shape wire
	// response (EncodeOpenAIEmbeddingsResponse strips nexus.ext.* before sending
	// the canonical body back to the caller).
	if returnedEmbeddingType != "" {
		stamped, stampErr := canonicalext.Set(canonicalBytes, "cohere", "returned_embedding_type", returnedEmbeddingType)
		if stampErr == nil {
			canonicalBytes = stamped
		}
	}

	// Extract usage via the shared normalizer path.
	usage := provcore.ExtractUsage(canonicalBytes, provcore.FormatOpenAI)

	return provcore.DecodeResult{CanonicalBody: canonicalBytes, Usage: usage}, nil
}
