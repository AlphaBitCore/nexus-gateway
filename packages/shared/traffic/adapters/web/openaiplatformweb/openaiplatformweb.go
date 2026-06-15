// Package openaiplatformweb implements the openai-platform-web traffic
// adapter for browser-side traffic to platform.openai.com (the OpenAI
// developer console — playground, dashboard, settings).
//
// Distinct from:
//   - api.openai.com (handled by openai-compat)
//   - chatgpt.com (handled by chatgpt-web; consumer ChatGPT)
//
// platform.openai.com hosts the OpenAI playground which exercises the
// public API directly with developer Bearer tokens, plus dashboard
// management endpoints. Most chat-completion playground requests use
// the same wire format as api.openai.com, so the adapter delegates to
// openai-compat after recognising the body shape.
//
// Convention: even though the wire is OpenAI-compat,
// platform.openai.com gets its own adapter ID so audit can attribute
// traffic to the developer-console surface separately.
package openaiplatformweb

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

const adapterID = "openai-platform-web"

// Adapter delegates to openai-compat with a different Provider label.
type Adapter struct {
	inner openai.Adapter
}

func (a *Adapter) ID() string                         { return adapterID }
func (a *Adapter) Configure(cfg map[string]any) error { return a.inner.Configure(cfg) }

// ExtractRequest delegates after normalising path. The playground
// posts to /v1/chat/completions, /v1/responses, /v1/embeddings same
// as the public API.
func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractRequest(ctx, body, normalisePath(path))
}

func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractResponse(ctx, body, normalisePath(path))
}

func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractStreamChunk(ctx, chunk, normalisePath(path))
}

func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteRequestBody(ctx, body, normalisePath(path), content)
}

func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteResponseBody(ctx, body, normalisePath(path), content)
}

// DetectRequestMeta sets Provider="openai-platform-web" so audit
// distinguishes developer-console playground traffic from API traffic.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "openai-platform-web"
	return meta
}

// DetectResponseUsage delegates — playground responses include usage
// the same as direct API calls.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}

// normalisePath strips any platform.openai.com-specific prefix so the
// inner adapter's path-based routing works. Currently the playground
// already uses standard /v1/* paths, so this is identity.
func normalisePath(path string) string { return path }
