// Package githubcopilot implements the github-copilot traffic adapter
// for traffic to api.githubcopilot.com and the legacy
// copilot-proxy.githubusercontent.com proxy. GitHub Copilot Chat
// (the IDE-side conversational surface) uses an OpenAI-compatible wire
// format under `/chat/completions`, so the adapter delegates content
// extraction to openai-compat after path normalisation. Provider is
// reported as "github-copilot" so traffic_event.provider_name
// disambiguates Copilot traffic from direct OpenAI API traffic.
//
// Endpoints we recognise:
//   - POST /chat/completions                        — Copilot Chat
//   - POST /v1/chat/completions                     — newer alias
//   - POST /v1/engines/copilot-codex/completions    — legacy completion
//     endpoint (autocomplete; minimal audit value but covered)
//
// Authentication is a GitHub-issued Bearer token. The presented form is
// typically `Bearer tid_<jwt>` (Copilot session token) or
// `Bearer gho_<token>` / `ghs_<token>` (GitHub OAuth / app tokens).
// We classify the key class accordingly and fingerprint it.
package githubcopilot

import (
	"context"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

const adapterID = "github-copilot"

// Adapter wraps the openai-compat adapter with Copilot-specific path
// normalisation and provider attribution.
type Adapter struct {
	inner openai.Adapter
}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure delegates to the inner adapter.
func (a *Adapter) Configure(cfg map[string]any) error { return a.inner.Configure(cfg) }

// ExtractRequest delegates to openai-compat after normalising the
// path. Copilot's `/chat/completions` and `/v1/chat/completions` both
// map to `/v1/chat/completions` so the inner adapter routes correctly.
func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractRequest(ctx, body, normalisePath(path))
}

// ExtractResponse delegates to openai-compat.
func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractResponse(ctx, body, normalisePath(path))
}

// ExtractStreamChunk delegates to openai-compat.
func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	return a.inner.ExtractStreamChunk(ctx, chunk, normalisePath(path))
}

// RewriteRequestBody delegates to openai-compat. Copilot Chat bodies
// are OpenAI-compatible so the rewrite walk is identical.
func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteRequestBody(ctx, body, normalisePath(path), content)
}

// RewriteResponseBody delegates to openai-compat.
func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	return a.inner.RewriteResponseBody(ctx, body, normalisePath(path), content)
}

// DetectRequestMeta sets Provider="github-copilot" and classifies the
// Bearer token by GitHub-issued prefix. Falls through to OpenAI-style
// `sk-` classes if the wire format is somehow upstream OpenAI keys.
func (a *Adapter) DetectRequestMeta(r *http.Request, body []byte) traffic.RequestMeta {
	meta := a.inner.DetectRequestMeta(r, body)
	meta.Provider = "github-copilot"
	// Re-classify the API key by GitHub-issued prefix; openai's
	// detector defaults to "sk-" which would mis-attribute Copilot
	// traffic.
	if r != nil {
		auth := r.Header.Get("Authorization")
		if cls := classifyGitHubKey(auth); cls != "" {
			meta.ApiKeyClass = cls
			meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(extractBearer(auth))
		}
	}
	return meta
}

// DetectResponseUsage delegates to openai-compat — Copilot Chat
// responses include the standard OpenAI usage block when the client
// requests stream_options.include_usage or for non-streaming calls.
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	return a.inner.DetectResponseUsage(r, body)
}

// normalisePath maps Copilot endpoints to canonical OpenAI paths so
// the inner openai-compat adapter routes correctly.
func normalisePath(path string) string {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return "/v1/chat/completions"
	case strings.Contains(path, "/embeddings"):
		return "/v1/embeddings"
	case strings.Contains(path, "/completions"):
		// Legacy Codex completion endpoint. The inner adapter does
		// not parse the legacy `/completions` shape; the path is
		// passed through unchanged so the inner returns ErrUnknownSchema
		// and the proxy applies its unmatched_action policy.
		return path
	default:
		return path
	}
}

// classifyGitHubKey returns a stable label for the GitHub-issued
// token prefix or "" when the header is not Bearer-shaped.
func classifyGitHubKey(authHeader string) string {
	tok := extractBearer(authHeader)
	if tok == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(tok, "tid_"):
		return "github-copilot-tid"
	case strings.HasPrefix(tok, "gho_"):
		return "github-oauth"
	case strings.HasPrefix(tok, "ghs_"):
		return "github-app"
	case strings.HasPrefix(tok, "ghu_"):
		return "github-user-to-server"
	case strings.HasPrefix(tok, "ghp_"):
		return "github-personal-access"
	case strings.HasPrefix(tok, "github_pat_"):
		return "github-fine-grained-pat"
	}
	return ""
}

// extractBearer pulls the token out of an "Authorization: Bearer <tok>"
// header. Returns "" when the header is absent or malformed.
func extractBearer(authHeader string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	return strings.TrimSpace(authHeader[len(prefix):])
}
