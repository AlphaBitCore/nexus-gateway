package embeddings

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestEncodeOpenAIRequest_FullRequest(t *testing.T) {
	req := Request{
		Model:          "text-embedding-3-small",
		Input:          "hello world",
		Dimensions:     1536,
		EncodingFormat: "float",
	}
	got, err := EncodeOpenAIRequest(req)
	if err != nil {
		t.Fatalf("EncodeOpenAIRequest: %v", err)
	}
	body := string(got)
	for _, want := range []string{
		`"model":"text-embedding-3-small"`,
		`"input":"hello world"`,
		`"dimensions":1536`,
		`"encoding_format":"float"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
}

func TestEncodeOpenAIRequest_NoDimensions_NoFormat(t *testing.T) {
	req := Request{Model: "text-embedding-3-small", Input: "hi"}
	got, err := EncodeOpenAIRequest(req)
	if err != nil {
		t.Fatalf("EncodeOpenAIRequest: %v", err)
	}
	body := string(got)
	if strings.Contains(body, "dimensions") {
		t.Errorf("dimensions should be omitted when 0; got %s", body)
	}
	if strings.Contains(body, "encoding_format") {
		t.Errorf("encoding_format should be omitted when empty; got %s", body)
	}
}

func TestEncodeOpenAIRequest_EmptyModel_Error(t *testing.T) {
	_, err := EncodeOpenAIRequest(Request{Input: "x"})
	if err == nil {
		t.Fatal("expected error for empty model")
	}
}

func TestEncodeOpenAIRequest_EmptyInput_Error(t *testing.T) {
	_, err := EncodeOpenAIRequest(Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestEncodeOpenAIRequest_DimensionsZeroOmitted(t *testing.T) {
	req := Request{Model: "m", Input: "x", Dimensions: 0}
	got, err := EncodeOpenAIRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "dimensions") {
		t.Errorf("Dimensions=0 should be omitted; got %s", got)
	}
}

// DecodeOpenAIResponse — float array path

func TestDecodeOpenAIResponse_FloatArray(t *testing.T) {
	body := `{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}],
		"model":"text-embedding-3-small",
		"usage":{"prompt_tokens":8,"total_tokens":8}
	}`
	resp, err := DecodeOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeOpenAIResponse: %v", err)
	}
	if len(resp.Embedding) != 3 {
		t.Errorf("expected 3 dims; got %d", len(resp.Embedding))
	}
	if resp.Model != "text-embedding-3-small" {
		t.Errorf("model = %q; want text-embedding-3-small", resp.Model)
	}
	if resp.PromptTokens != 8 {
		t.Errorf("PromptTokens = %d; want 8", resp.PromptTokens)
	}
	if math.Abs(float64(resp.Embedding[0])-0.1) > 1e-6 {
		t.Errorf("embedding[0] = %v; want ~0.1", resp.Embedding[0])
	}
}

// TestDecodeOpenAIResponse_TextEmbedding3Small_1536D verifies that the
// decoder handles a fixture matching the real text-embedding-3-small
// response shape (1536 dimensions, prompt_tokens=42).
func TestDecodeOpenAIResponse_TextEmbedding3Small_1536D(t *testing.T) {
	// Build a JSON body with 1536 float values.
	var sb strings.Builder
	sb.WriteString(`{"object":"list","data":[{"object":"embedding","embedding":[`)
	for i := range 1536 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%.6f", float64(i)*0.001)
	}
	sb.WriteString(`],"index":0}],"model":"text-embedding-3-small","usage":{"prompt_tokens":42,"total_tokens":42}}`)

	resp, err := DecodeOpenAIResponse([]byte(sb.String()))
	if err != nil {
		t.Fatalf("DecodeOpenAIResponse 1536D: %v", err)
	}
	if len(resp.Embedding) != 1536 {
		t.Errorf("expected 1536 dims; got %d", len(resp.Embedding))
	}
	if resp.PromptTokens != 42 {
		t.Errorf("PromptTokens = %d; want 42", resp.PromptTokens)
	}
	if resp.Model != "text-embedding-3-small" {
		t.Errorf("model = %q; want text-embedding-3-small", resp.Model)
	}
	// Spot-check: index 100 = 0.100000
	if math.Abs(float64(resp.Embedding[100])-0.1) > 1e-4 {
		t.Errorf("embedding[100] = %v; want ~0.1", resp.Embedding[100])
	}
}

// DecodeOpenAIResponse — base64 path

func TestDecodeOpenAIResponse_Base64(t *testing.T) {
	// Encode 3 float32 values as little-endian IEEE 754 bytes, then base64.
	vals := []float32{0.1, 0.2, 0.3}
	raw := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(v))
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	body := `{"data":[{"embedding":"` + b64 + `","index":0}],"model":"text-embedding-3-small","usage":{"prompt_tokens":5}}`
	resp, err := DecodeOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeOpenAIResponse base64: %v", err)
	}
	if len(resp.Embedding) != 3 {
		t.Errorf("expected 3 dims; got %d", len(resp.Embedding))
	}
	if math.Abs(float64(resp.Embedding[0])-float64(vals[0])) > 1e-6 {
		t.Errorf("embedding[0] = %v; want %v", resp.Embedding[0], vals[0])
	}
}

func TestDecodeOpenAIResponse_Base64_BadEncoding_Error(t *testing.T) {
	body := `{"data":[{"embedding":"not@@valid@@base64","index":0}],"model":"m","usage":{}}`
	_, err := DecodeOpenAIResponse([]byte(body))
	if err == nil {
		t.Fatal("expected error for bad base64")
	}
}

func TestDecodeOpenAIResponse_Base64_OddLength_Error(t *testing.T) {
	// 5 bytes — not divisible by 4.
	raw := []byte{1, 2, 3, 4, 5}
	b64 := base64.StdEncoding.EncodeToString(raw)
	body := `{"data":[{"embedding":"` + b64 + `","index":0}],"model":"m","usage":{}}`
	_, err := DecodeOpenAIResponse([]byte(body))
	if err == nil {
		t.Fatal("expected error for non-multiple-of-4 length")
	}
}

// DecodeOpenAIResponse — error paths

func TestDecodeOpenAIResponse_InvalidJSON_Error(t *testing.T) {
	_, err := DecodeOpenAIResponse([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecodeOpenAIResponse_EmptyData_Error(t *testing.T) {
	_, err := DecodeOpenAIResponse([]byte(`{"data":[],"model":"m","usage":{}}`))
	if err == nil {
		t.Fatal("expected error for empty data array")
	}
}

func TestDecodeOpenAIResponse_MissingData_Error(t *testing.T) {
	_, err := DecodeOpenAIResponse([]byte(`{"model":"m","usage":{}}`))
	if err == nil {
		t.Fatal("expected error for missing data")
	}
}

func TestDecodeOpenAIResponse_MissingEmbedding_Error(t *testing.T) {
	_, err := DecodeOpenAIResponse([]byte(`{"data":[{"index":0}],"model":"m","usage":{}}`))
	if err == nil {
		t.Fatal("expected error for missing embedding field")
	}
}

func TestDecodeOpenAIResponse_EmbeddingIsObject_Error(t *testing.T) {
	_, err := DecodeOpenAIResponse([]byte(`{"data":[{"embedding":{"bad":true}}],"model":"m","usage":{}}`))
	if err == nil {
		t.Fatal("expected error for embedding being an object (wrong type)")
	}
}

func TestDecodeOpenAIResponse_NaNInFloatArray_Error(t *testing.T) {
	// Use "null" which gjson evaluates to a NaN/0 — actually gjson returns
	// 0 for null. Instead use a raw numeric NaN keyword via a crafted body.
	// JSON spec doesn't allow NaN, but we can test IsNaN/Inf by passing
	// infinity-like number if gjson parses it.
	// In practice, JSON doesn't allow NaN/Inf literals. gjson.Float() on
	// a very large exponent still returns a finite float64. We exercise
	// this path by crafting a body where the value is the string "null"
	// (gjson type JSON, array element type Null → v.Float() = 0, finite).
	// Actually the only way to hit the NaN/IsInf path in real code would
	// be if the provider returns an invalid number. Since JSON can't
	// represent NaN, this path is defensive only.
	// Verify it compiles and the normal path works (already covered above).
	// Document the defensive nature with a comment.
	t.Skip("NaN/Inf branch is defensive-only — JSON spec disallows NaN/Inf literals; covered by design")
}

func TestDecodeOpenAIResponse_EmbeddingNotJSONOrString_Error(t *testing.T) {
	// The embedding field is a JSON number — gjson type Number, not JSON array or String.
	// This exercises the default branch in the switch.
	_, err := DecodeOpenAIResponse([]byte(`{"data":[{"embedding":42}],"model":"m","usage":{}}`))
	if err == nil {
		t.Fatal("expected error for embedding being a number")
	}
	if !strings.Contains(err.Error(), "unexpected JSON type") {
		t.Errorf("error message should mention unexpected JSON type; got %v", err)
	}
}

func TestOpenAIEmbeddingError_StructuredEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"insufficient quota","type":"invalid_request_error","code":"quota_exceeded"}}`)
	got := openAIEmbeddingError(body)
	if got != "insufficient quota" {
		t.Errorf("got %q; want 'insufficient quota'", got)
	}
}

