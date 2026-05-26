package bedrock

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// isTitanEmbedModel / isCohereEmbedModel dispatch predicates.

func TestIsTitanEmbedModel(t *testing.T) {
	cases := []struct {
		modelID string
		want    bool
	}{
		{"amazon.titan-embed-text-v2:0", true},
		{"amazon.titan-embed-text-v1", true},
		{"amazon.titan-embed-image-v1", true},
		{"AMAZON.TITAN-EMBED-TEXT-V2:0", true}, // uppercase
		{"cohere.embed-english-v3", false},
		{"anthropic.claude-3-sonnet", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isTitanEmbedModel(tc.modelID)
		if got != tc.want {
			t.Errorf("isTitanEmbedModel(%q) = %v, want %v", tc.modelID, got, tc.want)
		}
	}
}

func TestIsCohereEmbedModel(t *testing.T) {
	cases := []struct {
		modelID string
		want    bool
	}{
		{"cohere.embed-english-v3", true},
		{"cohere.embed-multilingual-v3", true},
		{"COHERE.EMBED-ENGLISH-V3", true}, // uppercase
		{"amazon.titan-embed-text-v2:0", false},
		{"cohere.command-r-plus", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isCohereEmbedModel(tc.modelID)
		if got != tc.want {
			t.Errorf("isCohereEmbedModel(%q) = %v, want %v", tc.modelID, got, tc.want)
		}
	}
}

// embeddings dispatch (embeddingEncodeRequest).

func TestEmbeddingEncodeRequest_TitanDispatch(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"hello world"}`)
	res, err := embeddingEncodeRequest(body, provcore.CallTarget{
		ProviderModelID: "amazon.titan-embed-text-v2:0",
	})
	if err != nil {
		t.Fatalf("titan dispatch: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("wire body invalid JSON: %v", e)
	}
	if out["inputText"] != "hello world" {
		t.Errorf("inputText=%v want hello world", out["inputText"])
	}
}

func TestEmbeddingEncodeRequest_CohereDispatch(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":["search text"]}`)
	res, err := embeddingEncodeRequest(body, provcore.CallTarget{
		ProviderModelID: "cohere.embed-english-v3",
	})
	if err != nil {
		t.Fatalf("cohere dispatch: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("wire body invalid JSON: %v", e)
	}
	texts, ok := out["texts"].([]any)
	if !ok || len(texts) != 1 || texts[0] != "search text" {
		t.Errorf("texts=%v want [search text]", out["texts"])
	}
}

func TestEmbeddingEncodeRequest_UnknownModel(t *testing.T) {
	body := []byte(`{"input":"hello"}`)
	_, err := embeddingEncodeRequest(body, provcore.CallTarget{
		ProviderModelID: "unknown.model-xyz",
	})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest {
		t.Fatalf("want 400 ProviderError, got %v", err)
	}
}

func TestEmbeddingEncodeRequest_FallsBackToBodyModel(t *testing.T) {
	// Empty ProviderModelID → fall back to body model for dispatch.
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"hello"}`)
	res, err := embeddingEncodeRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("fallback to body model: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["inputText"] != "hello" {
		t.Errorf("inputText=%v want hello", out["inputText"])
	}
}

// Titan codec — encodeTitanEmbedRequest.

func TestTitan_StringInput(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"test text"}`)
	res, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("titan encode: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["inputText"] != "test text" {
		t.Errorf("inputText=%v want test text", out["inputText"])
	}
	if res.ContentType != "application/json" {
		t.Errorf("ContentType=%q", res.ContentType)
	}
}

func TestTitan_SingleElementArray(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":["single"]}`)
	res, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("titan encode single array: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["inputText"] != "single" {
		t.Errorf("inputText=%v want single", out["inputText"])
	}
}

func TestTitan_MultiElementArrayRejected(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":["a","b"]}`)
	_, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "batch_unsupported") {
		t.Fatalf("expected batch_unsupported error, got %v", err)
	}
}

func TestTitan_EmptyArrayRejected(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":[]}`)
	_, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("empty array must be rejected")
	}
}

