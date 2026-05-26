// Package googleaistudioweb implements the google-aistudio-web traffic
// adapter for browser-side traffic to aistudio.google.com (Google AI
// Studio — the Gemini API developer playground / experiment surface).
//
// Distinct from:
//   - generativelanguage.googleapis.com (handled by gemini; programmatic API)
//   - gemini.google.com (handled by gemini-web; consumer chat)
//   - *-aiplatform.googleapis.com (handled by vertex; Vertex AI)
//
// AI Studio's playground exercises the public Gemini API via JSON
// requests, so the adapter delegates to the gemini adapter for
// content extraction while attributing traffic to the AI-Studio
// surface separately for audit.
//
// Per the session policy: every vendor / surface gets its own adapter
// ID even when the wire is shared.
package googleaistudioweb

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/gemini"
)

const adapterID = "google-aistudio-web"

// Adapter delegates to the gemini adapter with a different Provider label.
type Adapter struct {
	inner gemini.Adapter
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

// DetectRequestMeta delegates then re-labels Provider as
// "google-aistudio-web" so audit distinguishes AI Studio playground
// traffic from direct Gemini API traffic.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "google-aistudio-web"
	return meta
}

// DetectResponseUsage delegates — AI Studio playground responses use
// the standard Gemini usage block.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}
