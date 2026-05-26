package embeddings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// silentLogger returns a logger that discards all output so test runs
// don't emit noise.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestClient constructs a Client backed by a plain http.Client with
// no transport overrides (suitable for httptest servers).
func newTestClient() *Client {
	return &Client{
		http:    &http.Client{Timeout: 5 * time.Second},
		log:     silentLogger(),
		metrics: nil, // nil metrics — observeCall/observeTokens guard against nil
	}
}

// embeddingHandler is a convenience constructor for httptest servers
// that return a fixed OpenAI-style embeddings JSON body.
func embeddingHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// floatEmbeddingBody constructs an OpenAI embeddings response JSON with
// the given float32 slice and prompt_tokens count.
func floatEmbeddingBody(floats []float32, promptTokens int) string {
	// Marshal the embedding as a JSON array.
	arr, _ := json.Marshal(floats)
	return `{"object":"list","data":[{"object":"embedding","embedding":` +
		string(arr) +
		`,"index":0}],"model":"text-embedding-3-small","usage":{"prompt_tokens":` +
		jsonInt(promptTokens) + `,"total_tokens":` + jsonInt(promptTokens) + `}}`
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Embed — success path

func TestClient_Embed_Success(t *testing.T) {
	floats := []float32{0.1, 0.2, 0.3}
	srv := httptest.NewServer(embeddingHandler(http.StatusOK, floatEmbeddingBody(floats, 10)))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "text-embedding-3-small", Input: "hello"}
	resp, err := c.Embed(context.Background(), srv.URL, "text-embedding-3-small", "sk-test", req, 0)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Embedding) != 3 {
		t.Errorf("embedding length = %d; want 3", len(resp.Embedding))
	}
	if resp.PromptTokens != 10 {
		t.Errorf("PromptTokens = %d; want 10", resp.PromptTokens)
	}
}

// Embed — verifies correct path is called

func TestClient_Embed_CallsEmbeddingsPath(t *testing.T) {
	var capturedPath string
	floats := []float32{0.5}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(floatEmbeddingBody(floats, 5)))
	}))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "test"}
	_, _ = c.Embed(context.Background(), srv.URL, "m", "key", req, 0)
	if capturedPath != "/v1/embeddings" {
		t.Errorf("path = %q; want /v1/embeddings", capturedPath)
	}
}

// Embed — bearer auth header

func TestClient_Embed_SetsAuthHeader(t *testing.T) {
	var capturedAuth string
	floats := []float32{0.1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(floatEmbeddingBody(floats, 3)))
	}))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, _ = c.Embed(context.Background(), srv.URL, "m", "my-api-key", req, 0)
	if capturedAuth != "Bearer my-api-key" {
		t.Errorf("Authorization header = %q; want 'Bearer my-api-key'", capturedAuth)
	}
}

func TestClient_Embed_EmptyAPIKey_NoAuthHeader(t *testing.T) {
	var capturedAuth string
	floats := []float32{0.1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(floatEmbeddingBody(floats, 3)))
	}))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, _ = c.Embed(context.Background(), srv.URL, "m", "", req, 0)
	if capturedAuth != "" {
		t.Errorf("empty apiKey should omit Authorization header; got %q", capturedAuth)
	}
}

// Embed — HTTP 500

func TestClient_Embed_HTTP500_ReturnsProviderError(t *testing.T) {
	srv := httptest.NewServer(embeddingHandler(http.StatusInternalServerError,
		`{"error":{"message":"internal server error"}}`))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 0)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !errors.Is(err, ErrEmbeddingProviderError) {
		t.Errorf("err type = %T (%v); want ErrEmbeddingProviderError", err, err)
	}
}

// Embed — HTTP 429

func TestClient_Embed_HTTP429_ReturnsProviderError(t *testing.T) {
	srv := httptest.NewServer(embeddingHandler(http.StatusTooManyRequests,
		`{"error":{"message":"rate limit exceeded"}}`))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 0)
	if !errors.Is(err, ErrEmbeddingProviderError) {
		t.Errorf("err = %v; want ErrEmbeddingProviderError", err)
	}
}

// Embed — timeout via cancelled context

func TestClient_Embed_Timeout_ReturnsErrEmbeddingTimeout(t *testing.T) {
	// Server that hangs and tracks when it should stop.
	stopCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stopCh:
		}
	}))
	// Signal the server goroutine to exit before closing connections.
	defer func() {
		close(stopCh)
		srv.CloseClientConnections()
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(ctx, srv.URL, "m", "k", req, 0)
	if !errors.Is(err, ErrEmbeddingTimeout) {
		t.Errorf("err = %v; want ErrEmbeddingTimeout", err)
	}
}

// Embed — dimension mismatch

func TestClient_Embed_DimMismatch_ReturnsErrEmbeddingDimMismatch(t *testing.T) {
	floats := []float32{0.1, 0.2, 0.3} // 3-dim vector
	srv := httptest.NewServer(embeddingHandler(http.StatusOK, floatEmbeddingBody(floats, 5)))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 1536 /* expectedDim */)
	if !errors.Is(err, ErrEmbeddingDimMismatch) {
		t.Errorf("err = %v; want ErrEmbeddingDimMismatch", err)
	}
}

func TestClient_Embed_DimMatch_NoError(t *testing.T) {
	floats := []float32{0.1, 0.2, 0.3}
	srv := httptest.NewServer(embeddingHandler(http.StatusOK, floatEmbeddingBody(floats, 5)))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	resp, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 3)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Embedding) != 3 {
		t.Errorf("expected 3 dims; got %d", len(resp.Embedding))
	}
}

