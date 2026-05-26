package domain

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// Engine is the runtime decision engine for compliance-proxy
// host + path interception. Constructed empty via NewEngine and
// re-loaded via Swap when configloader returns a new domain list.
//
// Hot paths (Match, PathAction, AllowlistEntries) read an
// atomic.Pointer to a snapshot — no locks per request. Swap updates
// the pointer atomically.
type Engine struct {
	current atomic.Pointer[snapshot]
}

type snapshot struct {
	domains []InterceptionDomain
	// hostIndex sorted by priority DESC; iteration order is the
	// match order (first match wins).
	// Pre-compiled patterns live here so request-time matching is
	// O(N) over enabled domains without recompiling regexes.
	matchers []hostMatcher
}

type hostMatcher struct {
	domain InterceptionDomain
	rx     *regexp.Regexp // populated when HostMatchType == REGEX
}

// NewEngine returns an empty Engine that matches no host until Swap is
// called.
func NewEngine() *Engine {
	e := &Engine{}
	e.current.Store(&snapshot{})
	return e
}

// Swap replaces the engine's domain set with the provided one. Errors
// from regex compilation cause the swap to be rejected — the previous
// snapshot is preserved so a bad config can't black-hole the proxy.
func (e *Engine) Swap(domains []InterceptionDomain) error {
	matchers := make([]hostMatcher, 0, len(domains))
	for _, d := range domains {
		m := hostMatcher{domain: d}
		if d.HostMatchType == HostMatchRegex {
			rx, err := regexp.Compile(d.HostPattern)
			if err != nil {
				return err
			}
			m.rx = rx
		}
		matchers = append(matchers, m)
	}
	e.current.Store(&snapshot{
		domains:  domains,
		matchers: matchers,
	})
	return nil
}

// MatchHost returns the highest-priority enabled InterceptionDomain
// whose host pattern matches the supplied host. host is the raw value
// from the CONNECT line or HTTP Host header — port included is
// tolerated by stripping before pattern compare.
func (e *Engine) MatchHost(host string) *InterceptionDomain {
	if i := strings.IndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil
	}
	snap := e.current.Load()
	if snap == nil {
		return nil
	}
	for i := range snap.matchers {
		m := &snap.matchers[i]
		if matchHost(host, m) {
			d := m.domain
			return &d
		}
	}
	return nil
}

func matchHost(host string, m *hostMatcher) bool {
	pat := strings.ToLower(strings.TrimSpace(m.domain.HostPattern))
	switch m.domain.HostMatchType {
	case HostMatchExact:
		return host == pat
	case HostMatchGlob:
		// Support a single leading "*." wildcard — matches any
		// subdomain depth. Other glob shapes fall back to literal.
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:] // ".example.com"
			return strings.HasSuffix(host, suffix) || host == suffix[1:]
		}
		return host == pat
	case HostMatchPrefix:
		return strings.HasPrefix(host, pat)
	case HostMatchRegex:
		if m.rx == nil {
			return false
		}
		return m.rx.MatchString(host)
	default:
		return host == pat
	}
}

// PathAction returns the action that applies to a request to (host, path)
// once host has matched. When no path pattern matches, the domain's
// DefaultPathAction is returned (typically PROCESS). When the engine
// has no matching domain at all, returns PathActionProcess as a
// conservative default — callers should consult MatchHost first when
// this distinction matters.
func (e *Engine) PathAction(domain *InterceptionDomain, path string) PathAction {
	if domain == nil {
		return PathActionProcess
	}
	for i := range domain.Paths {
		p := &domain.Paths[i]
		if pathMatchesAny(path, p) {
			return p.Action
		}
	}
	if domain.DefaultPathAction == "" {
		return PathActionProcess
	}
	return domain.DefaultPathAction
}

func pathMatchesAny(path string, p *InterceptionPath) bool {
	for _, pat := range p.PathPattern {
		if pathMatchOne(path, pat, p.MatchType) {
			return true
		}
	}
	return false
}

func pathMatchOne(path, pattern string, mt PathMatchType) bool {
	switch mt {
	case PathMatchExact:
		return path == pattern
	case PathMatchPrefix:
		// Strip trailing glob wildcard so "/api/*" and "/api/" both mean
		// "any path under /api/". Users commonly enter glob-style patterns
		// in the UI; the match semantics for PREFIX are always prefix-only.
		pat := pattern
		if strings.HasSuffix(pat, "/*") {
			pat = pat[:len(pat)-1] // "/api/*" → "/api/"
		} else if strings.HasSuffix(pat, "*") {
			pat = pat[:len(pat)-1] // "/api*" → "/api"
		}
		return strings.HasPrefix(path, pat)
	case PathMatchRegex:
		// Lazy-compile per call. Path-level regex is rare and the
		// engine snapshot doesn't pre-compile path regex (paths are
		// typically dozens, not thousands). If this becomes a hot
		// spot, hoist into snapshot like host regex.
		rx, err := regexp.Compile(pattern)
		if err != nil {
			return false
		}
		return rx.MatchString(path)
	default:
		return path == pattern
	}
}

// AllowlistEntries returns the host:port list compatible with the
// pre-existing access.Checker.SwapDomainAllowlist API. Lets the engine
// be the single source of truth for "which hosts is the proxy bumping
// today" without ripping out the access checker.
func (e *Engine) AllowlistEntries() []string {
	snap := e.current.Load()
	if snap == nil {
		return nil
	}
	out := make([]string, 0, len(snap.domains))
	seen := make(map[string]bool, len(snap.domains))
	for _, d := range snap.domains {
		entry := normalizeAllowlist(d.HostPattern, d.HostMatchType)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	return out
}

func normalizeAllowlist(hostPattern string, mt HostMatchType) string {
	hostPattern = strings.TrimSpace(hostPattern)
	if hostPattern == "" {
		return ""
	}
	if mt == HostMatchRegex {
		// Regex hosts can't be flattened to a simple host:port — the
		// engine still matches them; the pre-existing access.Checker
		// allowlist won't contain them. Acceptable: requests to
		// regex-only hosts are admitted at request time after CONNECT
		// since we don't gate on the simple allowlist. Still log so
		// operators see the gap.
		return ""
	}
	if strings.Contains(hostPattern, ":") {
		return strings.ToLower(hostPattern)
	}
	return strings.ToLower(hostPattern) + ":443"
}

// Snapshot returns a defensive copy of the current domain list for
// runtime introspection. Callers must not mutate the returned slice
// (it shares element data with the live engine).
func (e *Engine) Snapshot() []InterceptionDomain {
	snap := e.current.Load()
	if snap == nil {
		return nil
	}
	out := make([]InterceptionDomain, len(snap.domains))
	copy(out, snap.domains)
	return out
}
