package embeddings

// no streaming — embeddings endpoint is request/response only.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client issues HTTP calls to an OpenAI-compatible /v1/embeddings
// endpoint using the existing provider infrastructure (base URL + API
// key coming from the Provider/Credential rows).
//
// Retries and singleflight deduplication are the caller's concern
// (callers wrap this client in a golang.org/x/sync/singleflight keyed by
// SHA-256(input)).
//
// Cross-provider batch embeddings (used by the proxy's /v1/embeddings
// ingress path) route through the canonical adapter path inside
// providers/specs/*/codec/embeddings.go — this client stays on the L2
// cache hot path only.
type Client struct {
	http    *http.Client
	log     *slog.Logger
	metrics *Metrics
}

// NewClient constructs an embedding Client.
//   - httpClient should be the shared upstream singleton from specutil
//     (specutil.NewHTTPClient()) so the admin-configurable transport
//     tunables apply here too.
//   - log is the service-level slog.Logger (passed by the caller's
//     constructor, never global).
//   - namespace is the Prometheus namespace string (e.g. "nexus");
//     passed to NewMetrics.
func NewClient(httpClient *http.Client, log *slog.Logger, namespace string) *Client {
	return &Client{
		http:    httpClient,
		log:     log,
		metrics: NewMetrics(namespace),
	}
}

// Embed calls providerBaseURL/v1/embeddings with the given request and
// returns the parsed Response.
//
// Arguments:
//   - providerBaseURL: the base URL of the provider row, e.g.
//     "https://api.openai.com" or "http://localhost:9001/v1". A
//     trailing "/v1" is trimmed so the path is always appended as
//     "/v1/embeddings".
//   - model: the provider model ID to include in the request body.
//   - apiKey: the decrypted API key from the Credential row; passed as
//     "Authorization: Bearer <apiKey>". Empty key omits the header
//     (local servers may not require auth).
//   - req: the embedding request.
//   - expectedDim: if > 0, Embed verifies that len(resp.Embedding) ==
//     expectedDim and returns ErrEmbeddingDimMismatch on mismatch. Pass
//     0 to skip the check.
//
// Named errors: ErrEmbeddingTimeout, ErrEmbeddingProviderError,
// ErrEmbeddingDimMismatch.
func (c *Client) Embed(ctx context.Context, providerBaseURL, model, apiKey string, req Request, expectedDim int) (Response, error) {
	provider := labelFromURL(providerBaseURL)
	start := time.Now()

	body, err := EncodeOpenAIRequest(req)
	if err != nil {
		return Response{}, fmt.Errorf("embeddings: encode request: %w", err)
	}

	url := buildEmbeddingsURL(providerBaseURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("embeddings: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.http.Do(httpReq)
	latency := time.Since(start).Seconds()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			c.metrics.observeCall(provider, model, "timeout", latency)
			c.log.Warn("embeddings: request timed out",
				"provider", provider, "model", model,
				"latency_ms", int(latency*1000), "error", err)
			return Response{}, ErrEmbeddingTimeout
		}
		c.metrics.observeCall(provider, model, "provider_error", latency)
		c.log.Error("embeddings: HTTP transport error",
			"provider", provider, "model", model, "error", err)
		return Response{}, fmt.Errorf("%w: transport error: %w", ErrEmbeddingProviderError, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		c.metrics.observeCall(provider, model, "provider_error", latency)
		return Response{}, fmt.Errorf("%w: read body: %w", ErrEmbeddingProviderError, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := openAIEmbeddingError(respBody)
		c.metrics.observeCall(provider, model, "provider_error", latency)
		c.log.Warn("embeddings: provider returned non-2xx",
			"provider", provider, "model", model,
			"status", resp.StatusCode, "detail", detail)
		return Response{}, fmt.Errorf("%w: HTTP %d: %s", ErrEmbeddingProviderError, resp.StatusCode, detail)
	}

	result, err := DecodeOpenAIResponse(respBody)
	if err != nil {
		c.metrics.observeCall(provider, model, "provider_error", latency)
		return Response{}, fmt.Errorf("embeddings: decode response: %w", err)
	}

	if expectedDim > 0 && len(result.Embedding) != expectedDim {
		c.metrics.observeCall(provider, model, "dim_mismatch", latency)
		c.log.Warn("embeddings: dimension mismatch",
			"provider", provider, "model", model,
			"expected", expectedDim, "got", len(result.Embedding))
		return Response{}, fmt.Errorf("%w: expected %d, got %d",
			ErrEmbeddingDimMismatch, expectedDim, len(result.Embedding))
	}

	c.metrics.observeCall(provider, model, "success", latency)
	c.metrics.observeTokens(provider, model, result.PromptTokens)
	return result, nil
}

// buildEmbeddingsURL appends /v1/embeddings to baseURL, normalising
// any trailing slash or pre-existing "/v1" suffix so the full path is
// always "<base>/v1/embeddings" regardless of how the admin entered the
// provider base URL.
func buildEmbeddingsURL(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	// If the admin stored the base URL with "/v1" already (common for
	// local-inference servers that expose only one API version), strip it
	// so we don't produce "<host>/v1/v1/embeddings".
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/embeddings"
}

// labelFromURL extracts a short provider label for Prometheus labels.
// It returns the hostname (without port) so labels stay readable even
// when providerBaseURL carries different paths.
func labelFromURL(rawURL string) string {
	// Find the host part after the scheme.
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Strip path and port.
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "unknown"
	}
	return s
}
