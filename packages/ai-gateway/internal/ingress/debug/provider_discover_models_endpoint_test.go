// Tests for ProviderDiscoverModelsHandler and SuggestModelType.
//
// Named failure modes covered:
//   - OpenAI-family adapter + conforming upstream list → HTTP 200, models list
//     with suggestedType populated
//   - Adapter that does NOT implement model listing (stub) → HTTP 400,
//     error code "discovery_unsupported"
//   - Invalid adapterType → HTTP 400
//   - Missing baseUrl → HTTP 400
//   - SuggestModelType: embedding / audio / image / chat classification
package debug

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	specanthropic "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	specopenai "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// TestSuggestModelType verifies the heuristic classification for known
// model-id patterns.
func TestSuggestModelType(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"text-embedding-3-small", "embedding"},
		{"text-embedding-ada-002", "embedding"},
		{"x-embedding-3", "embedding"},
		{"whisper-1", "audio"},
		{"tts-1", "audio"},
		{"tts-1-hd", "audio"},
		{"audio-preview", "audio"},
		{"gpt-4o-transcribe", "audio"},
		{"dall-e-3", "image"},
		{"dall-e-2", "image"},
		{"gpt-image-1", "image"},
		{"gpt-4o", "chat"},
		{"gpt-4o-mini", "chat"},
		{"o3", "chat"},
		{"claude-3-5-sonnet", "chat"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := SuggestModelType(tc.id)
			if got != tc.want {
				t.Errorf("SuggestModelType(%q) = %q; want %q", tc.id, got, tc.want)
			}
		})
	}
}

// buildOpenAIRegistry builds a Registry containing the real OpenAI spec
// adapter so that ProviderDiscoverModelsHandler can type-assert the adapter
// to modelLister.
func buildOpenAIRegistry(t *testing.T, log *slog.Logger) *provcore.Registry {
	t.Helper()
	reg := provcore.NewRegistry()
	spec := specopenai.NewSpec(log)
	reg.MustRegister(provdispatch.NewSpecAdapterWithAllowlist(spec, nil, log))
	return reg
}

// buildNoListModelsRegistry builds a Registry containing a stub adapter that
// does NOT implement model listing (no openai transport). Used to verify
// the discovery_unsupported error path.
func buildNoListModelsRegistry(t *testing.T) *provcore.Registry {
	t.Helper()
	reg := provcore.NewRegistry()
	// Register the probeOnlyAdapter defined in debug_branches_test.go as the
	// openai format so the handler can look it up but it won't satisfy modelLister.
	reg.MustRegister(probeOnlyAdapter{format: provcore.FormatOpenAI})
	return reg
}

// TestProviderDiscoverModels_happyPath verifies the full success path:
// a real OpenAI spec adapter is registered; the upstream httptest server
// returns a conforming /v1/models list; the handler returns HTTP 200 with
// success:true and models with suggestedType populated.
func TestProviderDiscoverModels_happyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q; want /v1/models", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %q; want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-gpt-4o-mini"},{"id":"mock-text-embedding-3-small"}]}`))
	}))
	defer upstream.Close()

	log := slog.Default()
	reg := buildOpenAIRegistry(t, log)
	h := ProviderDiscoverModelsHandler(reg, log)

	body, _ := json.Marshal(map[string]any{
		"adapterType": "openai",
		"baseUrl":     upstream.URL,
		"apiKey":      "sk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Models  []struct {
			ID            string `json:"id"`
			SuggestedType string `json:"suggestedType"`
		} `json:"models"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success: got false, want true")
	}
	if len(resp.Models) != 2 {
		t.Fatalf("models count: got %d, want 2; models=%v", len(resp.Models), resp.Models)
	}
	// Check first model: mock-gpt-4o-mini → chat
	if resp.Models[0].ID != "mock-gpt-4o-mini" {
		t.Errorf("models[0].id: got %q, want mock-gpt-4o-mini", resp.Models[0].ID)
	}
	if resp.Models[0].SuggestedType != "chat" {
		t.Errorf("models[0].suggestedType: got %q, want chat", resp.Models[0].SuggestedType)
	}
	// Check second model: mock-text-embedding-3-small → embedding
	if resp.Models[1].ID != "mock-text-embedding-3-small" {
		t.Errorf("models[1].id: got %q, want mock-text-embedding-3-small", resp.Models[1].ID)
	}
	if resp.Models[1].SuggestedType != "embedding" {
		t.Errorf("models[1].suggestedType: got %q, want embedding", resp.Models[1].SuggestedType)
	}
}

