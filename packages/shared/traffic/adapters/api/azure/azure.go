// Package azure implements the traffic adapter for Azure OpenAI Service.
// The wire format is identical to OpenAI; the only difference is URL path
// structure. This adapter remaps Azure paths to OpenAI paths and delegates
// all extraction to the openai-compat adapter.
package azure

import (
	"context"
	"regexp"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// azureDeploymentPattern matches Azure OpenAI deployment paths and captures
// the trailing endpoint (e.g. "chat/completions", "embeddings").
var azureDeploymentPattern = regexp.MustCompile(
	`/openai/deployments/[^/]+/(.+?)(?:\?|$)`,
)

// Adapter implements the Azure OpenAI content extraction by delegating
// to the openai-compat adapter after path remapping.
type Adapter struct {
	inner openai.Adapter
}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "azure-openai" }

// Configure delegates to the inner openai adapter.
func (a *Adapter) Configure(config map[string]any) error {
	return a.inner.Configure(config)
}

// ExtractRequest remaps the Azure path and delegates to the openai adapter.
func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractRequest(ctx, body, remapPath(path))
}

// ExtractResponse remaps the Azure path and delegates to the openai adapter.
func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractResponse(ctx, body, remapPath(path))
}

// ExtractStreamChunk delegates directly to the openai adapter (streaming
// format is path-independent in the openai adapter).
func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractStreamChunk(ctx, chunk, remapPath(path))
}

// RewriteRequestBody remaps the Azure path and delegates to the openai
// adapter so the shared chat/completions and responses rewriters handle
// both wire formats identically.
func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteRequestBody(ctx, body, remapPath(path), content)
}

// RewriteResponseBody delegates to the inner openai adapter with remapped path.
func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteResponseBody(ctx, body, remapPath(path), content)
}

// remapPath converts Azure-style deployment paths to OpenAI-style paths.
// e.g. "/openai/deployments/gpt4/chat/completions?api-version=2024-02-01"
//
//	→ "/v1/chat/completions"
func remapPath(path string) string {
	m := azureDeploymentPattern.FindStringSubmatch(path)
	if len(m) < 2 {
		return path // pass through as-is; the inner adapter will handle unknown paths
	}
	return "/v1/" + m[1]
}
