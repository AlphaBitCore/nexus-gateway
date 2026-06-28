package dispatch

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
)

// effectiveAllowlist returns the adapter's wired allowlist, or the
// package-level embedded default when a caller (typically a test) did
// not wire one. The returned pointer is process-stable; callers must
// not mutate it.
//
// Accept-Encoding is permanently excluded from the request-side
// allowlist. Forwarding it to the upstream disables Go
// net/http.Transport's transparent gzip auto-decompression, which
// breaks Anthropic streaming SSE responses — see
// https://pkg.go.dev/net/http#Transport. The hard denylist enforced
// at config load (forwardheader.Resolve) makes it impossible for an
// operator's YAML to re-add `accept-encoding`.
func (a *specAdapter) effectiveAllowlist() *forwardheader.Resolved {
	// Live atomic snapshot wins; the forwardHeaders allowlist is
	// resolved once from yaml at boot and never rewritten thereafter.
	// The construction-time pointer is retained only as a fallback for
	// tests that haven't seeded forwardheader.SetActive.
	if live := forwardheader.Active(); live != nil {
		return live
	}
	if a.allowlist != nil {
		return a.allowlist
	}
	return forwardheader.Default()
}

// forwardHeaders copies allowlisted client headers from src onto the
// outbound dst request. Anything not on the resolved request-side
// allowlist is dropped and counted against
// nexus_forward_header_dropped_total.
//
// The allowlist is precomputed per Format at config load
// (forwardheader.Resolve), so no per-request map allocation happens here.
func (a *specAdapter) forwardHeaders(dst *http.Request, src http.Header) {
	if len(src) == 0 {
		return
	}
	allowed := a.effectiveAllowlist().Request(string(a.spec.Format))
	adapterLabel := string(a.spec.Format)
	for k, vs := range src {
		lk := canonicalLower(k)
		if _, ok := allowed[lk]; !ok {
			emitForwardHeaderDrop("request", adapterLabel, forwardheader.BucketDroppedHeader(lk))
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
}

// FilterResponseHeaders is a free function (not a method on Adapter)
// that returns a new http.Header containing only the upstream
// response headers permitted by the resolved response allowlist for
// the supplied Format. Per-request headers (e.g. x-request-id,
// rate-limit headers) are dropped on cache HIT — replaying a stale
// per-request value is worse than not surfacing it, since clients
// would correlate to a request that never happened.
//
// allowlist may be nil; callers get the embedded defaults via
// [forwardheader.Default]. format selects the per-adapter-type
// extension; passing an unknown Format returns just the base set.
//
// Headers not on either Static or PerRequest are dropped silently
// and counted against
// nexus_forward_header_dropped_total{direction="response"}.
//
// Kept as a free function so the handler does not need to type-assert
// on Adapter or pull the method through the interface (which would
// force every test mock of Adapter to grow it).
func FilterResponseHeaders(allowlist *forwardheader.Resolved, format Format, src http.Header, isCacheHit bool) http.Header {
	out := make(http.Header)
	if len(src) == 0 {
		return out
	}
	if allowlist == nil {
		allowlist = forwardheader.Default()
	}
	set := allowlist.Response(string(format))
	adapterLabel := string(format)
	for k, vs := range src {
		lk := canonicalLower(k)
		if _, ok := set.Static[lk]; ok {
			for _, v := range vs {
				out.Add(k, v)
			}
			continue
		}
		if _, ok := set.PerRequest[lk]; ok {
			if isCacheHit {
				// Strip on cache hit; replaying a stale per-request value
				// (request id, ratelimit-remaining, processing-ms) lies to
				// the client.
				continue
			}
			for _, v := range vs {
				out.Add(k, v)
			}
			continue
		}
		emitForwardHeaderDrop("response", adapterLabel, forwardheader.BucketDroppedHeader(lk))
	}
	return out
}

// canonicalLower returns the lower-cased canonical form of an HTTP
// header name. Reserved as a single chokepoint so future name
// normalization (e.g. tightening for non-ASCII inputs) lands here.
func canonicalLower(name string) string {
	// http.CanonicalMIMEHeaderKey already canonicalizes ASCII case;
	// lower-casing it once gives a stable key that matches the
	// pre-lowered allowlist. textproto would do the same, but the
	// stdlib already canonicalizes header keys on Header.Get/Set.
	b := make([]byte, len(name))
	for i := range len(name) {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
