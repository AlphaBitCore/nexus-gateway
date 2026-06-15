// Package proxy — embedding_metadata.go implements the helpers that
// extract and stamp embedding-specific audit metadata.
//
// The metadata.embedding.* JSONB keys stamped here are:
//
//	dimension              (int)     — response: actual vector length
//	requested_dimension    (*int)    — request: client's `dimensions` param
//	batch_size             (int)     — request: number of input items
//	encoding_format        (string)  — request: "float" | "base64"
//	cross_format_routing   (bool)    — derived: ingress format ≠ target adapter
//
// Stamping is split into two phases to avoid threading the original
// request body through all five handler call sites:
//
//  1. preStampEmbeddingRequestMeta — called in ServeProxy after routing
//     (where both the request body and routing target are available).
//     Sets all request-side fields and cross_format_routing.
//
//  2. updateEmbeddingDimension — called in each of the five response
//     paths (handleNonStream, handleNonStreamHit, broker non-stream
//     HIT_LIVE). Updates metadata.embedding.dimension from the canonical
//     response body `data[0].embedding.#`. Stream HIT paths skip this
//     update because embedding responses never stream in practice.
package proxy

import (
	"encoding/base64"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
)

// preStampEmbeddingRequestMeta merges the request-side embedding metadata
// into the existing metadata map and returns the updated value. The
// returned value should be assigned back to rec.Metadata.
//
// Fields set here (never overwritten by the response-side update):
//   - embedding.requested_dimension  (int | absent)
//   - embedding.batch_size           (int, ≥ 1)
//   - embedding.encoding_format      (string, default "float")
//   - embedding.cross_format_routing (bool)
//
// The response-side field embedding.dimension is intentionally left absent
// here; updateEmbeddingDimension fills it when the response arrives.
func preStampEmbeddingRequestMeta(existing any, reqBody []byte, crossFormatRouting bool) any {
	md := mergeIntoMetadataMap(existing)
	emb := embeddingSubmap(md)

	// requested_dimension: nil when the client omitted `dimensions`.
	if d := gjson.GetBytes(reqBody, "dimensions"); d.Exists() {
		emb["requested_dimension"] = int(d.Int())
	}

	// batch_size: 1 for single input, N for a batch of N inputs.
	emb["batch_size"] = embeddingBatchSize(gjson.GetBytes(reqBody, "input"))

	// estimated_prompt_tokens: a local heuristic token count over the input
	// text(s). Used as the cost/usage fallback when the upstream embeddings
	// response does not report token usage (e.g. Gemini embedContent returns
	// only the vector, no usageMetadata). OpenAI/Azure embeddings report real
	// usage, so this estimate is only consumed when usage is absent.
	estTokens := 0
	if in := gjson.GetBytes(reqBody, "input"); in.Exists() {
		if in.IsArray() {
			in.ForEach(func(_, v gjson.Result) bool {
				estTokens += inputstaging.EstimateTokens(v.String())
				return true
			})
		} else {
			estTokens = inputstaging.EstimateTokens(in.String())
		}
	}
	emb["estimated_prompt_tokens"] = estTokens

	// encoding_format: default "float" when absent.
	ef := gjson.GetBytes(reqBody, "encoding_format").String()
	if ef == "" {
		ef = "float"
	}
	emb["encoding_format"] = ef

	// cross_format_routing: true when ingress wire format ≠ target adapter.
	emb["cross_format_routing"] = crossFormatRouting

	md["embedding"] = emb
	return md
}

// updateEmbeddingDimension reads the actual vector length from the
// canonical OpenAI-shape response body and stamps it into
// metadata.embedding.dimension. The canonical response has the form:
//
//	{"data":[{"embedding":[…], "object":"embedding", "index":0}], …}
//
// The vector may arrive in two shapes depending on encoding_format:
//   - "float" (default): `data[0].embedding` is a JSON array; the length
//     comes from the `data.0.embedding.#` gjson count query (no full-vector
//     allocation).
//   - "base64": `data[0].embedding` is a base64 STRING of packed float32s
//     (OpenAI/Azure). `.#` returns 0 for a string, so the length is decoded
//     from the base64 byte count (4 bytes per float32). Without this the
//     valid base64 path was mis-stamped with warning="empty_data_array".
//
// If the response carries no usable vector (genuinely empty data array,
// undecodable base64, or parse failure) we leave dimension absent and add a
// warning key so dashboards can surface the anomaly.
//
// Returns the updated metadata value (assign back to rec.Metadata).
func updateEmbeddingDimension(existing any, respBody []byte) any {
	md := mergeIntoMetadataMap(existing)
	emb := embeddingSubmap(md)

	if dim := embeddingResponseDimension(respBody); dim > 0 {
		emb["dimension"] = dim
	} else {
		// Empty data array, undecodable base64, or parse failure — stamp a
		// warning so dashboards can surface the anomaly.
		emb["warning"] = "empty_data_array"
	}

	md["embedding"] = emb
	return md
}

