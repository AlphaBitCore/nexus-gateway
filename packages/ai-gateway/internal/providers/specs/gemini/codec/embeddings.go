// Package codec — Gemini embedding request encoding and response decoding.
//
// Architecture references:
//   - docs/dev/architecture/provider-adapter-architecture.md §3a Rules 1-7
//   - docs/dev/architecture/endpoint-typology-architecture.md §2
//
// Gemini exposes two separate REST surfaces for embeddings:
//   - :embedContent   — single string input → {"embedding":{"values":[...]}}
//   - :batchEmbedContents — []string input → {"embeddings":[{"values":[...]},…]}
//
// The codec selects the correct endpoint by inspecting canonical "input":
// string → :embedContent, array → :batchEmbedContents. The selection is
// communicated to the specAdapter via EncodeResult.URLOverride (":embedContent"
// or ":batchEmbedContents") which the dispatch layer appends to the
// transport-supplied base URL.
//
// Gemini does NOT support token array inputs for embeddings. Token inputs
// result in a safety-net 400 from the upstream.
package codec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// encodeGeminiEmbeddingRequest translates a canonical OpenAI-shape embedding
// request into either a Gemini :embedContent or :batchEmbedContents wire body.
// The EncodeResult.URLOverride is set to ":embedContent" or ":batchEmbedContents"
// so the specAdapter's dispatch layer appends the correct action to the URL
// built by the Gemini transport.
func encodeGeminiEmbeddingRequest(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("gemini embed: empty canonical body")
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: invalid canonical JSON body",
		}
	}

	// -- taskType (default: RETRIEVAL_QUERY) --
	taskType := "RETRIEVAL_QUERY"
	if ext := canonicalext.Get(canonicalBody, "gemini", "taskType"); ext.Type == gjson.String && ext.Str != "" {
		taskType = ext.Str
	}

	// -- title --
	title := ""
	if ext := canonicalext.Get(canonicalBody, "gemini", "title"); ext.Type == gjson.String {
		title = ext.Str
	}

	// -- outputDimensionality from canonical dimensions --
	var outputDimensionality *int64
	if dim := gjson.GetBytes(canonicalBody, "dimensions"); dim.Exists() && dim.Type == gjson.Number {
		v := dim.Int()
		outputDimensionality = &v
	}

	// -- Dispatch on input shape --
	inputVal := gjson.GetBytes(canonicalBody, "input")
	if !inputVal.Exists() {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: canonical 'input' field is missing",
		}
	}

	switch {
	case inputVal.Type == gjson.String:
		// Single string → :embedContent
		return buildGeminiSingleEmbedRequest(inputVal.Str, taskType, title, outputDimensionality)

	case inputVal.IsArray():
		arr := inputVal.Array()
		if len(arr) == 0 {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "gemini embed: 'input' array is empty",
			}
		}
		first := arr[0]
		if first.Type == gjson.Number {
			// Token array — unsupported by Gemini embed endpoint.
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "gemini embed: token array inputs are not supported by the Gemini embedding API; use string inputs",
			}
		}
		if first.IsArray() {
			// Batch token array — also unsupported.
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "gemini embed: batch token array inputs are not supported by the Gemini embedding API",
			}
		}
		if first.Type != gjson.String {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "gemini embed: 'input' array elements must be strings",
			}
		}
		// Array of strings → :batchEmbedContents
		// Use single :embedContent path for single-element array to match
		// Gemini recommendation (single endpoint clearer than single-element batch).
		// SDD §T4.2 implementation note: "Use single endpoint for single-input
		// requests for clarity."
		if len(arr) == 1 {
			return buildGeminiSingleEmbedRequest(arr[0].Str, taskType, title, outputDimensionality)
		}
		texts := make([]string, 0, len(arr))
		for _, el := range arr {
			if el.Type != gjson.String {
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "gemini embed: mixed-type 'input' array",
				}
			}
			texts = append(texts, el.Str)
		}
		// Gemini :batchEmbedContents requires each sub-request to carry a
		// `model` field with the full resource path ("models/<id>"). The
		// transport builds the request URL with the bare provider model
		// id; the per-item `model` field is a *separate* requirement of
		// the batch API surface. Without it Google returns HTTP 400
		// `BatchEmbedContentsRequest.requests[i].model: model is not specified`.
		return buildGeminiBatchEmbedRequest(texts, target.ProviderModelID, taskType, title, outputDimensionality)

	default:
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: 'input' must be a string or array of strings",
		}
	}
}

