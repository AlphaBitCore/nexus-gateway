package requestcontext

import (
	"net/http"
	"sync"

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
	identity *vkauth.VKMeta
	endpoint string
	headers  http.Header
	rawBody  []byte

	// Lazy canonical seam. The request canonical is a pure derivative of the
	// raw body; computing it eagerly on every request is the dominant hot-path
	// allocator when no consumer needs it. It is therefore computed on first
	// Normalized() call from normalizeFn and memoized. The only request-time
	// consumers are smart routing and the response cache; both pull it via
	// Normalized(). The audit writer must NOT trigger the compute — it reads
	// NormalizedIfComputed() and defers to view-time when nothing materialized
	// it (lazy-audit-normalize). WithNormalized(p) pins an already-computed
	// payload (eager path / tests): normalizeFn nil + computed true.
	mu          sync.Mutex
	normalizeFn func() *normcore.NormalizedPayload
	normalized  *normcore.NormalizedPayload
	computed    bool
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
// computing it on first call from the raw body (lazy seam) and memoizing the
// result. Returns nil when the receiver is nil, no seam was attached (empty
// body), or normalize failed (the compute fn elides the payload). Safe for
// concurrent callers. Triggering this is what materializes the canonical — call
// it only when a consumer genuinely needs the canonical (smart routing, cache);
// the audit path must use NormalizedIfComputed instead.
func (rc *RequestContext) Normalized() *normcore.NormalizedPayload {
	if rc == nil {
		return nil
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if !rc.computed {
		if rc.normalizeFn != nil {
			rc.normalized = rc.normalizeFn()
		}
		rc.computed = true
	}
	return rc.normalized
}

// NormalizedIfComputed returns the memoized canonical and whether it was already
// materialized, WITHOUT triggering the lazy compute. The audit writer uses this
// to reuse the canonical iff a request-time consumer (smart routing / cache)
// already produced it; when ok is false the audit direction is deferred to
// view-time recompute (lazy-audit-normalize) rather than computing it on the
// hot path. Nil-safe.
func (rc *RequestContext) NormalizedIfComputed() (*normcore.NormalizedPayload, bool) {
	if rc == nil {
		return nil, false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.normalized, rc.computed && rc.normalized != nil
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
// A future typed SafeHeaders boundary may replace this getter, exposing
// only a whitelisted HeaderName enum; consumers depending on the
// raw http.Header shape would migrate at the same time.
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

// WithNormalized pins an already-computed canonical payload (the eager path and
// tests). Marks the context computed so Normalized() returns p without invoking
// any lazy seam.
func (b *Builder) WithNormalized(p *normcore.NormalizedPayload) *Builder {
	b.rc.normalized = p
	b.rc.computed = true
	return b
}

// WithLazyNormalize installs the lazy compute seam: fn is invoked at most once,
// on the first Normalized() call, and its result memoized. Use this on the
// request hot path so the canonical is produced only if a consumer pulls it.
// fn must be safe to call once; returning nil elides the canonical.
func (b *Builder) WithLazyNormalize(fn func() *normcore.NormalizedPayload) *Builder {
	b.rc.normalizeFn = fn
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
