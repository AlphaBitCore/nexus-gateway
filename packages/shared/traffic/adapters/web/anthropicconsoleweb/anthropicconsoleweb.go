// Package anthropicconsoleweb implements the anthropic-console-web
// traffic adapter for browser-side traffic to console.anthropic.com
// (Anthropic's developer console — workbench, settings, dashboard).
//
// Distinct from:
//   - api.anthropic.com (handled by anthropic; programmatic API)
//   - claude.ai (handled by claude-web; consumer chat)
//
// The console workbench exercises the public Anthropic Messages API
// directly with developer Bearer tokens, so the adapter delegates to
// the anthropic adapter for parsing while attributing traffic to the
// developer-console surface separately.
//
// Convention (provider-adapter-architecture.md §3a): every vendor / surface
// gets its own adapter ID even when the wire protocol is shared.
package anthropicconsoleweb

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
)

const adapterID = "anthropic-console-web"

// Adapter delegates to the anthropic adapter with a different Provider label.
type Adapter struct {
	inner anthropic.Adapter
}

func (a *Adapter) ID() string                         { return adapterID }
func (a *Adapter) Configure(cfg map[string]any) error { return a.inner.Configure(cfg) }

func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractRequest(ctx, body, path)
}

func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractResponse(ctx, body, path)
}

func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractStreamChunk(ctx, chunk, path)
}

func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteRequestBody(ctx, body, path, content)
}

func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteResponseBody(ctx, body, path, content)
}

// DetectRequestMeta delegates to the anthropic adapter then re-labels
// Provider as "anthropic-console-web" so audit distinguishes
// console / workbench traffic from direct API traffic.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "anthropic-console-web"
	return meta
}

// DetectResponseUsage delegates — workbench responses use the standard
// Anthropic usage block.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}
