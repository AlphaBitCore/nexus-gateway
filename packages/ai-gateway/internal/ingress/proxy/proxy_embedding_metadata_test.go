// Package proxy — proxy_embedding_metadata_test.go covers the
// embedding metadata helper functions.
//
// Named failure modes:
//   - preStampEmbeddingRequestMeta: single-string input → batch_size=1
//   - preStampEmbeddingRequestMeta: array input → batch_size=N
//   - preStampEmbeddingRequestMeta: dimensions present → requested_dimension set
//   - preStampEmbeddingRequestMeta: dimensions absent → requested_dimension absent
//   - preStampEmbeddingRequestMeta: encoding_format present → stored
//   - preStampEmbeddingRequestMeta: encoding_format absent → "float" default
//   - preStampEmbeddingRequestMeta: cross_format_routing=true → stored
//   - preStampEmbeddingRequestMeta: existing metadata map preserved
//   - updateEmbeddingDimension: data array with vectors → dimension populated
//   - updateEmbeddingDimension: empty data array → warning key set
//   - updateEmbeddingDimension: nil body → warning key set
//   - updateEmbeddingDimension: preserves prior embedding sub-map fields
//   - mergeIntoMetadataMap: nil existing → empty map
//   - mergeIntoMetadataMap: non-map existing preserved under _prev
package proxy

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestPreStampEmbeddingRequestMeta_singleStringInput(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hello world"}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if bs, ok := emb["batch_size"].(int); !ok || bs != 1 {
		t.Errorf("batch_size = %v, want 1", emb["batch_size"])
	}
}

func TestPreStampEmbeddingRequestMeta_arrayInput(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":["foo","bar","baz"]}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if bs, ok := emb["batch_size"].(int); !ok || bs != 3 {
		t.Errorf("batch_size = %v, want 3", emb["batch_size"])
	}
}

func TestPreStampEmbeddingRequestMeta_dimensionsPresent(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hi","dimensions":512}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if rd, ok := emb["requested_dimension"].(int); !ok || rd != 512 {
		t.Errorf("requested_dimension = %v, want 512", emb["requested_dimension"])
	}
}

func TestPreStampEmbeddingRequestMeta_dimensionsAbsent(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if _, ok := emb["requested_dimension"]; ok {
		t.Errorf("requested_dimension should be absent when not in request, got %v", emb["requested_dimension"])
	}
}

func TestPreStampEmbeddingRequestMeta_encodingFormatPresent(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hi","encoding_format":"base64"}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if ef, ok := emb["encoding_format"].(string); !ok || ef != "base64" {
		t.Errorf("encoding_format = %v, want base64", emb["encoding_format"])
	}
}

func TestPreStampEmbeddingRequestMeta_encodingFormatDefault(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if ef, ok := emb["encoding_format"].(string); !ok || ef != "float" {
		t.Errorf("encoding_format = %v, want float (default)", emb["encoding_format"])
	}
}

func TestPreStampEmbeddingRequestMeta_crossFormatRoutingTrue(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	result := preStampEmbeddingRequestMeta(nil, body, true)

	emb := embeddingFromMeta(t, result)
	if cfr, ok := emb["cross_format_routing"].(bool); !ok || !cfr {
		t.Errorf("cross_format_routing = %v, want true", emb["cross_format_routing"])
	}
}

func TestPreStampEmbeddingRequestMeta_crossFormatRoutingFalse(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	result := preStampEmbeddingRequestMeta(nil, body, false)

	emb := embeddingFromMeta(t, result)
	if cfr, ok := emb["cross_format_routing"].(bool); !ok || cfr {
		t.Errorf("cross_format_routing = %v, want false", emb["cross_format_routing"])
	}
}

func TestPreStampEmbeddingRequestMeta_preservesExistingMetadata(t *testing.T) {
	existing := map[string]any{"custom_key": "custom_value"}
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	result := preStampEmbeddingRequestMeta(existing, body, false)

	md, ok := result.(map[string]any)
	if !ok {
		t.Fatal("result is not map[string]any")
	}
	if md["custom_key"] != "custom_value" {
		t.Errorf("existing key 'custom_key' was lost: %v", md)
	}
	if _, ok := md["embedding"]; !ok {
		t.Error("embedding sub-map not added")
	}
}

func TestPreStampEmbeddingRequestMeta_emptyBody(t *testing.T) {
	result := preStampEmbeddingRequestMeta(nil, nil, false)

	emb := embeddingFromMeta(t, result)
	if bs, ok := emb["batch_size"].(int); !ok || bs != 1 {
		t.Errorf("batch_size = %v, want 1 for empty body", emb["batch_size"])
	}
}

