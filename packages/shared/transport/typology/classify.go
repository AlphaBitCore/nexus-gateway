package typology

import "strings"

// Rule is one classification rule. ClassifyPath iterates the built-in
// rule table in registration order; the first match wins. Both Method
// and PathPattern must match for the rule to fire.
//
// PathPattern supports glob matching where "*" matches any run of
// non-slash characters within the same path segment. "**" is not
// supported. Examples:
//   - "/v1/chat/completions"                              — exact
//   - "/openai/deployments/*/chat/completions"           — wildcard segment
//   - "/v1*/projects/*/locations/*/publishers/*/models/*:embedContent"
//     — multi-wildcard with the trailing ":embedContent" literal suffix
type Rule struct {
	Method      string // POST | GET; case-insensitive comparison
	PathPattern string // glob; see globMatch semantics
	Kind        EndpointKind
	Shape       WireShape
}

// ClassifyPath maps an HTTP (method, path) pair to its canonical
// (EndpointKind, WireShape) pair using the built-in rule table.
//
// Returns (_, _, false) when no rule matches; callers treat this as
// "unclassified" — the request still flows, but consumers that need
// classification (hook filter, audit persistence) record empty values
// and degrade gracefully.
//
// Method comparison is case-insensitive. PathPattern matching uses
// the single-segment glob semantics described on [Rule].
//
// This is the only path → typology mapping function in the tree.
// Every consumer — AI Gateway dispatch, Compliance Proxy forward
// handler, Agent intercept handler, hook pipeline filter, audit
// persistence, routing rule matcher — calls this function.
func ClassifyPath(method, path string) (EndpointKind, WireShape, bool) {
	for _, r := range defaultRules {
		if r.Method != "" && !equalFold(r.Method, method) {
			continue
		}
		if !matchPath(r.PathPattern, path) {
			continue
		}
		return r.Kind, r.Shape, true
	}
	return "", WireShapeNone, false
}

// matchPath reports whether path matches the glob pattern.
func matchPath(pattern, path string) bool {
	if pattern == path {
		return true
	}
	return globMatch(pattern, path)
}

// globMatch implements a simplified glob where "*" matches any run of
// non-separator characters within the same path segment. The separator
// is "/". "**" is not supported.
//
// Single source of truth for the path-rule glob matcher used by
// ClassifyPath. The pre-E87 shared/traffic/classify package's matcher
// was deleted in E87-S3a-1; this is now the only implementation.
func globMatch(pattern, s string) bool {
	if len(pattern) == 0 {
		return s == ""
	}
	starIdx := strings.IndexByte(pattern, '*')
	if starIdx < 0 {
		return pattern == s
	}
	prefix := pattern[:starIdx]
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	s = s[len(prefix):]
	pattern = pattern[starIdx+1:]

	if len(pattern) == 0 {
		// Trailing star: matches any remaining non-empty sequence
		// that does not cross a segment boundary.
		return !strings.Contains(s, "/") || s == ""
	}

	// The star is greedy within the segment only (stops at '/').
	nextChar := pattern[0]
	for len(s) > 0 {
		if s[0] == '/' && nextChar != '/' {
			break
		}
		if globMatch(pattern, s) {
			return true
		}
		s = s[1:]
	}
	// Last chance: zero-length star match.
	return globMatch(pattern, s)
}

// equalFold is the ASCII case-insensitive string compare used for HTTP
// method matching. Allocates nothing; equivalent to strings.EqualFold
// for the constrained inputs.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca |= 0x20
		}
		if cb >= 'A' && cb <= 'Z' {
			cb |= 0x20
		}
		if ca != cb {
			return false
		}
	}
	return true
}
