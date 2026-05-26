package traffic

import (
	"net/http"
	"net/textproto"
	"strings"
)

// HeaderVia is the canonical name of the via-chain marker.
const HeaderVia = "X-Nexus-Via"

// PrependVia adds svc to the front of the X-Nexus-Via chain. If svc already
// appears anywhere in the existing chain (case-insensitive comma-split match),
// the header is left unchanged.
func PrependVia(h http.Header, svc string) {
	cur := h.Get(HeaderVia)
	if cur == "" {
		h.Set(HeaderVia, svc)
		return
	}
	for _, part := range strings.Split(cur, ",") {
		if strings.EqualFold(strings.TrimSpace(part), svc) {
			return
		}
	}
	h.Set(HeaderVia, svc+", "+cur)
}

// PrependChain prepends value to a comma-separated marker chain header.
// Unlike PrependVia, this helper preserves duplicates (each hop is allowed
// to repeat the same value) and distinguishes "header absent" from "header
// present but empty" so 1:1 alignment with X-Nexus-Via is preserved when
// some hops have a value to report for the marker and others do not.
//
// Semantics (matches the X-Nexus-Hook + X-Nexus-Mode contract documented in
// nexus-response-markers.md):
//   - First hop (header absent on entry): h.Set(key, value)
//   - Later hop (header present on entry, even if value is ""): prepend
//     value + ", " + existing so the new value lands at position 0 and the
//     existing chain shifts right by one.
//
// The "header present but empty" case is what lets an inner hop reserve a
// position in the chain without claiming a value (e.g. AI Gateway has no
// Mode concept; it stamps an empty X-Nexus-Mode so outer hops still align
// 1:1 with the X-Nexus-Via chain when they prepend their own mode).
func PrependChain(h http.Header, key, value string) {
	canonical := textproto.CanonicalMIMEHeaderKey(key)
	existing, present := h[canonical]
	if !present {
		h.Set(key, value)
		return
	}
	cur := ""
	if len(existing) > 0 {
		cur = existing[0]
	}
	h.Set(key, value+", "+cur)
}

// HeaderExpose is the standard CORS header name.
const HeaderExpose = "Access-Control-Expose-Headers"

// ExposeHeaders is the canonical list of Nexus marker headers exposed to
// browser-side JS via Access-Control-Expose-Headers. Build the CORS value
// from this slice — never hand-maintain the list elsewhere.
// Documented in docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md.
var ExposeHeaders = []string{
	// Cross-service correlation + chain. X-Nexus-Request-Id is the single
	// canonical correlation ID; there is no separate trace ID header.
	"X-Nexus-Via",
	"X-Nexus-Request-Id",
	// AI Gateway — outcome
	"X-Nexus-Cache",
	"X-Nexus-Routed-Model",
	"X-Nexus-Routed-Provider",
	"X-Nexus-Attempts",
	"X-Nexus-Coerced",
	// AI Gateway — quota
	"X-Nexus-Quota-Used",
	"X-Nexus-Quota-Limit",
	"X-Nexus-Quota-Downgrade",
	"X-Nexus-Quota-Original-Model",
	"X-Nexus-Quota-Warning",
	// Per-hop chain markers (1:1 indexed with X-Nexus-Via via PrependChain)
	"X-Nexus-Hook",
	"X-Nexus-Mode",
	// Compliance-proxy interception-domain rule UUID (CP-only; no chain semantics)
	"X-Nexus-Domain-Rule",
	// HTTP standard timing (RFC 8674)
	"Server-Timing",
	// Attestation — request-only in production paths; in the response allowlist
	// for reverse-proxy edge cases that surface the request header back.
	"X-Nexus-Attestation",
}

// SetExposeHeaders writes the full Nexus marker list to
// Access-Control-Expose-Headers, replacing any existing value. Use this on
// synthetic responses (reject path, AI Gateway origin responses) where there
// is no upstream CORS state to merge with.
func SetExposeHeaders(h http.Header) {
	h.Set(HeaderExpose, strings.Join(ExposeHeaders, ", "))
}

// MergeExposeHeaders appends the given marker names to any existing
// Access-Control-Expose-Headers value, deduping case-insensitively. Use this
// on the transparent-proxy success path (CP / Agent) where an upstream may
// have already set its own expose list.
func MergeExposeHeaders(h http.Header, names ...string) {
	cur := h.Get(HeaderExpose)
	seen := map[string]struct{}{}
	var out []string
	if cur != "" {
		for _, p := range strings.Split(cur, ",") {
			t := strings.TrimSpace(p)
			if t == "" {
				continue
			}
			seen[strings.ToLower(t)] = struct{}{}
			out = append(out, t)
		}
	}
	for _, n := range names {
		if _, ok := seen[strings.ToLower(n)]; ok {
			continue
		}
		seen[strings.ToLower(n)] = struct{}{}
		out = append(out, n)
	}
	h.Set(HeaderExpose, strings.Join(out, ", "))
}

// HookOutcomeInput is the data needed to render an X-Nexus-Hook value.
// Pass Rejected non-empty to indicate a reject (its name + RejectReason are
// the sole fields used; Passed/Transformed are ignored).
type HookOutcomeInput struct {
	Passed       []string // ordered list of hook slugs that ran (and passed)
	Transformed  bool     // true if at least one passed hook modified the body
	Rejected     string   // hook slug that rejected the pipeline; empty when none
	RejectReason string   // reason slug from the rejecting hook (free text accepted; sanitized here)
}

// FormatHookOutcome renders the X-Nexus-Hook value for a single hop:
//   - "none" when no hook ran
//   - "passed:a,b" all passed, no transform
//   - "transformed:a,b" all passed, at least one modified the body
//   - "rejected:<hook>:<reason-slug>" pipeline rejected; reason is sanitized
//     to [a-z0-9-]+ (unsafe characters dropped) to prevent header/log injection.
//
// Multi-hop assembly is the caller's job via PrependChain — the outer hop
// prepends its FormatHookOutcome to the existing X-Nexus-Hook chain so the
// final value reads innermost-last (same 1:1 alignment as X-Nexus-Via).
func FormatHookOutcome(in HookOutcomeInput) string {
	if in.Rejected != "" {
		reason := sanitizeSlug(in.RejectReason)
		return "rejected:" + in.Rejected + ":" + reason
	}
	if len(in.Passed) == 0 {
		return "none"
	}
	prefix := "passed:"
	if in.Transformed {
		prefix = "transformed:"
	}
	return prefix + strings.Join(in.Passed, ",")
}

// sanitizeSlug lowercases s and keeps only [a-z0-9-]; non-allowed runs collapse
// into a single hyphen. Empty input or all-stripped input returns "unknown".
func sanitizeSlug(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteRune('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}
