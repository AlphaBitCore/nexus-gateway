// Package perplexity implements the perplexity traffic adapter for
// Perplexity Sonar's OpenAI-compatible API at api.perplexity.ai.
//
// Per session policy: every vendor / surface gets its own adapter ID
// even when the wire is shared. The adapter delegates to openai-compat
// for parsing while attributing traffic to "perplexity" for audit.
package perplexity

import (
	"context"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

const adapterID = "perplexity"

// Adapter delegates to openai-compat with a different Provider label.
type Adapter struct {
	inner openai.Adapter
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

// DetectRequestMeta delegates then re-labels Provider as "perplexity".
// Perplexity keys typically begin with `pplx-` and travel as Bearer;
// classify them under the "perplexity-bearer" key class.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "perplexity"
	if r != nil {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimSpace(auth[len("Bearer "):])
			if tok != "" {
				meta.ApiKeyClass = "perplexity-bearer"
				meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
			}
		}
	}
	return meta
}

func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}
