package voyage

import (
	"fmt"
	"github.com/goccy/go-json"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codec implements provcore.SchemaCodec for Voyage AI's /v1/embeddings surface.
//
// EncodeRequest translates canonical OpenAI embedding shape → Voyage AI wire.
// DecodeResponse translates Voyage AI response → canonical OpenAI embedding shape.
//
// Voyage AI wire request:
//
//	{model, input: string|[]string, input_type?, truncation?, output_dimension?, output_dtype?}
//
// Voyage AI wire response (identical to OpenAI embeddings shape):
//
//	{object:"list", data:[{object:"embedding", embedding:[...], index:N}],
//	 model, usage:{total_tokens:N}}
type codec struct{}

func newCodec() provcore.SchemaCodec {
	return codec{}
}

// EncodeRequest converts canonical OpenAI-shape embedding request to Voyage AI wire.
// Non-embeddings endpoints are rejected.
func (codec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if endpoint != typology.WireShapeVoyageEmbeddings {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeEndpointUnsupported,
			Message: fmt.Sprintf("voyage: unsupported endpoint %q — only embeddings is available", endpoint),
		}
	}
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{ContentType: "application/json"}, nil
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: invalid canonical JSON body",
		}
	}

	// Model: prefer target.ProviderModelID, fall back to canonical body.
	model := target.ProviderModelID
	if model == "" {
		model = gjson.GetBytes(canonicalBody, "model").Str
	}
	if model == "" {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: missing model — set via CallTarget.ProviderModelID or canonical body",
		}
	}

	// Input → Voyage wire `input` (string or []string).
	inputVal := gjson.GetBytes(canonicalBody, "input")
	if !inputVal.Exists() {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: canonical 'input' field is missing",
		}
	}

	wire := []byte(`{}`)
	wire, _ = sjson.SetBytes(wire, "model", model)

	switch {
	case inputVal.Type == gjson.String:
		wire, _ = sjson.SetBytes(wire, "input", inputVal.Str)
	case inputVal.IsArray():
		arr := inputVal.Array()
		if len(arr) == 0 {
			wire, _ = sjson.SetBytes(wire, "input", []string{})
		} else {
			first := arr[0]
			if first.Type == gjson.Number || first.IsArray() {
				// Token inputs are not supported by Voyage AI.
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "voyage: token_array_unsupported — Voyage AI /v1/embeddings does not accept integer token inputs; use string inputs",
				}
			}
			inputs := make([]string, 0, len(arr))
			for _, el := range arr {
				if el.Type != gjson.String {
					return provcore.EncodeResult{}, &provcore.ProviderError{
						Status:  http.StatusBadRequest,
						Code:    provcore.CodeInvalidRequest,
						Message: "voyage: mixed-type input array — all elements must be strings",
					}
				}
				inputs = append(inputs, el.Str)
			}
			wire, _ = sjson.SetBytes(wire, "input", inputs)
		}
	default:
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: 'input' must be a string or array of strings",
		}
	}

	// Voyage-specific extension fields (Rule 4: per-provider extensions via nexus.ext).
	// nexus.ext.voyage.input_type → wire input_type
	if it := canonicalext.Get(canonicalBody, "voyage", "input_type"); it.Type == gjson.String && it.Str != "" {
		wire, _ = sjson.SetBytes(wire, "input_type", it.Str)
	}
	// nexus.ext.voyage.output_dtype → wire output_dtype
	if od := canonicalext.Get(canonicalBody, "voyage", "output_dtype"); od.Type == gjson.String && od.Str != "" {
		wire, _ = sjson.SetBytes(wire, "output_dtype", od.Str)
	}
	// nexus.ext.voyage.output_dimension → wire output_dimension
	// Also translate canonical dimensions field when no explicit extension.
	if dim := canonicalext.Get(canonicalBody, "voyage", "output_dimension"); dim.Type == gjson.Number {
		wire, _ = sjson.SetBytes(wire, "output_dimension", dim.Int())
	} else if dim := gjson.GetBytes(canonicalBody, "dimensions"); dim.Exists() && dim.Type == gjson.Number {
		// dimensions is the canonical OpenAI field — forward as output_dimension.
		wire, _ = sjson.SetBytes(wire, "output_dimension", dim.Int())
	}
	// nexus.ext.voyage.truncation → wire truncation (bool)
	if tr := canonicalext.Get(canonicalBody, "voyage", "truncation"); tr.Type == gjson.True || tr.Type == gjson.False {
		wire, _ = sjson.SetBytes(wire, "truncation", tr.Bool())
	}

	return provcore.EncodeResult{Body: wire, ContentType: "application/json"}, nil
}