func TestTitan_TokenArrayRejected(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":[100,200]}`)
	_, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "token_array_unsupported") {
		t.Fatalf("expected token_array_unsupported error, got %v", err)
	}
}

func TestTitan_InvalidInputType(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":42}`)
	_, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("numeric input must be rejected")
	}
}

func TestTitan_MissingInput(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0"}`)
	_, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing-input error, got %v", err)
	}
}

func TestTitan_InvalidJSON(t *testing.T) {
	_, err := encodeTitanEmbedRequest([]byte(`not-json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected invalid-JSON error")
	}
}

func TestTitan_DimensionsFromCanonical(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"x","dimensions":512}`)
	res, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("titan encode with dimensions: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if v, ok := out["dimensions"].(float64); !ok || v != 512 {
		t.Errorf("dimensions=%v want 512", out["dimensions"])
	}
}

func TestTitan_DimensionsFromExtension(t *testing.T) {
	// dimensions from nexus.ext.bedrock.titan_dimensions (no canonical dimensions).
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"x","nexus":{"ext":{"bedrock":{"titan_dimensions":256}}}}`)
	res, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("titan encode with ext dimensions: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if v, ok := out["dimensions"].(float64); !ok || v != 256 {
		t.Errorf("ext dimensions=%v want 256", out["dimensions"])
	}
}

func TestTitan_NormalizeExtension(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"x","nexus":{"ext":{"bedrock":{"titan_normalize":true}}}}`)
	res, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("titan encode with normalize: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["normalize"] != true {
		t.Errorf("normalize=%v want true", out["normalize"])
	}
}