// buildGeminiSingleEmbedRequest creates a :embedContent request body.
func buildGeminiSingleEmbedRequest(text, taskType, title string, outputDimensionality *int64) (provcore.EncodeResult, error) {
	req := map[string]any{
		"content": map[string]any{
			"parts": []map[string]any{
				{"text": text},
			},
		},
	}
	if taskType != "" {
		req["taskType"] = taskType
	}
	if title != "" {
		req["title"] = title
	}
	if outputDimensionality != nil {
		req["outputDimensionality"] = *outputDimensionality
	}
	body, err := json.Marshal(req)
	if err != nil {
		return provcore.EncodeResult{}, fmt.Errorf("gemini embed: marshal single request: %w", err)
	}
	return provcore.EncodeResult{
		Body:        body,
		ContentType: "application/json",
		URLOverride: ":embedContent",
	}, nil
}

// buildGeminiBatchEmbedRequest creates a :batchEmbedContents request body.
// modelID is the provider model id ("text-embedding-004", "gemini-embedding-001")
// — empty is rejected because Gemini batch requires each sub-request to
// carry a `model` field with the full resource path "models/<id>".
func buildGeminiBatchEmbedRequest(texts []string, modelID, taskType, title string, outputDimensionality *int64) (provcore.EncodeResult, error) {
	if modelID == "" {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusInternalServerError,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: missing ProviderModelID for batch embed (required by :batchEmbedContents per-item model field)",
		}
	}
	modelPath := modelID
	if !strings.HasPrefix(modelPath, "models/") {
		modelPath = "models/" + modelPath
	}
	requests := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		r := map[string]any{
			"model": modelPath,
			"content": map[string]any{
				"parts": []map[string]any{
					{"text": text},
				},
			},
		}
		if taskType != "" {
			r["taskType"] = taskType
		}
		if title != "" {
			r["title"] = title
		}
		if outputDimensionality != nil {
			r["outputDimensionality"] = *outputDimensionality
		}
		requests = append(requests, r)
	}
	body, err := json.Marshal(map[string]any{"requests": requests})
	if err != nil {
		return provcore.EncodeResult{}, fmt.Errorf("gemini embed: marshal batch request: %w", err)
	}
	return provcore.EncodeResult{
		Body:        body,
		ContentType: "application/json",
		URLOverride: ":batchEmbedContents",
	}, nil
}

