// clienttags.go — extraction of the caller-declared tag bag.
//
// A sibling product in the Nexus family sometimes knows something about a
// request that the gateway has no opinion about — which of its own tenants
// the traffic belongs to, what its own admission layer decided before
// forwarding. The tags carry those facts onto traffic_event.details so that
// product can read its own decisions back out of gateway traffic.
//
// Same trust model as the end-user tag: caller-asserted, never validated,
// never resolved against Nexus identities, and never fed into routing,
// quota, hooks, caching, IAM, or metrics. Storage only. A forged value can
// only misattribute the caller's own traffic, which stays inside their
// virtual-key scope.
//
// Deliberately NOT a dedicated column per fact. A column would bind this
// gateway's schema to one consumer's vocabulary and would cost a new MQ
// field id plus a consumer change for every fact added; details is already
// a JSONB column travelling as a single blob, so a new tag costs nothing
// downstream.
package proxy

import (
	"net/http"
	"strings"
)

// clientTagsHeader is the only carrier. Header-only for the same reason as
// the end-user tag: no wire protocol carries a field that means "facts the
// calling product asserts about this request", and reading one out of a
// provider-addressed body would file traffic under something nobody chose
// for this purpose.
const clientTagsHeader = "X-Nexus-Client-Tags"

// clientTagsMaxPairs caps how many tags one request can add. traffic_event
// takes the gateway's full ingest rate, so this ceiling — together with the
// per-value cap — is what stops a caller widening every row without bound.
// Eight is comfortably above the two facts the first consumer sends and far
// below anything that would matter to row size.
const clientTagsMaxPairs = 8

// clientTagsMaxHeaderBytes bounds parse WORK, not just stored size: Split
// allocates one substring per comma over the FULL input before any per-pair
// cap ever applies, so an uncapped header lets an authenticated caller
// inside their own RPM allowance burn CPU and GC churn against the shared
// gateway process for zero stored tags. Same reasoning as authenticating
// before the body read (stage_admission.go phase 1): bound the expensive
// step before doing it, not after. Sized as clientTagsMaxPairs pairs of
// "key=value" at the maximum key (32) + '=' (1) + value (endUserMaxBytes)
// width, PLUS the (clientTagsMaxPairs-1) commas joining them — the fully
// legal, fully-packed header must fit inside the bound, or a caller sending
// exactly the documented maximum shape would lose data to truncation
// instead of hitting a cap that only bites past-legal input.
const clientTagsMaxHeaderBytes = clientTagsMaxPairs*(32+1+endUserMaxBytes) + (clientTagsMaxPairs - 1) // 2319

// extractClientTags parses the caller's tag bag. Returns nil — not an empty
// map — when the header is absent or nothing in it survives parsing, so the
// details object omits the key entirely and existing rows stay byte-identical.
//
// Malformed input degrades per pair, never per request: one bad pair must not
// discard the good ones beside it, because the caller cannot see which pair
// the gateway rejected. Duplicate keys: last wins.
func extractClientTags(h http.Header) map[string]string {
	raw := strings.TrimSpace(h.Get(clientTagsHeader))
	if raw == "" {
		return nil
	}
	// Truncate BEFORE Split, not after: bounding the stored result alone still
	// pays the full parse cost on an oversized header. A trailing partial pair
	// left by the cut degrades exactly like any other malformed pair (dropped;
	// its siblings before the cut still parse).
	if len(raw) > clientTagsMaxHeaderBytes {
		raw = raw[:clientTagsMaxHeaderBytes]
	}
	var tags map[string]string
	for _, part := range strings.Split(raw, ",") {
		// SplitN(2): a value may legitimately contain "=", so only the
		// first separator delimits.
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		if !validClientTagKey(key) {
			continue
		}
		if tags == nil {
			tags = make(map[string]string, clientTagsMaxPairs)
		}
		// The cap counts distinct keys, so a repeated key overwriting an
		// existing entry does not consume a slot.
		if _, exists := tags[key]; !exists && len(tags) >= clientTagsMaxPairs {
			continue
		}
		// capEndUser and its endUserMaxBytes cap are SHARED with the end-user
		// tag (enduser.go) — there is no separate client-tags cap. Raising
		// endUserMaxBytes to widen the end-user tag silently widens every
		// client-tag value by the same delta too, up to clientTagsMaxPairs
		// (8x) over on a single row.
		tags[key] = capEndUser(strings.TrimSpace(kv[1]))
	}
	return tags
}

// validClientTagKey accepts lower-case snake_case only. The keys land as
// JSONB object keys that analytics queries address by name, so a permissive
// charset would invite two spellings of the same fact.
func validClientTagKey(key string) bool {
	if key == "" || len(key) > 32 {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}