// TestProviderDiscoverModels_unsupportedAdapter verifies that a stub adapter
// (probeOnlyAdapter) which does NOT implement model listing returns HTTP 400
// with error code "discovery_unsupported".
func TestProviderDiscoverModels_unsupportedAdapter(t *testing.T) {
	reg := buildNoListModelsRegistry(t)
	h := ProviderDiscoverModelsHandler(reg, slog.Default())

	body, _ := json.Marshal(map[string]any{
		"adapterType": "openai",
		"baseUrl":     "https://api.example.com",
		"apiKey":      "sk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["success"] != false {
		t.Errorf("success: got %v, want false", resp["success"])
	}
	code, _ := resp["code"].(string)
	if code != "discovery_unsupported" {
		t.Errorf("code: got %q, want discovery_unsupported", code)
	}
	errStr, _ := resp["error"].(string)
	if !strings.Contains(strings.ToLower(errStr), "openai") && !strings.Contains(strings.ToLower(errStr), "discovery") {
		t.Errorf("error message %q does not mention openai or discovery", errStr)
	}
}

// TestProviderDiscoverModels_invalidAdapterType verifies that an unknown
// adapterType returns HTTP 400 before any adapter lookup.
func TestProviderDiscoverModels_invalidAdapterType(t *testing.T) {
	reg := provcore.NewRegistry()
	h := ProviderDiscoverModelsHandler(reg, slog.Default())

	body, _ := json.Marshal(map[string]any{
		"adapterType": "totally-unknown-format",
		"baseUrl":     "https://api.example.com",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["success"] != false {
		t.Errorf("success: got %v, want false", resp["success"])
	}
}

// TestProviderDiscoverModels_missingBaseURL verifies that an empty baseUrl
// returns HTTP 400 before any network call is made.
func TestProviderDiscoverModels_missingBaseURL(t *testing.T) {
	reg := provcore.NewRegistry()
	h := ProviderDiscoverModelsHandler(reg, slog.Default())

	body, _ := json.Marshal(map[string]any{
		"adapterType": "openai",
		"baseUrl":     "",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["success"] != false {
		t.Errorf("success: got %v, want false", resp["success"])
	}
}

// TestProviderDiscoverModels_upstreamError verifies that when the upstream
// /v1/models call fails (non-2xx), the handler returns HTTP 200 with
// success:false and the error message.
func TestProviderDiscoverModels_upstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	log := slog.Default()
	reg := buildOpenAIRegistry(t, log)
	h := ProviderDiscoverModelsHandler(reg, log)

	body, _ := json.Marshal(map[string]any{
		"adapterType": "openai",
		"baseUrl":     upstream.URL,
		"apiKey":      "bad-key",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (upstream errors use 200+success:false); body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["success"] != false {
		t.Errorf("success: got %v, want false", resp["success"])
	}
	if resp["error"] == nil {
		t.Error("error field should be present on upstream failure")
	}
}

// TestProviderDiscoverModels_invalidJSON verifies that a malformed request
// body returns HTTP 400.
func TestProviderDiscoverModels_invalidJSON(t *testing.T) {
	reg := provcore.NewRegistry()
	h := ProviderDiscoverModelsHandler(reg, slog.Default())
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader("{invalid json"))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// buildAnthropicRegistry builds a Registry containing the real Anthropic spec
// adapter. Anthropic's transport does NOT implement the transportModelLister
// interface (no ListModels method), so specAdapter.ListModels returns
// (nil, false, nil) — the handler must respond with HTTP 400 and
// code "discovery_unsupported". This test exercises the production
// supported=false gate rather than the stub cast path.
func buildAnthropicRegistry(t *testing.T, log *slog.Logger) *provcore.Registry {
	t.Helper()
	reg := provcore.NewRegistry()
	spec := specanthropic.NewSpec(log)
	reg.MustRegister(provdispatch.NewSpecAdapterWithAllowlist(spec, nil, log))
	return reg
}

// TestProviderDiscoverModels_anthropicUnsupported verifies that the real
// Anthropic spec adapter — registered via the production specAdapter path —
// returns HTTP 400 with code "discovery_unsupported". This exercises the
// supported=false branch: specAdapter.ListModels type-asserts the Anthropic
// transport to transportModelLister, the assertion fails (Anthropic has no
// ListModels), and the adapter returns (nil, false, nil).
func TestProviderDiscoverModels_anthropicUnsupported(t *testing.T) {
	log := slog.Default()
	reg := buildAnthropicRegistry(t, log)
	h := ProviderDiscoverModelsHandler(reg, log)

	body, _ := json.Marshal(map[string]any{
		"adapterType": "anthropic",
		"baseUrl":     "https://api.anthropic.com",
		"apiKey":      "sk-ant-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/provider-discover-models",
		strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["success"] != false {
		t.Errorf("success: got %v, want false", resp["success"])
	}
	code, _ := resp["code"].(string)
	if code != "discovery_unsupported" {
		t.Errorf("code: got %q, want discovery_unsupported", code)
	}
	errStr, _ := resp["error"].(string)
	if !strings.Contains(strings.ToLower(errStr), "openai") && !strings.Contains(strings.ToLower(errStr), "discovery") {
		t.Errorf("error message %q does not mention openai or discovery", errStr)
	}
}