func TestUpdateEmbeddingDimension_vectorPresent(t *testing.T) {
	// Simulate canonical OpenAI embeddings response.
	respBody := buildEmbeddingResponse(1536)
	result := updateEmbeddingDimension(nil, respBody)

	emb := embeddingFromMeta(t, result)
	if dim, ok := emb["dimension"].(int); !ok || dim != 1536 {
		t.Errorf("dimension = %v, want 1536", emb["dimension"])
	}
	if _, ok := emb["warning"]; ok {
		t.Errorf("warning should be absent when dimension is found, got %v", emb["warning"])
	}
}

func TestUpdateEmbeddingDimension_emptyDataArray(t *testing.T) {
	respBody := []byte(`{"object":"list","data":[],"model":"text-embedding-3-small"}`)
	result := updateEmbeddingDimension(nil, respBody)

	emb := embeddingFromMeta(t, result)
	if _, ok := emb["dimension"]; ok {
		t.Errorf("dimension should be absent for empty data array, got %v", emb["dimension"])
	}
	if w, ok := emb["warning"].(string); !ok || w != "empty_data_array" {
		t.Errorf("warning = %v, want empty_data_array", emb["warning"])
	}
}

func TestUpdateEmbeddingDimension_nilBody(t *testing.T) {
	result := updateEmbeddingDimension(nil, nil)

	emb := embeddingFromMeta(t, result)
	if _, ok := emb["dimension"]; ok {
		t.Errorf("dimension should be absent for nil body")
	}
	if w, ok := emb["warning"].(string); !ok || w != "empty_data_array" {
		t.Errorf("warning = %v, want empty_data_array", emb["warning"])
	}
}

func TestUpdateEmbeddingDimension_preservesPriorSubmap(t *testing.T) {
	// Pre-stamp the request-side metadata first.
	reqBody := []byte(`{"model":"text-embedding-3-small","input":["a","b"],"dimensions":512}`)
	intermediate := preStampEmbeddingRequestMeta(nil, reqBody, true)

	// Now update with the response dimension.
	respBody := buildEmbeddingResponse(512)
	result := updateEmbeddingDimension(intermediate, respBody)

	emb := embeddingFromMeta(t, result)
	// Dimension from response.
	if dim, ok := emb["dimension"].(int); !ok || dim != 512 {
		t.Errorf("dimension = %v, want 512", emb["dimension"])
	}
	// Request-side fields preserved.
	if bs, ok := emb["batch_size"].(int); !ok || bs != 2 {
		t.Errorf("batch_size = %v, want 2 (preserved from preStamp)", emb["batch_size"])
	}
	if rd, ok := emb["requested_dimension"].(int); !ok || rd != 512 {
		t.Errorf("requested_dimension = %v, want 512 (preserved from preStamp)", emb["requested_dimension"])
	}
	if cfr, ok := emb["cross_format_routing"].(bool); !ok || !cfr {
		t.Errorf("cross_format_routing = %v, want true (preserved from preStamp)", emb["cross_format_routing"])
	}
}

func TestMergeIntoMetadataMap_nilReturnsEmptyMap(t *testing.T) {
	m := mergeIntoMetadataMap(nil)
	if m == nil {
		t.Error("expected non-nil empty map, got nil")
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestMergeIntoMetadataMap_mapPassedThrough(t *testing.T) {
	in := map[string]any{"k": "v"}
	out := mergeIntoMetadataMap(in)
	if out["k"] != "v" {
		t.Errorf("map not preserved: %v", out)
	}
}

func TestMergeIntoMetadataMap_nonMapPreservedUnderPrev(t *testing.T) {
	in := "some-string-value"
	out := mergeIntoMetadataMap(in)
	if out["_prev"] != "some-string-value" {
		t.Errorf("non-map value not preserved under _prev: %v", out)
	}
}

// embeddingFromMeta extracts the embedding sub-map from the metadata value.
func embeddingFromMeta(t *testing.T, meta any) map[string]any {
	t.Helper()
	md, ok := meta.(map[string]any)
	if !ok {
		t.Fatalf("metadata is %T, want map[string]any", meta)
	}
	emb, ok := md["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.embedding is %T, want map[string]any: %v", md["embedding"], md)
	}
	return emb
}

// buildEmbeddingResponse constructs a minimal OpenAI-shape embeddings
// response with one embedding of the given dimension.
func buildEmbeddingResponse(dim int) []byte {
	vec := make([]float64, dim)
	for i := range vec {
		vec[i] = float64(i) * 0.001
	}
	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{
				"object":    "embedding",
				"index":     0,
				"embedding": vec,
			},
		},
		"model": "text-embedding-3-small",
		"usage": map[string]any{"prompt_tokens": 10, "total_tokens": 10},
	}
	b, _ := json.Marshal(resp)
	return b
}
