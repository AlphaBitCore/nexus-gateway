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
//
// Embed v4 broke that "all Cohere models are fixed-dimension" assumption.
// Probed against api.cohere.com/v2/embed on 2026-08-19: embed-v4.0 accepts
// output_dimension 256/512/1024/1536 and returns a vector exactly that wide,
// answers 422 for anything else ("[256 512 1024 1536] is supported"), and
// defaults to 1536 when the field is omitted; embed-english-v3.0 answers 400
// to the field's mere presence. So the parameter is carried for v4 and still
// withheld from v3 — a fixed SET, which is why the catalog row enumerates
// rather than declaring a min/max range.
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

// cohereOutputDimensionRegex matches the Cohere embedding models that accept
// an output_dimension. Generation-gated rather than an exact suffix list for
// the same reason the DeepSeek thinking gate is: an exact list goes stale in
// the direction that hurts, and the next v4-line model would silently drop the
// caller's width again. v3 and v2 are excluded because they reject the field.
var cohereOutputDimensionRegex = regexp.MustCompile(`^embed-v[4-9]`)

// canonicalToCohereEmbed translates a canonical OpenAI-shape embedding request
// into the Cohere /v1/embed wire body.
//
// Mapping (per SDD §T3.2):
//   - canonical input (string)   → wire texts: ["..."]
//   - canonical input ([]string) → wire texts: [...]
//   - canonical input (tokens)   → safety-net 400 (Cohere does not support token arrays)
//   - canonical model            → wire model
//   - canonical dimensions       → wire output_dimension on models that accept
//     it (v4); withheld on v3/v2, which reject the field outright
//   - canonical encoding_format  → embedding_types: ["float"] for both "float" and
//     "base64"; Cohere's wire has no base64 type, so the ingress response layer
//     re-encodes for a caller who asked for it (see the encoding_format branch below)
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

	// -- dimensions → output_dimension (v4 and later only) --
	// Withheld on v3/v2: they answer 400 to the field's presence, so sending it
	// would turn a servable request into a failure. On v4 the opposite holds —
	// dropping it returned a 1536-wide vector to a caller who asked for 512,
	// and a vector of the wrong width is a wrong answer with no error attached,
	// not a clamped one.
	if d := gjson.GetBytes(canonicalBody, "dimensions"); d.Exists() && d.Int() > 0 &&
		cohereOutputDimensionRegex.MatchString(model) {
		wire, _ = sjson.SetBytes(wire, "output_dimension", d.Int())
	}

	// -- encoding_format → embedding_types (before ext override) --
	var embeddingTypes []string
	if ef := gjson.GetBytes(canonicalBody, "encoding_format"); ef.Exists() {
		switch ef.Str {
		case "float", "":
			embeddingTypes = []string{"float"}
		case "base64":
			// Cohere's wire has no base64 embedding type, so ask for float and let
			// the ingress response layer re-encode to base64 for the caller
			// (honorEmbeddingEncodingFormat in internal/ingress/proxy). Rejecting
			// used to be the safe choice, but the official OpenAI SDKs send
			// encoding_format="base64" IMPLICITLY when the caller omits it, so a
			// 400 here broke a stock `embeddings.create(model="embed-english-v3.0")`
			// outright — observed on staging 2026-07-27.
			embeddingTypes = []string{"float"}
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
		floatKey := embeddingsVal.Get("float")
		if !floatKey.IsArray() {
			// Fall back to first key.
			embeddingsVal.ForEach(func(_, v gjson.Result) bool {
				if v.IsArray() {
					floatKey = v
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

	// Extract usage via the shared normalizer path.
	usage := provcore.ExtractUsage(canonicalBytes, provcore.FormatOpenAI)

	return provcore.DecodeResult{CanonicalBody: canonicalBytes, Usage: usage}, nil
}
