package forwardheader

import (
	"net/http"
	"strings"
)

// Apply copies the request-side allowlisted headers from src into dst for the
// given adapter-type formatSlug, returning the low-cardinality bucket label of
// every header it DROPPED (for optional metric attribution by the caller).
//
// It is the request-direction analogue of [FilterResponseHeaders]: a free
// function so a caller that builds an outbound request by hand — the STT
// streaming-proxy forward, which constructs a fresh multipart request rather
// than going through the spec adapter — single-sources the exact same
// whitelist-by-omission filter the chat adapter's forwardHeaders applies,
// instead of hand-rolling a second, drift-prone allowlist. Anything not on the
// resolved request allowlist for formatSlug is omitted, so credential / framing
// / Nexus-internal headers (the hard denylist enforced at [Resolve]) can never
// forward even if a client sends them.
//
// A nil Resolved falls back to the embedded [Default] allowlist. dst header
// keys are added with http.Header.Add so multi-valued client headers survive.
func Apply(dst, src http.Header, resolved *Resolved, formatSlug string) []string {
	if len(src) == 0 {
		return nil
	}
	if resolved == nil {
		resolved = Default()
	}
	allowed := resolved.Request(formatSlug)
	var dropped []string
	for k, vs := range src {
		lk := strings.ToLower(k)
		if _, ok := allowed[lk]; !ok {
			dropped = append(dropped, BucketDroppedHeader(lk))
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	return dropped
}
