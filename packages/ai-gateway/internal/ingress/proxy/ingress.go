// Package handler — ingress.go defines the per-route Ingress descriptor
// and the context plumbing that carries it through the proxy pipeline.
//
// Every /v1/* and native ingress route mounts `ProxyHandler.ServeProxy`
// with an [Ingress] value that declares the canonical endpoint kind,
// the wire body format a client will send, and whether the route is a
// streaming surface. Detection is path-authoritative: the decorator
// stamps the Ingress on the request context before the pipeline reads
// the body, so downstream stages (VK extract, model extract, routing,
// format-compat check, executor dispatch) never guess.
//
// `x-nexus-aigw-body-format` is honoured as an explicit override only on
// the OpenAI-compat family (`/v1/chat/completions`, `/v1/embeddings`),
// and only when it names a registered [provcore.Format].
package proxy

import (
	"context"
	"net/http"
	"strings"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Ingress captures the detected shape of an inbound client request.
// It is populated at route registration time and stamped on the
// request context by [Handler.ServeProxy]; downstream stages read it
// from context via [IngressFromContext].
type Ingress struct {
	// WireShape is the canonical wire-shape tag for this route
	// (openai-chat, openai-embeddings, anthropic-messages, …). Drives
	// adapter dispatch + routing match + cache shape-keying.
	WireShape typology.WireShape

	// BodyFormat is the wire format a well-behaved client is expected
	// to send on this route. "openai" for the OpenAI-compat family;
	// "anthropic" for `/v1/messages`; etc. Populated by the route
	// table in main.go.
	BodyFormat provcore.Format

	// Stream is a path-scoped hint (Gemini's `:streamGenerateContent`
	// is always streaming, `:generateContent` is always non-streaming).
	// For body-carrying formats (openai, anthropic, minimax, glm,
	// azure) the authoritative signal comes from the JSON body's
	// `stream: true` field; for those formats Stream here is always
	// false and the extractor reads the body.
	Stream bool

	// StreamFromPath is true when the route path itself is streaming
	// (Gemini `:streamGenerateContent`). In that case the body's
	// `stream` field is ignored.
	StreamFromPath bool
}

// ingressCtxKey is a private type for request-scoped Ingress storage.
type ingressCtxKey struct{}

// WithIngress returns a child context carrying in. Used by
// [Handler.ServeProxy] before dispatching to the pipeline.
func WithIngress(ctx context.Context, in Ingress) context.Context {
	return context.WithValue(ctx, ingressCtxKey{}, in)
}

// IngressFromContext returns the Ingress stamped on ctx by
// [WithIngress], or the zero value with ok=false when none is present.
// Pipeline stages that run outside ServeProxy (e.g. the
// `/internal/routing-simulate` endpoint) default to the zero value.
func IngressFromContext(ctx context.Context) (Ingress, bool) {
	if v, ok := ctx.Value(ingressCtxKey{}).(Ingress); ok {
		return v, true
	}
	return Ingress{}, false
}

// geminiCacheInvalidateCtxKey is the private context key for the
// per-request Gemini-cache invalidation callback. Set by ServeProxy
// when a HIT injects a cachedContent reference; read by the response
// path (handleNonStream / stream code) to invalidate the Redis entry
// when the upstream reports the cache no longer exists.
type geminiCacheInvalidateCtxKey struct{}

func withGeminiCacheInvalidate(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, geminiCacheInvalidateCtxKey{}, fn)
}

// GeminiCacheInvalidateFromContext returns the per-request invalidation
// callback, or nil when none was set (no HIT, no Gemini target, or
// Gemini cache disabled).
func GeminiCacheInvalidateFromContext(ctx context.Context) func() {
	if v, ok := ctx.Value(geminiCacheInvalidateCtxKey{}).(func()); ok {
		return v
	}
	return nil
}

// responsesUpgradeCtxKey carries the auto-upgrade decision through the
// request context. When set to true on the ctx, the handler:
//   - has rewritten the upstream body via openai.EncodeResponsesRequest
//   - has flipped req.WireShape/BodyFormat to EndpointResponsesAPI/FormatOpenAIResponses
//     so the spec_openai adapter dispatches to POST /v1/responses
//   - MUST re-encode the upstream response back to chat-completions shape
//     before writing to the client (non-stream: DecodeResponsesResponse +
//     chat-completions envelope; stream: a Responses→chat-completions
//     transcoder substituted in canonicalbridge.NewStreamTranscoder)
type responsesUpgradeCtxKey struct{}

