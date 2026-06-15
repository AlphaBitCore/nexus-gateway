// Package m365copilotweb implements the m365-copilot-web traffic
// adapter for Microsoft 365 Copilot — the in-suite Copilot embedded in
// Word / Excel / PowerPoint / Outlook / Teams. Hosted under
// m365.cloud.microsoft and reachable via various *.office.com paths.
//
// Distinct from copilot.microsoft.com (the consumer Copilot product,
// handled by copilot-ms-web). The two share the same upstream backend
// (Sydney) but route through different surfaces; this adapter
// delegates to copilot-ms-web for parsing while attributing traffic to
// the M365 surface separately for audit.
//
// Convention: every vendor / surface gets its own adapter ID
// even when the wire is shared.
package m365copilotweb

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/web/copilotmsweb"
)

const adapterID = "m365-copilot-web"

// Adapter delegates to copilot-ms-web with a different Provider label.
type Adapter struct {
	inner copilotmsweb.Adapter
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
// "m365-copilot-web" so audit distinguishes M365 Copilot traffic from
// consumer copilot.microsoft.com.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "m365-copilot-web"
	return meta
}

func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}
