package traffic

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// Adapter is the capability bundle for one AI provider format.
// Instantiated once per InterceptionDomain at configcache load time;
// hot-swapped atomically via DomainSnapshot.
//
// Adapters MUST be safe for concurrent use — one instance is shared across
// all requests routed to the matching domain.
type Adapter interface {
	// ID returns the unique adapter identifier (must match InterceptionDomain.adapterId).
	ID() string

	// Configure applies adapter-specific config from InterceptionDomain.adapterConfig.
	// Called once at instantiation. Returns error if config is invalid.
	Configure(config map[string]any) error

	// ExtractRequest parses a provider-specific request body into NormalizedContent.
	// path is the HTTP request path (influences parsing for multi-endpoint adapters).
	// Error values: ErrUnknownSchema, ErrMalformed, ErrPartial.
	ExtractRequest(ctx context.Context, body []byte, path string) (NormalizedContent, error)

	// ExtractResponse parses a provider-specific response body.
	ExtractResponse(ctx context.Context, body []byte, path string) (NormalizedContent, error)

	// ExtractStreamChunk parses a single SSE delta chunk.
	// Returns only the new delta (not accumulated). Accumulation is the pipeline's job.
	ExtractStreamChunk(ctx context.Context, chunk []byte, path string) (NormalizedContent, error)

	// DetectRequestMeta extracts LLM signals (provider, model, api-key class
	// and fingerprint) from an intercepted request. Never returns an error:
	// failure yields empty-string fields. See RequestMeta.
	DetectRequestMeta(r *http.Request, body []byte) RequestMeta

	// DetectResponseUsage extracts token counts from a non-streaming response.
	// For streaming responses the caller consults the streaming accumulator
	// (see packages/shared/transport/streaming) instead.
	DetectResponseUsage(r *http.Response, body []byte) UsageMeta

	// RewriteRequestBody replaces the text segments inside a
	// provider-specific request body with the supplied NormalizedContent
	// — the reverse of ExtractRequest. The caller uses this to push
	// hook-modified content (e.g. PII-redacted text) back onto the wire
	// before forwarding upstream.
	//
	// Adapters MUST walk their schema in the same order that
	// ExtractRequest emitted segments, so position i in
	// content.Segments pairs with the i-th extractable text slot.
	// Returns:
	//   - the rewritten body bytes (identical structure, only text
	//     fields overwritten)
	//   - the number of content slots actually overwritten
	//   - ErrRewriteUnsupported when the adapter cannot reverse-encode
	//     its wire format (callers fall back to forwarding the original
	//     body plus a warn log rather than failing the request).
	//
	// Other errors (including ErrMalformed and ErrUnknownSchema) are
	// propagated and should fail the request — by the time Rewrite is
	// called the body has already passed Extract, so a malformed or
	// unknown-schema body here represents an internal inconsistency.
	RewriteRequestBody(ctx context.Context, body []byte, path string, content NormalizedContent) ([]byte, int, error)

	// RewriteResponseBody replaces text segments inside a provider-specific
	// non-streaming response body with the supplied NormalizedContent — the
	// reverse of ExtractResponse. Used when response-stage hooks return
	// Modify (e.g. PII redaction) before the body is returned to the client.
	//
	// Semantics match RewriteRequestBody: slot order must mirror
	// ExtractResponse; ErrRewriteUnsupported means forward the upstream body
	// unchanged with a warn log.
	RewriteResponseBody(ctx context.Context, body []byte, path string, content NormalizedContent) ([]byte, int, error)
}

// AdapterFactory creates a new Adapter instance. Called once per
// InterceptionDomain when building a DomainSnapshot.
type AdapterFactory func() Adapter

// AdapterRegistry maps adapterId → AdapterFactory. Built-in adapters register
// via Register() at startup. The registry is read-only after startup (D8).
type AdapterRegistry struct {
	mu        sync.RWMutex
	factories map[string]AdapterFactory
	frozen    bool
	namespace string
}

// NewAdapterRegistry creates a new registry. namespace is used for metrics
// prefixing per §2.5 (e.g. "nexus_compliance_proxy").
func NewAdapterRegistry(namespace string) *AdapterRegistry {
	return &AdapterRegistry{
		factories: make(map[string]AdapterFactory),
		namespace: namespace,
	}
}

// Register adds an adapter factory. Returns an error if called after Freeze().
func (r *AdapterRegistry) Register(id string, factory AdapterFactory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("traffic: cannot register adapter %q after registry is frozen", id)
	}
	r.factories[id] = factory
	return nil
}

// Freeze prevents further registrations. Called after startup init.
func (r *AdapterRegistry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Get returns the factory for the given adapter ID, or nil if not found.
func (r *AdapterRegistry) Get(id string) AdapterFactory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.factories[id]
}

// All returns all registered adapter IDs.
func (r *AdapterRegistry) All() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.factories))
	for id := range r.factories {
		ids = append(ids, id)
	}
	return ids
}

// Namespace returns the metrics namespace for this registry.
func (r *AdapterRegistry) Namespace() string { return r.namespace }
