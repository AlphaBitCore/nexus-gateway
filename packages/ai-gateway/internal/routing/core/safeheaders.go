package core

import (
	"net/http"
	"strings"
)

// SafeHeaders is the routing-visible projection of an inbound HTTP
// header set. It enforces a structural trust boundary between the
// gateway's external surface (whatever the client sent) and its
// internal routing state (canonical request payload, identity,
// endpoint, retry plan, …): the type has no exported Set / Add /
// raw-map accessor, so internal code cannot smuggle data into a
// routing strategy through this container.
//
// Auth-bearing headers (Authorization, Cookie, X-API-Key) are dropped
// at construction so they never reach matchConditions predicate
// evaluation or trace logs. The deny list is centralised in
// blockedHeaders below.
//
// The type has no exported Set / Add / raw-map accessor: internal code
// cannot smuggle data into a routing strategy through this container.
// This makes a class of auth-credential leak unrepresentable at the
// type level.
type SafeHeaders struct {
	h http.Header // unexported; never returned by reference
}

// blockedHeaders names the inbound HTTP headers that must never reach
// the routing layer. Matched case-insensitively against the input keys
// at NewSafeHeaders construction time.
//
// Operator predicates that need to vary on authentication state should
// look at RoutingContext.VirtualKey (typed identity), not raw auth
// headers. If a future header carries something operationally
// load-bearing but auth-derived, it must be projected into a typed
// field on RoutingContext rather than allow-listed here.
var blockedHeaders = map[string]struct{}{
	"authorization": {},
	"cookie":        {},
	"x-api-key":     {},
}

// NewSafeHeaders builds a SafeHeaders view of the inbound headers.
// Returns the zero value when h is nil or empty (no allocation). The
// returned SafeHeaders retains a copy of the deny-list-filtered keys
// internally; the input map is not mutated and may be modified by the
// caller after construction.
func NewSafeHeaders(h http.Header) SafeHeaders {
	if len(h) == 0 {
		return SafeHeaders{}
	}
	filtered := make(http.Header, len(h))
	for k, v := range h {
		if _, blocked := blockedHeaders[strings.ToLower(k)]; blocked {
			continue
		}
		filtered[k] = v
	}
	return SafeHeaders{h: filtered}
}

// Get returns the first value associated with name. Lookup is
// case-insensitive (net/http convention). Returns "" when name is
// absent, when name was deny-listed at construction, or on a zero
// SafeHeaders.
func (s SafeHeaders) Get(name string) string {
	if s.h == nil {
		return ""
	}
	return s.h.Get(name)
}