// decodeGeminiEmbeddingResponse converts a Gemini embedding response
// (either :embedContent or :batchEmbedContents shape) into canonical
// OpenAI embeddings format.
//
// :embedContent response:   {"embedding":{"values":[…]}}
// :batchEmbedContents response: {"embeddings":[{"values":[…]},…]}
//
// reqBody is the Gemini wire request body (single :embedContent carries
// `content`, batch :batchEmbedContents carries `requests`[]). It is used to
// (1) assert the response vector count matches the request input count
// and (2) estimate prompt tokens from the request text when
// the Gemini embedding wire returns no usage. A zero reqBody
// disables both — they fail open.
func decodeGeminiEmbeddingResponse(nativeBody, reqBody []byte) (provcore.DecodeResult, error) {
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("gemini embed response: invalid JSON body")
	}

	var floatRows [][]float64

	// Detect single (:embedContent) vs batch (:batchEmbedContents) by
	// checking for top-level "embedding" (single) or "embeddings" (batch).
	singleEmbedding := gjson.GetBytes(nativeBody, "embedding")
	batchEmbeddings := gjson.GetBytes(nativeBody, "embeddings")

	switch {
	case singleEmbedding.Exists():
		// Single :embedContent response: {"embedding":{"values":[…]}}
		values := singleEmbedding.Get("values")
		if values.IsArray() {
			vec := make([]float64, 0, 256)
			values.ForEach(func(_, n gjson.Result) bool {
				vec = append(vec, n.Float())
				return true
			})
			floatRows = [][]float64{vec}
		}
	case batchEmbeddings.IsArray():
		// Batch :batchEmbedContents response: {"embeddings":[{"values":[…]},…]}
		batchEmbeddings.ForEach(func(_, item gjson.Result) bool {
			values := item.Get("values")
			if values.IsArray() {
				vec := make([]float64, 0, 256)
				values.ForEach(func(_, n gjson.Result) bool {
					vec = append(vec, n.Float())
					return true
				})
				floatRows = append(floatRows, vec)
			}
			return true
		})
	}

	// Guard against a provider silently dropping or reordering items: the
	// canonical data[] is re-indexed by upstream position, so a count
	// mismatch means the vectors no longer align with the request inputs.
	// Fail the decode (→ 502) rather than serve misaligned
	// vectors. expected = request inputs (single `content`→1, batch
	// `requests`→len).
	if err := specutil.ValidateEmbeddingRowCount(geminiEmbedInputCount(reqBody), len(floatRows)); err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("gemini embed response: %w", err)
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

	// Extract usage. Gemini's embedding API surfaces tokens under different
	// paths depending on the variant:
	//   - generateContent shape (chat): usageMetadata.totalTokenCount
	//   - :embedContent (v1beta): historically no usage; recent variants
	//     include `billable_character_count` (chars not tokens, scoped per
	//     embedding) under the top-level response.
	//   - :batchEmbedContents (v1beta): each sub-item carries
	//     `embedding.statistics.token_count`; some variants also expose
	//     `metadata.billable_character_count`. The top-level
	//     `embedding.statistics.token_count` (single) or summed across
	//     batch items is the closest analog to OpenAI prompt_tokens.
	//   - googleapis newer surfaces also emit usageMetadata.promptTokenCount.
	//
	// We try each path in fall-through order and sum across batch items so
	// the gateway emits a usable cost-accounting figure for every variant.
	var promptTokens int64
	if um := gjson.GetBytes(nativeBody, "usageMetadata.totalTokenCount"); um.Exists() {
		promptTokens = um.Int()
	} else if um := gjson.GetBytes(nativeBody, "usageMetadata.promptTokenCount"); um.Exists() {
		promptTokens = um.Int()
	} else if t := gjson.GetBytes(nativeBody, "embedding.statistics.token_count"); t.Exists() {
		promptTokens = t.Int()
	} else if arr := gjson.GetBytes(nativeBody, "embeddings"); arr.IsArray() {
		// Batch: sum per-item statistics.token_count (or fall back to billable counts).
		arr.ForEach(func(_, item gjson.Result) bool {
			if t := item.Get("statistics.token_count"); t.Exists() {
				promptTokens += t.Int()
			} else if t := item.Get("metadata.billable_character_count"); t.Exists() {
				// Approximate tokens as chars/4 — the canonical heuristic
				// when token counts aren't surfaced. Better than 0 for
				// cost accounting; admins can tune the multiplier later.
				promptTokens += t.Int() / 4
			}
			return true
		})
	} else if bc := gjson.GetBytes(nativeBody, "billable_character_count"); bc.Exists() {
		promptTokens = bc.Int() / 4
	}

	// The Gemini/Vertex embedding wire frequently returns no token counts at
	// all. Recover a usable prompt-token figure by estimating from the
	// request text (chars/4 heuristic) so per-call cost accounting is not
	// silently zeroed (formerly a Gemini-format branch in the
	// generic dispatcher, now owned by the codec that holds the request).
	if promptTokens == 0 {
		if est := geminiEmbedEstimatedPromptTokens(reqBody); est > 0 {
			promptTokens = est
		}
	}

	canonical, _ := sjson.SetBytes([]byte(`{}`), "object", "list")
	canonical, _ = sjson.SetBytes(canonical, "data", data)
	// The Gemini embedding wire response carries no model field, and the
	// stateless SchemaCodec.DecodeResponse interface does not receive the
	// CallTarget, so the model cannot be stamped here. It is back-filled
	// from req.Target.ProviderModelID by the dispatcher's generic
	// embeddings model-stamp (spec_adapter.go) so OpenAI SDK callers see
	// the model they requested instead of an empty string.
	canonical, _ = sjson.SetBytes(canonical, "usage", map[string]any{
		"prompt_tokens": promptTokens,
		"total_tokens":  promptTokens,
	})

	// Extract usage via shared normalizer path (consistent with other codecs).
	usage := provcore.ExtractUsage(canonical, provcore.FormatOpenAI)

	return provcore.DecodeResult{CanonicalBody: canonical, Usage: usage}, nil
}

// geminiEmbedInputCount returns the number of inputs in a Gemini embedding
// wire request: a single :embedContent body carries `content` (count 1); a
// batch :batchEmbedContents body carries `requests`[] (count len). Returns
// 0 when the body is absent or neither field is present (disables the count
// guard rather than rejecting).
func geminiEmbedInputCount(reqBody []byte) int {
	if len(reqBody) == 0 {
		return 0
	}
	if reqs := gjson.GetBytes(reqBody, "requests"); reqs.IsArray() {
		return len(reqs.Array())
	}
	if gjson.GetBytes(reqBody, "content").Exists() {
		return 1
	}
	return 0
}

// geminiEmbedEstimatedPromptTokens estimates prompt tokens from a Gemini
// embedding wire request body using the chars/4 heuristic over every
// `content.parts[].text` (single) and `requests[].content.parts[].text`
// (batch). Returns 0 when no text is present (caller keeps the upstream 0).
func geminiEmbedEstimatedPromptTokens(reqBody []byte) int64 {
	if len(reqBody) == 0 {
		return 0
	}
	var charCount int
	gjson.GetBytes(reqBody, "content.parts").ForEach(func(_, p gjson.Result) bool {
		charCount += len(p.Get("text").String())
		return true
	})
	gjson.GetBytes(reqBody, "requests").ForEach(func(_, r gjson.Result) bool {
		r.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			charCount += len(p.Get("text").String())
			return true
		})
		return true
	})
	if charCount == 0 {
		return 0
	}
	est := int64(charCount / 4)
	if est < 1 {
		est = 1
	}
	return est
}
