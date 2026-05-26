// Package deepseek implements the traffic adapter for the DeepSeek API.
// DeepSeek uses the OpenAI-compatible wire format, so all content extraction
// delegates to the openai adapter. The only DeepSeek-specific piece is
// the provider label emitted by DetectRequestMeta.
package deepseek

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// Adapter wraps the openai adapter and rewrites the emitted provider label.
type Adapter struct {
	inner openai.Adapter
}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "deepseek" }

// Configure delegates to the inner openai adapter.
func (a *Adapter) Configure(config map[string]any) error { return a.inner.Configure(config) }

// ExtractRequest delegates to the inner openai adapter.
func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractRequest(ctx, body, path)
}

// ExtractResponse delegates to the inner openai adapter.
func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractResponse(ctx, body, path)
}

// ExtractStreamChunk delegates to the inner openai adapter.
func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractStreamChunk(ctx, chunk, path)
}

// RewriteRequestBody delegates to the inner openai adapter.
func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteRequestBody(ctx, body, path, content)
}

// RewriteResponseBody delegates to the inner openai adapter.
func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteResponseBody(ctx, body, path, content)
}

// DetectRequestMeta reuses the openai detector and rewrites Provider.
// DeepSeek uses Authorization: Bearer sk-<tenant>... keys which match the
// "sk-" class through ApiKeyClassify.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "deepseek"
	return meta
}

// DetectResponseUsage delegates to the inner openai adapter.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}
