package requestcontext

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// RequestContext is the L3 artefact carried through the ai-gateway request
// pipeline. Construct it via NewBuilder().…Build(); treat the returned
// pointer as read-only.
//
// All fields are unexported. Consumers read via getters which are
// nil-receiver-safe so callers can use the type at zero-value sites
// without nil-checks.
type RequestContext struct {
	identity   *vkauth.VKMeta
	normalized *normcore.NormalizedPayload
	endpoint   string
	headers    http.Header
	rawBody    []byte
}

// Identity returns the authenticated virtual-key metadata for this
// request. Returns nil when the receiver is nil or no identity was
// attached (e.g. requests that authenticate downstream).
func (rc *RequestContext) Identity() *vkauth.VKMeta {
	if rc == nil {
		return nil
	}
	return rc.identity
}

// Normalized returns the canonical normalized payload for this request,
// produced by exactly one Registry.Normalize call. Returns nil when the
// receiver is nil or normalize was not attached (e.g. body was empty, or
// normalize failed and the handler chose to elide the payload).
func (rc *RequestContext) Normalized() *normcore.NormalizedPayload {
	if rc == nil {
		return nil
	}
	return rc.normalized
}

// Endpoint returns the endpoint family for this request
// ("chat/completions", "embeddings", "models", …). Returns "" on a nil
// receiver or when no endpoint was attached.
func (rc *RequestContext) Endpoint() string {
	if rc == nil {
		return ""
	}
	return rc.endpoint
}

// Headers returns the inbound HTTP headers. The returned map is the
// reference supplied to Builder.WithHeaders; callers must not mutate it.
// Returns nil on a nil receiver.
//
// S3 will replace this getter with a typed SafeHeaders boundary that
// exposes only a whitelisted HeaderName enum; consumers depending on the
// raw http.Header shape must migrate at the same time.
func (rc *RequestContext) Headers() http.Header {
	if rc == nil {
		return nil
	}
	return rc.headers
}

// RawBody returns the raw request body bytes. The returned slice is the
// reference supplied to Builder.WithRawBody; callers must not mutate it.
// Returns nil on a nil receiver.
//
// Strategies must not read from this — it exists for audit / spill /
// passthrough only. Use Normalized() for content-aware decisions.
func (rc *RequestContext) RawBody() []byte {
	if rc == nil {
		return nil
	}
	return rc.rawBody
}

// Builder constructs a RequestContext fluently. Use NewBuilder() to
// obtain a fresh Builder; chain With… setters; call Build() to obtain
// the populated pointer.
type Builder struct {
	rc *RequestContext
}

// NewBuilder returns a fresh Builder over an empty RequestContext.
func NewBuilder() *Builder {
	return &Builder{rc: &RequestContext{}}
}

// WithIdentity attaches the authenticated VK metadata.
func (b *Builder) WithIdentity(v *vkauth.VKMeta) *Builder {
	b.rc.identity = v
	return b
}

// WithNormalized attaches the canonical normalized payload produced by
// Registry.Normalize.
func (b *Builder) WithNormalized(p *normcore.NormalizedPayload) *Builder {
	b.rc.normalized = p
	return b
}

// WithEndpoint attaches the resolved endpoint family ("chat/completions",
// "embeddings", …).
func (b *Builder) WithEndpoint(e string) *Builder {
	b.rc.endpoint = e
	return b
}

// WithHeaders attaches the inbound HTTP header map. The Builder retains
// the reference; the caller must not subsequently mutate the map.
func (b *Builder) WithHeaders(h http.Header) *Builder {
	b.rc.headers = h
	return b
}

// WithRawBody attaches the raw request body bytes. The Builder retains
// the reference; the caller must not subsequently mutate the slice.
func (b *Builder) WithRawBody(body []byte) *Builder {
	b.rc.rawBody = body
	return b
}

// Build returns the populated RequestContext. Build is a one-shot
// factory: subsequent calls return the same pointer. Callers wanting an
// independent context must construct a new Builder.
func (b *Builder) Build() *RequestContext {
	return b.rc
}