// DecodeResponse converts a Voyage AI /v1/embeddings response into canonical
// OpenAI embedding shape.
//
// Voyage AI's response is already very close to OpenAI's shape:
//
//	{object:"list", data:[{object:"embedding",embedding:[...],index:0}],
//	 model, usage:{total_tokens:N}}
//
// The only translation needed is usage.total_tokens → usage.prompt_tokens+total_tokens
// in the canonical shape.
func (codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string, reqCtx provcore.DecodeContext) (provcore.DecodeResult, error) {
	if endpoint != typology.WireShapeVoyageEmbeddings {
		// Unexpected — Voyage only serves embeddings; pass through opaquely.
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("voyage embed response: invalid JSON body")
	}

	dataVal := gjson.GetBytes(nativeBody, "data")
	var floatRows [][]float64
	if dataVal.IsArray() {
		dataVal.ForEach(func(_, item gjson.Result) bool {
			vec := item.Get("embedding")
			if vec.IsArray() {
				row := make([]float64, 0, 256)
				vec.ForEach(func(_, n gjson.Result) bool {
					row = append(row, n.Float())
					return true
				})
				floatRows = append(floatRows, row)
			}
			return true
		})
	}

	// Guard against a provider silently dropping or reordering items: the
	// canonical data[] is re-indexed by upstream position, so a count
	// mismatch means the vectors no longer align with the request inputs.
	// Fail the decode (→ 502) rather than serve misaligned
	// vectors. expected counts the Voyage wire `input` (string→1, array→len).
	if err := specutil.ValidateEmbeddingRowCount(voyageInputCount(reqCtx.RequestBody), len(floatRows)); err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("voyage embed response: %w", err)
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

	// Extract usage — Voyage reports total_tokens only.
	var totalTokens int64
	if tt := gjson.GetBytes(nativeBody, "usage.total_tokens"); tt.Exists() {
		totalTokens = tt.Int()
	}

	canonical := map[string]any{
		"object": "list",
		"data":   data,
		"model":  gjson.GetBytes(nativeBody, "model").Str,
		"usage": map[string]any{
			"prompt_tokens": totalTokens,
			"total_tokens":  totalTokens,
		},
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("voyage embed response: marshal canonical: %w", err)
	}

	// Stamp the model into nexus.ext.voyage.model for audit consumers.
	if model := gjson.GetBytes(nativeBody, "model").Str; model != "" {
		stamped, stampErr := canonicalext.Set(canonicalBytes, "voyage", "model", model)
		if stampErr == nil {
			canonicalBytes = stamped
		}
	}

	usage := provcore.ExtractUsage(canonicalBytes, provcore.FormatOpenAI)
	return provcore.DecodeResult{CanonicalBody: canonicalBytes, Usage: usage}, nil
}

// voyageInputCount returns the number of inputs in a Voyage wire request
// body — a string `input` counts as 1, an array counts its length. Returns
// 0 when the body is absent or `input` is missing (disables the count
// guard rather than rejecting). Mirrors the encode-side input handling.
func voyageInputCount(reqBody []byte) int {
	if len(reqBody) == 0 {
		return 0
	}
	in := gjson.GetBytes(reqBody, "input")
	switch {
	case in.IsArray():
		return len(in.Array())
	case in.Exists():
		return 1
	default:
		return 0
	}
}
