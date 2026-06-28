package debug

import (
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sharedEmbeddingProbeHandler is constructed once for the whole test binary to
// avoid duplicate Prometheus metric registration panics. EmbeddingProbeHandler
// calls embeddings.NewClient which registers metrics in the global default
// registerer under the "nexus_probe" namespace; registering the same metrics
// twice panics, so we share a single handler across all tests.
var sharedEmbeddingProbeHandler = EmbeddingProbeHandler(&http.Client{}, slog.Default())

// openAIEmbeddingFixture returns a minimal valid OpenAI /v1/embeddings
// response JSON with the given float32 embedding values.
func openAIEmbeddingFixture(values []float32) string {
	arr := make([]string, len(values))
	for i, v := range values {
		arr[i] = fmt.Sprintf("%g", v)
	}
	return fmt.Sprintf(`{
		"object": "list",
		"data": [{"object":"embedding","embedding":[%s],"index":0}],
		"model": "text-embedding-3-small",
		"usage": {"prompt_tokens": 3, "total_tokens": 3}
	}`, strings.Join(arr, ","))
}

// newEmbeddingProbeReq builds an HTTP request with the given JSON body
// for POST /internal/embedding-probe.
func newEmbeddingProbeReq(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/internal/embedding-probe",
		strings.NewReader(body))
}

// TestEmbeddingProbeHandler_InvalidJSON_Returns400 verifies that a
// malformed JSON body yields 400 with ok=false.
func TestEmbeddingProbeHandler_InvalidJSON_Returns400(t *testing.T) {
	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, newEmbeddingProbeReq(`{bad json`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var resp embeddingProbeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("ok: got true, want false")
	}
	if !strings.Contains(resp.Error, "invalid request body") {
		t.Errorf("error: got %q, want 'invalid request body' substring", resp.Error)
	}
}

// TestEmbeddingProbeHandler_MissingBaseURL_Returns400 verifies that an
// empty baseUrl field yields 400.
func TestEmbeddingProbeHandler_MissingBaseURL_Returns400(t *testing.T) {
	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, newEmbeddingProbeReq(`{"providerModelId":"text-embedding-3-small"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var resp embeddingProbeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("ok: got true, want false")
	}
	if resp.Error != "baseUrl is required" {
		t.Errorf("error: got %q, want 'baseUrl is required'", resp.Error)
	}
}

// TestEmbeddingProbeHandler_MissingProviderModelID_Returns400 verifies
// that a missing providerModelId field yields 400.
func TestEmbeddingProbeHandler_MissingProviderModelID_Returns400(t *testing.T) {
	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, newEmbeddingProbeReq(`{"baseUrl":"https://api.openai.com"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var resp embeddingProbeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("ok: got true, want false")
	}
	if resp.Error != "providerModelId is required" {
		t.Errorf("error: got %q, want 'providerModelId is required'", resp.Error)
	}
}

// TestEmbeddingProbeHandler_SuccessSmallEmbedding_Returns200WithOkTrue
// verifies the happy path: the upstream returns a short embedding (≤10
// values) and the response mirrors it in sampleEmbeddingFirst10.
func TestEmbeddingProbeHandler_SuccessSmallEmbedding_Returns200WithOkTrue(t *testing.T) {
	// 4-dimensional embedding — less than 10, so sample == full vector.
	fixture := openAIEmbeddingFixture([]float32{0.1, 0.2, 0.3, 0.4})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, fixture)
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"providerId":      "prov-1",
		"modelId":         "mod-1",
		"modelName":       "text-embedding-3-small",
		"providerModelId": "text-embedding-3-small",
		"baseUrl":         srv.URL,
		"apiKey":          "sk-test",
		"dimension":       4,
	})

	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, httptest.NewRequest(http.MethodPost, "/internal/embedding-probe",
		strings.NewReader(string(body))))

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	var resp embeddingProbeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("ok: got false, want true; error=%q", resp.Error)
	}
	if resp.ProviderID != "prov-1" {
		t.Errorf("providerId: got %q, want prov-1", resp.ProviderID)
	}
	if resp.ModelID != "mod-1" {
		t.Errorf("modelId: got %q, want mod-1", resp.ModelID)
	}
	if resp.Dimension != 4 {
		t.Errorf("dimension: got %d, want 4", resp.Dimension)
	}
	if resp.PromptTokens != 3 {
		t.Errorf("promptTokens: got %d, want 3", resp.PromptTokens)
	}
	if len(resp.SampleEmbeddingFirst10) != 4 {
		t.Errorf("sampleEmbeddingFirst10 len: got %d, want 4", len(resp.SampleEmbeddingFirst10))
	}
	if resp.LatencyMs < 0 {
		t.Errorf("latencyMs: got %d, want ≥0", resp.LatencyMs)
	}
	if resp.Error != "" {
		t.Errorf("error: got %q, want empty", resp.Error)
	}
}