// WithResponsesUpgrade returns a child context flagged for the
// auto-upgrade post-processing. Set exactly once at routing/dispatch
// time; never toggled mid-request.
func WithResponsesUpgrade(ctx context.Context) context.Context {
	return context.WithValue(ctx, responsesUpgradeCtxKey{}, true)
}

// ResponsesUpgradeFromContext reports whether the auto-upgrade fired
// for this request.
func ResponsesUpgradeFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(responsesUpgradeCtxKey{}).(bool)
	return v
}

// streamHitOriginCtxKey carries the cache entry's origin ingress shape
// onto the stream-HIT pipeline so the chunkSSEReader can pick the right
// transcoder when the cached chunks were written under a different
// ingress than the current request (cross-ingress shape contamination
// fix). When unset (live MISS / non-stream HIT / non-stream
// joiner), the standard (ingress, target) transcoder selection runs.
type streamHitOriginCtxKey struct{}

// StreamHitOrigin captures the cache entry's origin ingress shape.
// Stored on the request context by [Handler.handleStreamHit] when the
// entry is tagged with OriginWireShape; absent for legacy untagged
// entries (the reader falls back to the standard transcoder selection
// which preserves the legacy behavior).
type StreamHitOrigin struct {
	WireShape typology.WireShape
}

// WithStreamHitOrigin stamps the cache entry's origin shape onto ctx so
// downstream stream-HIT plumbing (handleStreamWithSubscription) can
// override the default transcoder selection.
func WithStreamHitOrigin(ctx context.Context, origin StreamHitOrigin) context.Context {
	return context.WithValue(ctx, streamHitOriginCtxKey{}, origin)
}

// StreamHitOriginFromContext returns the stamped origin and ok=true,
// or the zero value and ok=false when no override was stored.
func StreamHitOriginFromContext(ctx context.Context) (StreamHitOrigin, bool) {
	v, ok := ctx.Value(streamHitOriginCtxKey{}).(StreamHitOrigin)
	return v, ok
}

// stickyKeyCtxKey is a private context key for per-request sticky-key storage.
type stickyKeyCtxKey struct{}

// withStickyKey returns a child context carrying the virtual-key ID used for
// consistent credential pool hashing.
func withStickyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, stickyKeyCtxKey{}, key)
}

// stickyKeyFromCtx returns the sticky key stamped by [withStickyKey], or ""
// when absent.
func stickyKeyFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(stickyKeyCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// WireShapeToBodyFormat maps a wire shape back to its provcore.Format
// adapter-family identifier. This is the structural bridge between the
// cache-entry wire-shape tag (per-request precision) and the SSE encoder
// dispatch in canonicalbridge.NewStreamTranscoder (per-Format-family
// grammar selection — see api.go for the asymmetry rationale).
//
// Returns (Format, ok=false) for wire shapes that are not yet routed
// through Format-keyed SSE transcoder construction (Gemini, Vertex,
// Bedrock, Cohere, Voyage). Callers MUST check ok and skip the dependent
// operation when the mapping is undefined — silently returning the empty
// Format would drive downstream dispatch to an unconfigured codec.
func WireShapeToBodyFormat(w typology.WireShape) (provcore.Format, bool) {
	switch w {
	case typology.WireShapeOpenAIChat,
		typology.WireShapeOpenAICompletionsLegacy,
		typology.WireShapeOpenAIEmbeddings:
		return provcore.FormatOpenAI, true
	case typology.WireShapeOpenAIResponses:
		return provcore.FormatOpenAIResponses, true
	case typology.WireShapeAnthropicMessages:
		return provcore.FormatAnthropic, true
	}
	return provcore.Format(""), false
}

// applyHeaderOverride returns a copy of in with BodyFormat overridden by
// a valid `x-nexus-aigw-body-format` request header when in.BodyFormat ==
// openai. Any other value is ignored (path-based detection is
// authoritative on native surfaces). A non-empty header that names an
// unknown format returns ok=false so the caller can reject the request.
func (in Ingress) applyHeaderOverride(r *http.Request) (Ingress, bool) {
	if in.BodyFormat != provcore.FormatOpenAI {
		return in, true
	}
	raw := strings.TrimSpace(r.Header.Get("x-nexus-aigw-body-format"))
	if raw == "" {
		return in, true
	}
	f := provcore.Format(strings.ToLower(raw))
	if !f.Valid() {
		return in, false
	}
	in.BodyFormat = f
	return in, true
}
