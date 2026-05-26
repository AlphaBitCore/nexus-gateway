// Package glm implements the traffic adapter for Zhipu AI's GLM API.
// GLM uses the OpenAI-compatible wire format. Authentication is a JWT
// signed with an api-key pair (`<id>.<secret>`); the header is
// `Authorization: Bearer <jwt>`. For fingerprint purposes we hash the
// full JWT — the client-side id portion is embedded in the token's
// `api_key` claim but extracting it would require parsing the JWT.
package glm

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// Adapter wraps the openai adapter.
type Adapter struct {
	inner openai.Adapter
}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "glm" }

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

// RewriteRequestBody delegates to the inner openai adapter — GLM reuses
// the OpenAI chat/completions and responses wire formats.
func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteRequestBody(ctx, body, path, content)
}

// RewriteResponseBody delegates to the inner openai adapter.
func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteResponseBody(ctx, body, path, content)
}

// DetectRequestMeta extracts model from body, provider label, and classifies
// the JWT Bearer token as "glm-jwt".
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "glm"
	if tok := traffic.ExtractBearerToken(r); tok != "" {
		meta.ApiKeyClass = "glm-jwt"
		meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
	}
	return meta
}

// DetectResponseUsage delegates to the inner openai adapter; GLM follows the
// same {usage.prompt_tokens,completion_tokens} shape.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}