// TestEmbeddingProbeHandler_SuccessLargeEmbedding_TruncatesAt10 verifies
// that a 1536-dimensional embedding is truncated to 10 values in the sample.
func TestEmbeddingProbeHandler_SuccessLargeEmbedding_TruncatesAt10(t *testing.T) {
	values := make([]float32, 1536)
	for i := range values {
		values[i] = float32(i) * 0.001
	}
	fixture := openAIEmbeddingFixture(values)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, fixture)
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"providerModelId": "text-embedding-3-small",
		"baseUrl":         srv.URL,
		"dimension":       0, // skip dim check
	})

	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, httptest.NewRequest(http.MethodPost, "/internal/embedding-probe",
		strings.NewReader(string(body))))

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	var resp embeddingProbeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("ok: got false, want true; error=%q", resp.Error)
	}
	if len(resp.SampleEmbeddingFirst10) != 10 {
		t.Errorf("sampleEmbeddingFirst10 len: got %d, want 10", len(resp.SampleEmbeddingFirst10))
	}
	if resp.Dimension != 1536 {
		t.Errorf("dimension: got %d, want 1536", resp.Dimension)
	}
}

// TestEmbeddingProbeHandler_UpstreamError_Returns200WithOkFalse verifies
// that a non-2xx upstream response yields 200 HTTP with ok=false and an
// error message in the response body.
func TestEmbeddingProbeHandler_UpstreamError_Returns200WithOkFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"server overloaded"}}`)
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"providerId":      "prov-2",
		"modelId":         "mod-2",
		"providerModelId": "text-embedding-3-small",
		"baseUrl":         srv.URL,
	})

	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, httptest.NewRequest(http.MethodPost, "/internal/embedding-probe",
		strings.NewReader(string(body))))

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (error is in body)", w.Code)
	}
	var resp embeddingProbeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("ok: got true, want false")
	}
	if resp.Error == "" {
		t.Error("error: got empty, want non-empty")
	}
	if resp.ProviderID != "prov-2" {
		t.Errorf("providerId: got %q, want prov-2", resp.ProviderID)
	}
}

// TestEmbeddingProbeHandler_NoAPIKey_OmitsAuthHeader verifies that an
// empty apiKey field does not send an Authorization header (local inference
// server scenario).
func TestEmbeddingProbeHandler_NoAPIKey_OmitsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, openAIEmbeddingFixture([]float32{0.5, 0.5}))
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"providerModelId": "BAAI/bge-small-en-v1.5",
		"baseUrl":         srv.URL,
		"apiKey":          "", // no key
		"dimension":       0,
	})

	w := httptest.NewRecorder()
	sharedEmbeddingProbeHandler(w, httptest.NewRequest(http.MethodPost, "/internal/embedding-probe",
		strings.NewReader(string(body))))

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header: got %q, want empty for no apiKey", gotAuth)
	}
}
