// Package vertex implements the traffic adapter for Google Vertex AI.
// Vertex publishes third-party models (Anthropic, AI21, Mistral) alongside
// Google's own Gemini family. The URL structure is
//
//	/v1/projects/<project>/locations/<region>/publishers/<publisher>/models/<model>:<method>
//
// Anthropic-on-Vertex uses Anthropic Messages-format bodies; Gemini uses
// the native Gemini format. We dispatch on the `publishers/<publisher>`
// segment at content-extraction time.
//
// Authentication is Google OAuth — `Authorization: Bearer ya29.<opaque>`
// — which we classify as `gcp-oauth` and fingerprint.
package vertex

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/gemini"
)

// vertexPublisherPattern captures the publisher (anthropic|google|mistral|…)
// and model id from a Vertex path.
var vertexPublisherPattern = regexp.MustCompile(`/publishers/([^/]+)/models/([^:/?]+)`)

// Adapter dispatches to the per-publisher sub-adapter for content and
// handles Vertex-specific authentication inside DetectRequestMeta.
type Adapter struct {
	anthropic anthropic.Adapter
	gemini    gemini.Adapter
}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "vertex" }

// Configure is a no-op.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest dispatches based on the publisher in the URL path.
// Unknown publishers return ErrUnknownSchema.
func (a *Adapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	switch publisherFromPath(path) {
	case "anthropic":
		return a.anthropic.ExtractRequest(ctx, body, path)
	case "google":
		return a.gemini.ExtractRequest(ctx, body, path)
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractResponse dispatches based on publisher.
func (a *Adapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	switch publisherFromPath(path) {
	case "anthropic":
		return a.anthropic.ExtractResponse(ctx, body, path)
	case "google":
		return a.gemini.ExtractResponse(ctx, body, path)
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// ExtractStreamChunk dispatches based on publisher.
func (a *Adapter) ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (traffic.NormalizedContent, error) {
	switch publisherFromPath(path) {
	case "anthropic":
		return a.anthropic.ExtractStreamChunk(ctx, chunk, path)
	case "google":
		return a.gemini.ExtractStreamChunk(ctx, chunk, path)
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// RewriteRequestBody dispatches to the per-publisher rewriter. Unknown
// publishers surface ErrRewriteUnsupported so the caller falls back to
// forwarding the original body.
func (a *Adapter) RewriteRequestBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	switch publisherFromPath(path) {
	case "anthropic":
		return a.anthropic.RewriteRequestBody(ctx, body, path, content)
	case "google":
		return a.gemini.RewriteRequestBody(ctx, body, path, content)
	}
	return nil, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody dispatches to the per-publisher response rewriter.
func (a *Adapter) RewriteResponseBody(ctx context.Context, body []byte, path string, content traffic.NormalizedContent) ([]byte, int, error) {
	switch publisherFromPath(path) {
	case "anthropic":
		return a.anthropic.RewriteResponseBody(ctx, body, path, content)
	case "google":
		return a.gemini.RewriteResponseBody(ctx, body, path, content)
	}
	return nil, 0, traffic.ErrRewriteUnsupported
}

// DetectRequestMeta extracts provider="vertex", model + publisher-qualified
// prefix from URL, and classifies the OAuth bearer token.
func (a *Adapter) DetectRequestMeta(r *http.Request, _ []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "vertex"}
	if r != nil {
		meta.Path = r.URL.Path
		publisher, model := publisherAndModel(r.URL.Path)
		if publisher != "" && model != "" {
			// Namespace the model with the publisher so "anthropic/claude-3-5-sonnet"
			// and "google/gemini-1.5-pro" are unambiguous downstream.
			meta.Model = publisher + "/" + model
		}
		if tok := traffic.ExtractBearerToken(r); tok != "" {
			meta.ApiKeyClass = "gcp-oauth"
			meta.ApiKeyFingerprint = traffic.ApiKeyFingerprint(tok)
		}
	}
	return meta
}

// DetectResponseUsage dispatches to the per-publisher detector using the
// request-path publisher segment (retrieved via r.Request.URL.Path).
func (a *Adapter) DetectResponseUsage(r *http.Response, body []byte) traffic.UsageMeta {
	if r == nil || r.Request == nil {
		if len(body) == 0 {
			return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
		}
		return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
	}
	switch publisherFromPath(r.Request.URL.Path) {
	case "anthropic":
		return a.anthropic.DetectResponseUsage(r, body)
	case "google":
		return a.gemini.DetectResponseUsage(r, body)
	}
	if len(body) == 0 {
		return traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
	}
	return traffic.UsageMeta{Status: traffic.UsageStatusParseFailed}
}

// publisherFromPath returns the publisher segment ("anthropic", "google", …).
// Returns "" when the path does not match the Vertex pattern.
func publisherFromPath(path string) string {
	m := vertexPublisherPattern.FindStringSubmatch(path)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// publisherAndModel returns both the publisher and the model id.
func publisherAndModel(path string) (string, string) {
	m := vertexPublisherPattern.FindStringSubmatch(path)
	if len(m) < 3 {
		return "", ""
	}
	return strings.ToLower(m[1]), m[2]
}