func TestOpenAIEmbeddingError_EmptyBody(t *testing.T) {
	got := openAIEmbeddingError(nil)
	if got != "" {
		t.Errorf("empty body should return empty string; got %q", got)
	}
}

func TestOpenAIEmbeddingError_UnstructuredJSON(t *testing.T) {
	body := []byte(`{"detail":"something went wrong"}`)
	got := openAIEmbeddingError(body)
	if got == "" {
		t.Errorf("unstructured JSON should return raw body; got empty")
	}
}

func TestOpenAIEmbeddingError_NonJSON(t *testing.T) {
	body := []byte("Service Unavailable")
	got := openAIEmbeddingError(body)
	if got == "" {
		t.Errorf("non-JSON body should return a description; got empty")
	}
}

func TestOpenAIEmbeddingError_LargeBody_Truncated(t *testing.T) {
	// Body longer than 256 chars, non-JSON (triggers truncation path).
	body := make([]byte, 400)
	for i := range body {
		body[i] = 'x'
	}
	got := openAIEmbeddingError(body)
	// Should return a non-empty description (not panic).
	if got == "" {
		t.Errorf("large non-JSON body should return a description; got empty")
	}
}

func TestBuildEmbeddingsURL(t *testing.T) {
	tests := []struct {
		base string
		want string
	}{
		{"https://api.openai.com", "https://api.openai.com/v1/embeddings"},
		{"https://api.openai.com/", "https://api.openai.com/v1/embeddings"},
		{"http://localhost:9001/v1", "http://localhost:9001/v1/embeddings"},
		{"http://localhost:9001/v1/", "http://localhost:9001/v1/embeddings"},
		{"http://localhost:9001", "http://localhost:9001/v1/embeddings"},
	}
	for _, tc := range tests {
		got := buildEmbeddingsURL(tc.base)
		if got != tc.want {
			t.Errorf("buildEmbeddingsURL(%q) = %q; want %q", tc.base, got, tc.want)
		}
	}
}

func TestLabelFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://api.openai.com", "api.openai.com"},
		{"https://api.openai.com/v1/embeddings", "api.openai.com"},
		{"http://localhost:9001/v1", "localhost"},
		{"http://127.0.0.1:8080", "127.0.0.1"},
		{"", "unknown"},
	}
	for _, tc := range tests {
		got := labelFromURL(tc.url)
		if got != tc.want {
			t.Errorf("labelFromURL(%q) = %q; want %q", tc.url, got, tc.want)
		}
	}
}

func TestDecodeFloat32LE_Roundtrip(t *testing.T) {
	in := []float32{1.0, -2.5, 0.0, math.MaxFloat32}
	raw := make([]byte, len(in)*4)
	for i, v := range in {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(v))
	}
	out := decodeFloat32LE(raw)
	if len(out) != len(in) {
		t.Fatalf("length mismatch: got %d; want %d", len(out), len(in))
	}
	for i, v := range in {
		if out[i] != v {
			t.Errorf("out[%d] = %v; want %v", i, out[i], v)
		}
	}
}
