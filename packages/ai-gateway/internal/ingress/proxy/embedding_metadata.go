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
	"github.com/tidwall/gjson"
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

	// batch_size: 1 for single-string input, N for array input.
	batchSize := 1
	if in := gjson.GetBytes(reqBody, "input"); in.IsArray() {
		if n := int(in.Get("#").Int()); n > 0 {
			batchSize = n
		}
	}
	emb["batch_size"] = batchSize

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
// `data.0.embedding.#` is a gjson length query that returns the array
// length without allocating the full vector. If the response carries no
// embedding vector (empty data array or parse failure) we leave dimension
// absent and add a warning key so dashboards can surface the anomaly.
//
// Returns the updated metadata value (assign back to rec.Metadata).
func updateEmbeddingDimension(existing any, respBody []byte) any {
	md := mergeIntoMetadataMap(existing)
	emb := embeddingSubmap(md)

	dim := int(gjson.GetBytes(respBody, "data.0.embedding.#").Int())
	if dim > 0 {
		emb["dimension"] = dim
	} else {
		// Empty data array or parse failure — stamp a warning so
		// dashboards can surface the anomaly.
		emb["warning"] = "empty_data_array"
	}

	md["embedding"] = emb
	return md
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