// Embed — bad URL (transport error)

func TestClient_Embed_BadURL_ReturnsProviderError(t *testing.T) {
	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(context.Background(), "http://127.0.0.1:1", "m", "k", req, 0)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	if errors.Is(err, ErrEmbeddingTimeout) {
		t.Errorf("connection refused should be provider_error, not timeout")
	}
	if !errors.Is(err, ErrEmbeddingProviderError) {
		t.Errorf("err = %v; want ErrEmbeddingProviderError", err)
	}
}

// TestClient_Embed_EmptyModel_ReturnsEncodeError exercises the
// EncodeOpenAIRequest error path within Embed.
func TestClient_Embed_EmptyModel_ReturnsEncodeError(t *testing.T) {
	c := newTestClient()
	req := Request{Model: "", Input: "x"} // empty model triggers encode error
	_, err := c.Embed(context.Background(), "http://localhost", "", "k", req, 0)
	if err == nil {
		t.Fatal("expected error for empty model in request")
	}
	if strings.Contains(err.Error(), "ErrEmbeddingTimeout") || errors.Is(err, ErrEmbeddingTimeout) {
		t.Errorf("encode error should not be timeout; got %v", err)
	}
}

// TestClient_Embed_InvalidURL_RequestBuildError exercises the
// http.NewRequestWithContext error path (bad scheme makes URL construction fail).
func TestClient_Embed_InvalidURL_RequestBuildError(t *testing.T) {
	c := newTestClient()
	req := Request{Model: "m", Input: "x"}
	// "://bad" is not a valid URL and NewRequestWithContext returns an error.
	_, err := c.Embed(context.Background(), "://bad", "m", "k", req, 0)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
	// This error is wrapped as a build error, not provider_error or timeout.
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("err = %v; want 'build request' in message", err)
	}
}

// Embed — local-inference server (same OpenAI codec, different baseURL)

func TestClient_Embed_LocalInference_CompatibleServer(t *testing.T) {
	// A local-inference server that speaks the OpenAI embeddings shape.
	floats := make([]float32, 384) // local-bge-small dim
	for i := range floats {
		floats[i] = float32(i) * 0.001
	}
	localBody := floatEmbeddingBody(floats, 6)
	localBody = strings.Replace(localBody, `"text-embedding-3-small"`, `"local-bge-small"`, 1)

	srv := httptest.NewServer(embeddingHandler(http.StatusOK, localBody))
	defer srv.Close()

	c := newTestClient()
	req := Request{Model: "local-bge-small", Input: "test local inference"}
	resp, err := c.Embed(context.Background(), srv.URL+"/v1", "local-bge-small", "", req, 384)
	if err != nil {
		t.Fatalf("Embed local: %v", err)
	}
	if len(resp.Embedding) != 384 {
		t.Errorf("embedding length = %d; want 384", len(resp.Embedding))
	}
	if resp.Model != "local-bge-small" {
		t.Errorf("model = %q; want local-bge-small", resp.Model)
	}
}

// NewClient — and real metrics path (observeCall / observeTokens non-nil)

func TestNewClient_ReturnsNonNil(t *testing.T) {
	c := NewClient(&http.Client{}, silentLogger(), "test_new_client_non_nil")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
}

// TestClient_RealMetrics_ObserveCallAndTokens exercises the non-nil paths
// in observeCall and observeTokens by running a successful Embed call
// through a NewClient (which initialises a real Metrics).
func TestClient_RealMetrics_ObserveCallAndTokens(t *testing.T) {
	floats := []float32{0.1, 0.2}
	srv := httptest.NewServer(embeddingHandler(http.StatusOK, floatEmbeddingBody(floats, 7)))
	defer srv.Close()

	// Use a unique Prometheus namespace per test to avoid duplicate registration.
	c := NewClient(&http.Client{Timeout: 5 * time.Second}, silentLogger(), "test_metrics_real_1")
	req := Request{Model: "m", Input: "x"}
	resp, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 0)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Embedding) != 2 {
		t.Errorf("embedding len = %d; want 2", len(resp.Embedding))
	}
}

// TestClient_RealMetrics_Error_ObserveProviderError exercises the
// provider_error outcome path in observeCall (non-nil metrics).
func TestClient_RealMetrics_Error_ObserveProviderError(t *testing.T) {
	srv := httptest.NewServer(embeddingHandler(http.StatusInternalServerError, `{"error":{"message":"err"}}`))
	defer srv.Close()

	c := NewClient(&http.Client{Timeout: 5 * time.Second}, silentLogger(), "test_metrics_real_2")
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestClient_RealMetrics_DimMismatch exercises the dim_mismatch outcome
// path in observeCall (non-nil metrics).
func TestClient_RealMetrics_DimMismatch_ObservesDimMismatch(t *testing.T) {
	floats := []float32{0.1, 0.2}
	srv := httptest.NewServer(embeddingHandler(http.StatusOK, floatEmbeddingBody(floats, 3)))
	defer srv.Close()

	c := NewClient(&http.Client{Timeout: 5 * time.Second}, silentLogger(), "test_metrics_real_3")
	req := Request{Model: "m", Input: "x"}
	_, err := c.Embed(context.Background(), srv.URL, "m", "k", req, 1536)
	if !errors.Is(err, ErrEmbeddingDimMismatch) {
		t.Errorf("err = %v; want ErrEmbeddingDimMismatch", err)
	}
}