// embeddingResponseDimension returns the vector length of the first
// embedding in a canonical OpenAI-shape embeddings response, handling both
// the float-array and base64-string encodings. Returns 0 when no usable
// vector is present.
func embeddingResponseDimension(respBody []byte) int {
	v := gjson.GetBytes(respBody, "data.0.embedding")
	switch {
	case v.IsArray():
		return len(v.Array())
	case v.Type == gjson.String && v.Str != "":
		// base64-packed float32 vector: 4 bytes per component. OpenAI emits
		// padded standard base64; tolerate the unpadded raw variant too.
		raw, err := base64.StdEncoding.DecodeString(v.Str)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(v.Str)
		}
		if err != nil || len(raw) < 4 {
			return 0
		}
		return len(raw) / 4
	default:
		return 0
	}
}

// embeddingBatchSize returns the number of distinct inputs in an OpenAI-shape
// embeddings `input` field. The wire shape is overloaded:
//
//	"text"               → 1   (single string)
//	["a","b"]            → 2   (batch of strings)
//	[1,2,3]              → 1   (ONE token-id sequence — NOT a batch of 3)
//	[[1,2],[3,4]]        → 2   (batch of token-id sequences)
//
// A top-level array whose first element is a number is therefore a single
// token-id sequence and must count as batch=1, otherwise it is false-rejected
// against a small MaxBatchSize and mis-stamped in the audit row.
func embeddingBatchSize(in gjson.Result) int {
	if !in.IsArray() {
		return 1
	}
	arr := in.Array()
	if len(arr) == 0 {
		return 1
	}
	if arr[0].Type == gjson.Number {
		return 1
	}
	return len(arr)
}

// mergeIntoMetadataMap coerces an arbitrary existing metadata value into
// a map[string]any so both preStamp and updateDimension can write into it
// without discarding prior keys set by hook pipelines or other stampers.
func mergeIntoMetadataMap(existing any) map[string]any {
	if existing == nil {
		return map[string]any{}
	}
	if m, ok := existing.(map[string]any); ok {
		return m
	}
	// Existing metadata is a non-map type (rare: set by test or a future
	// non-map stamper). Preserve it under a "_prev" key so we don't lose
	// its value, then wrap in a fresh map.
	return map[string]any{"_prev": existing}
}

// embeddingSubmap returns the existing embedding sub-map from md, or a
// fresh empty map. The caller is responsible for writing the result back
// into md["embedding"].
func embeddingSubmap(md map[string]any) map[string]any {
	if sub, ok := md["embedding"].(map[string]any); ok {
		return sub
	}
	return map[string]any{}
}

// embeddingTokenFallback returns the prompt-token count to bill for an
// embeddings response. When the endpoint is embeddings and the upstream
// reported no usage (currentPromptTokens == 0, e.g. Gemini embedContent which
// returns only the vector), it substitutes the request-side local estimate so
// the cost formula yields a non-zero embedding cost. Otherwise it returns
// currentPromptTokens unchanged. Shared by every non-stream cost site so the
// behaviour is identical on the live and broker-subscription paths.
func embeddingTokenFallback(endpointType string, currentPromptTokens int64, metadata any) int64 {
	if endpointType != "embeddings" || currentPromptTokens != 0 {
		return currentPromptTokens
	}
	if est := embeddingEstimatedPromptTokens(metadata); est > 0 {
		return int64(est)
	}
	return currentPromptTokens
}

// embeddingEstimatedPromptTokens reads the request-side local token estimate
// stamped by preStampEmbeddingRequestMeta. Returns 0 when absent. Used by the
// non-stream response path to back-fill prompt_tokens + cost when the upstream
// embeddings response reports no usage.
func embeddingEstimatedPromptTokens(existing any) int {
	md, ok := existing.(map[string]any)
	if !ok {
		return 0
	}
	emb, ok := md["embedding"].(map[string]any)
	if !ok {
		return 0
	}
	switch v := emb["estimated_prompt_tokens"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