func TestTitan_EmbeddingTypesExtension(t *testing.T) {
	body := []byte(`{"model":"amazon.titan-embed-text-v2:0","input":"x","nexus":{"ext":{"bedrock":{"titan_embedding_types":["float","int8"]}}}}`)
	res, err := encodeTitanEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("titan encode with embeddingTypes: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	et, ok := out["embeddingTypes"].([]any)
	if !ok || len(et) != 2 {
		t.Errorf("embeddingTypes=%v want [float int8]", out["embeddingTypes"])
	}
}

// Titan codec — decodeTitanEmbedResponse.

func TestTitan_DecodeResponse_HappyPath(t *testing.T) {
	native := []byte(`{"embedding":[0.1,0.2,0.3],"inputTextTokenCount":5}`)
	res, err := decodeTitanEmbedResponse(native, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("titan decode: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	if out["object"] != "list" {
		t.Errorf("object=%v", out["object"])
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len=%d want 1", len(data))
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 5 {
		t.Errorf("usage.prompt_tokens=%v want 5", usage["prompt_tokens"])
	}
}

func TestTitan_DecodeResponse_InvalidJSON(t *testing.T) {
	_, err := decodeTitanEmbedResponse([]byte(`not-json`), "amazon.titan-embed-text-v2:0")
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

func TestTitan_DecodeResponse_EmptyEmbedding(t *testing.T) {
	native := []byte(`{"inputTextTokenCount":2}`)
	res, err := decodeTitanEmbedResponse(native, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("empty embedding: %v", err)
	}
	data, _ := json.Marshal(res.CanonicalBody)
	_ = data
	// Should still return a valid canonical body (empty embedding array).
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
}

func TestTitan_DecodeResponse_ModelStampedInExt(t *testing.T) {
	native := []byte(`{"embedding":[0.5],"inputTextTokenCount":1}`)
	res, err := decodeTitanEmbedResponse(native, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("titan decode: %v", err)
	}
	if !strings.Contains(string(res.CanonicalBody), "amazon.titan-embed-text-v2:0") {
		t.Errorf("model not stamped in canonical: %s", res.CanonicalBody)
	}
}

// Cohere codec — encodeCohereEmbedRequest.

func TestCohere_StringInput(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":"search text"}`)
	res, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("cohere encode: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	texts, ok := out["texts"].([]any)
	if !ok || len(texts) != 1 || texts[0] != "search text" {
		t.Errorf("texts=%v want [search text]", out["texts"])
	}
	// Default input_type should be "search_document".
	if out["input_type"] != "search_document" {
		t.Errorf("input_type=%v want search_document (default)", out["input_type"])
	}
}

func TestCohere_ArrayInput(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":["a","b","c"]}`)
	res, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("cohere array: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	texts, _ := out["texts"].([]any)
	if len(texts) != 3 {
		t.Errorf("texts len=%d want 3", len(texts))
	}
}

func TestCohere_EmptyArrayRejected(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":[]}`)
	_, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("empty array must be rejected")
	}
}

func TestCohere_TokenArrayRejected(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":[100,200]}`)
	_, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "token_array_unsupported") {
		t.Fatalf("expected token_array_unsupported, got %v", err)
	}
}

func TestCohere_MixedTypeArrayRejected(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":["text",123]}`)
	_, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("mixed-type array must be rejected")
	}
}

func TestCohere_InvalidInputType(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":42}`)
	_, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("numeric input must be rejected")
	}
}

func TestCohere_MissingInput(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3"}`)
	_, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing-input error, got %v", err)
	}
}

func TestCohere_InvalidJSON(t *testing.T) {
	_, err := encodeCohereEmbedRequest([]byte(`not-json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected invalid-JSON error")
	}
}

func TestCohere_InputTypeFromExtension(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":["query"],"nexus":{"ext":{"bedrock":{"cohere_input_type":"search_query"}}}}`)
	res, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("cohere with input_type ext: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["input_type"] != "search_query" {
		t.Errorf("input_type=%v want search_query", out["input_type"])
	}
}

func TestCohere_TruncateFromExtension(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":["x"],"nexus":{"ext":{"bedrock":{"cohere_truncate":"END"}}}}`)
	res, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("cohere with truncate: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if out["truncate"] != "END" {
		t.Errorf("truncate=%v want END", out["truncate"])
	}
}

func TestCohere_EmbeddingTypesFromExtension(t *testing.T) {
	body := []byte(`{"model":"cohere.embed-english-v3","input":["x"],"nexus":{"ext":{"bedrock":{"cohere_embedding_types":["float"]}}}}`)
	res, err := encodeCohereEmbedRequest(body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("cohere with embedding_types: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.Body, &out); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	et, ok := out["embedding_types"].([]any)
	if !ok || len(et) != 1 || et[0] != "float" {
		t.Errorf("embedding_types=%v want [float]", out["embedding_types"])
	}
}

// Cohere codec — decodeCohereEmbedResponse.

func TestCohere_DecodeResponse_HappyPath(t *testing.T) {
	native := []byte(`{
		"embeddings":[[0.1,0.2,0.3],[0.4,0.5,0.6]],
		"id":"emb-123",
		"response_type":"embeddings_floats",
		"texts":["a","b"]
	}`)
	res, err := decodeCohereEmbedResponse(native, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("cohere decode: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	if out["object"] != "list" {
		t.Errorf("object=%v", out["object"])
	}
	data, _ := out["data"].([]any)
	if len(data) != 2 {
		t.Errorf("data len=%d want 2", len(data))
	}
}

func TestCohere_DecodeResponse_EmptyEmbeddings(t *testing.T) {
	native := []byte(`{"embeddings":[],"id":"emb-x","response_type":"embeddings_floats"}`)
	res, err := decodeCohereEmbedResponse(native, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("cohere decode empty: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	data, _ := out["data"].([]any)
	if len(data) != 0 {
		t.Errorf("data len=%d want 0", len(data))
	}
}

func TestCohere_DecodeResponse_InvalidJSON(t *testing.T) {
	_, err := decodeCohereEmbedResponse([]byte(`not-json`), "cohere.embed-english-v3")
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

func TestCohere_DecodeResponse_ModelStampedInExt(t *testing.T) {
	native := []byte(`{"embeddings":[[0.5]],"id":"x","response_type":"embeddings_floats"}`)
	res, err := decodeCohereEmbedResponse(native, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("cohere decode: %v", err)
	}
	if !strings.Contains(string(res.CanonicalBody), "cohere.embed-english-v3") {
		t.Errorf("model not stamped in canonical: %s", res.CanonicalBody)
	}
}

// decodeBedrockEmbedResponseByShape (shape-based dispatch).

func TestDecodeByShape_TitanShape(t *testing.T) {
	native := []byte(`{"embedding":[0.1,0.2],"inputTextTokenCount":3}`)
	res, err := decodeBedrockEmbedResponseByShape(native)
	if err != nil {
		t.Fatalf("shape-decode titan: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 3 {
		t.Errorf("prompt_tokens=%v want 3", usage["prompt_tokens"])
	}
}

func TestDecodeByShape_CohereShape(t *testing.T) {
	native := []byte(`{"embeddings":[[0.1,0.2]],"id":"x","response_type":"embeddings_floats"}`)
	res, err := decodeBedrockEmbedResponseByShape(native)
	if err != nil {
		t.Fatalf("shape-decode cohere: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("data len=%d want 1", len(data))
	}
}

func TestDecodeByShape_FallbackShape(t *testing.T) {
	// Body has neither "embedding" nor "embeddings" → fallback decoder.
	native := []byte(`{"some_field":"value"}`)
	res, err := decodeBedrockEmbedResponseByShape(native)
	if err != nil {
		t.Fatalf("shape-decode fallback: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
}

func TestDecodeByShape_InvalidJSON(t *testing.T) {
	_, err := decodeBedrockEmbedResponseByShape([]byte(`not-json`))
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

// EmbedRequestToCanonical — public cross-format API.

func TestEmbedRequestToCanonical_Titan(t *testing.T) {
	body := []byte(`{"inputText":"hello titan","dimensions":512}`)
	canonical, err := EmbedRequestToCanonical(body, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical titan: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(canonical, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	if out["input"] != "hello titan" {
		t.Errorf("input=%v want hello titan", out["input"])
	}
	if v, ok := out["dimensions"].(float64); !ok || v != 512 {
		t.Errorf("dimensions=%v want 512", out["dimensions"])
	}
}

func TestEmbedRequestToCanonical_Cohere(t *testing.T) {
	body := []byte(`{"texts":["search query"],"input_type":"search_query"}`)
	canonical, err := EmbedRequestToCanonical(body, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("EmbedRequestToCanonical cohere: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(canonical, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	arr, ok := out["input"].([]any)
	if !ok || len(arr) != 1 || arr[0] != "search query" {
		t.Errorf("input=%v want [search query]", out["input"])
	}
}

func TestEmbedRequestToCanonical_InvalidJSON(t *testing.T) {
	_, err := EmbedRequestToCanonical([]byte(`not-json`), "amazon.titan-embed-text-v2:0")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestEmbedRequestToCanonical_UnknownModel(t *testing.T) {
	body := []byte(`{"inputText":"x"}`)
	_, err := EmbedRequestToCanonical(body, "unknown.model")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestEmbedRequestToCanonical_Titan_MissingInputText(t *testing.T) {
	body := []byte(`{"dimensions":512}`)
	_, err := EmbedRequestToCanonical(body, "amazon.titan-embed-text-v2:0")
	if err == nil || !strings.Contains(err.Error(), "missing 'inputText'") {
		t.Fatalf("expected missing-inputText error, got %v", err)
	}
}

func TestEmbedRequestToCanonical_Cohere_MissingTexts(t *testing.T) {
	body := []byte(`{"input_type":"search_query"}`)
	_, err := EmbedRequestToCanonical(body, "cohere.embed-english-v3")
	if err == nil || !strings.Contains(err.Error(), "missing 'texts'") {
		t.Fatalf("expected missing-texts error, got %v", err)
	}
}

func TestEmbedRequestToCanonical_Cohere_EmptyTexts(t *testing.T) {
	body := []byte(`{"texts":[]}`)
	_, err := EmbedRequestToCanonical(body, "cohere.embed-english-v3")
	if err == nil {
		t.Fatal("expected error for empty texts")
	}
}

func TestEmbedRequestToCanonical_Cohere_NonStringElement(t *testing.T) {
	body := []byte(`{"texts":[1,2,3]}`)
	_, err := EmbedRequestToCanonical(body, "cohere.embed-english-v3")
	if err == nil {
		t.Fatal("expected error for non-string elements")
	}
}

func TestEmbedRequestToCanonical_Titan_NormalizeExtension(t *testing.T) {
	body := []byte(`{"inputText":"x","normalize":true}`)
	canonical, err := EmbedRequestToCanonical(body, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("titan with normalize: %v", err)
	}
	if !strings.Contains(string(canonical), "titan_normalize") {
		t.Errorf("titan_normalize not in canonical: %s", canonical)
	}
}

func TestEmbedRequestToCanonical_Titan_EmbeddingTypesExtension(t *testing.T) {
	body := []byte(`{"inputText":"x","embeddingTypes":["float"]}`)
	canonical, err := EmbedRequestToCanonical(body, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("titan with embeddingTypes: %v", err)
	}
	if !strings.Contains(string(canonical), "titan_embedding_types") {
		t.Errorf("titan_embedding_types not in canonical: %s", canonical)
	}
}

func TestEmbedRequestToCanonical_Cohere_TruncateExtension(t *testing.T) {
	body := []byte(`{"texts":["x"],"input_type":"search_query","truncate":"NONE"}`)
	canonical, err := EmbedRequestToCanonical(body, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("cohere with truncate: %v", err)
	}
	if !strings.Contains(string(canonical), "cohere_truncate") {
		t.Errorf("cohere_truncate not in canonical: %s", canonical)
	}
}

func TestEmbedRequestToCanonical_Cohere_EmbeddingTypesExtension(t *testing.T) {
	body := []byte(`{"texts":["x"],"embedding_types":["float"]}`)
	canonical, err := EmbedRequestToCanonical(body, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("cohere with embedding_types: %v", err)
	}
	if !strings.Contains(string(canonical), "cohere_embedding_types") {
		t.Errorf("cohere_embedding_types not in canonical: %s", canonical)
	}
}

func TestCanonicalToBedrockEmbedResponse_Titan(t *testing.T) {
	canonical := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}],
		"model":"amazon.titan-embed-text-v2:0",
		"usage":{"prompt_tokens":4,"total_tokens":4}
	}`)
	out, err := CanonicalToBedrockEmbedResponse(canonical, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("canonical→titan: %v", err)
	}
	var parsed map[string]any
	if e := json.Unmarshal(out, &parsed); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if _, ok := parsed["embedding"]; !ok {
		t.Errorf("Titan response must have 'embedding' field: %s", out)
	}
	if parsed["inputTextTokenCount"].(float64) != 4 {
		t.Errorf("inputTextTokenCount=%v want 4", parsed["inputTextTokenCount"])
	}
}

func TestCanonicalToBedrockEmbedResponse_Cohere(t *testing.T) {
	canonical := []byte(`{
		"object":"list",
		"data":[
			{"object":"embedding","embedding":[0.1,0.2],"index":0},
			{"object":"embedding","embedding":[0.3,0.4],"index":1}
		],
		"model":"cohere.embed-english-v3",
		"usage":{"prompt_tokens":0,"total_tokens":0}
	}`)
	out, err := CanonicalToBedrockEmbedResponse(canonical, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("canonical→cohere: %v", err)
	}
	var parsed map[string]any
	if e := json.Unmarshal(out, &parsed); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	embeddings, ok := parsed["embeddings"].([]any)
	if !ok || len(embeddings) != 2 {
		t.Errorf("Cohere response embeddings=%v want 2 items", parsed["embeddings"])
	}
}

func TestCanonicalToBedrockEmbedResponse_Unknown_FallsBackToTitan(t *testing.T) {
	// Unknown model falls back to Titan-like response shape.
	canonical := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.5],"index":0}],
		"model":"x",
		"usage":{"prompt_tokens":1,"total_tokens":1}
	}`)
	out, err := CanonicalToBedrockEmbedResponse(canonical, "unknown.model")
	if err != nil {
		t.Fatalf("unknown model fallback: %v", err)
	}
	var parsed map[string]any
	if e := json.Unmarshal(out, &parsed); e != nil {
		t.Fatalf("invalid JSON: %v", e)
	}
	if _, ok := parsed["embedding"]; !ok {
		t.Errorf("fallback response must have 'embedding' field: %s", out)
	}
}

func TestCanonicalToBedrockEmbedResponse_InvalidJSON(t *testing.T) {
	_, err := CanonicalToBedrockEmbedResponse([]byte(`not-json`), "amazon.titan-embed-text-v2:0")
	if err == nil || !strings.Contains(err.Error(), "invalid canonical body") {
		t.Fatalf("expected invalid-canonical error, got %v", err)
	}
}

func TestCanonicalToTitanEmbedResponse_MissingData(t *testing.T) {
	canonical := []byte(`{"object":"list","model":"x","usage":{}}`)
	_, err := canonicalToTitanEmbedResponse(canonical)
	if err == nil || !strings.Contains(err.Error(), "missing data") {
		t.Fatalf("expected missing-data error, got %v", err)
	}
}

func TestCanonicalToCohereEmbedResponse_MissingData(t *testing.T) {
	canonical := []byte(`{"object":"list","model":"x","usage":{}}`)
	_, err := canonicalToCohereEmbedResponse(canonical, "cohere.embed-english-v3")
	if err == nil || !strings.Contains(err.Error(), "missing data") {
		t.Fatalf("expected missing-data error, got %v", err)
	}
}

// Generic fallback decoder.

func TestGenericBedrockEmbedResponse_EmbeddingField(t *testing.T) {
	native := []byte(`{"embedding":[0.1,0.2]}`)
	res, err := decodeGenericBedrockEmbedResponse(native, "x")
	if err != nil {
		t.Fatalf("generic decode: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("data len=%d want 1", len(data))
	}
}

func TestGenericBedrockEmbedResponse_EmbeddingsField(t *testing.T) {
	native := []byte(`{"embeddings":[[0.1,0.2],[0.3,0.4]]}`)
	res, err := decodeGenericBedrockEmbedResponse(native, "x")
	if err != nil {
		t.Fatalf("generic decode embeddings: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
	// Generic fallback only extracts the first embedding row.
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("data len=%d want 1 (first row only)", len(data))
	}
}

func TestGenericBedrockEmbedResponse_NoEmbeddingFields(t *testing.T) {
	native := []byte(`{"some":"value"}`)
	res, err := decodeGenericBedrockEmbedResponse(native, "x")
	if err != nil {
		t.Fatalf("generic decode no-emb: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
}

func TestGenericBedrockEmbedResponse_InvalidJSON(t *testing.T) {
	_, err := decodeGenericBedrockEmbedResponse([]byte(`not-json`), "x")
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

// embeddingDecodeResponse (named dispatch path).

func TestEmbeddingDecodeResponse_TitanModel(t *testing.T) {
	native := []byte(`{"embedding":[0.1],"inputTextTokenCount":2}`)
	res, err := embeddingDecodeResponse(native, "amazon.titan-embed-text-v2:0")
	if err != nil {
		t.Fatalf("embeddingDecodeResponse titan: %v", err)
	}
	// Verify we got a canonical body.
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
}

func TestEmbeddingDecodeResponse_CohereModel(t *testing.T) {
	native := []byte(`{"embeddings":[[0.1]],"id":"x","response_type":"embeddings_floats"}`)
	res, err := embeddingDecodeResponse(native, "cohere.embed-english-v3")
	if err != nil {
		t.Fatalf("embeddingDecodeResponse cohere: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
}

func TestEmbeddingDecodeResponse_UnknownModel(t *testing.T) {
	native := []byte(`{"embedding":[0.1]}`)
	res, err := embeddingDecodeResponse(native, "unknown.model")
	if err != nil {
		t.Fatalf("embeddingDecodeResponse unknown: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal(res.CanonicalBody, &out); e != nil {
		t.Fatalf("canonical not JSON: %v", e)
	}
}
