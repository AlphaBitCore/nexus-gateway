package traffic

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// matchHost tests whether a hostname matches a pattern using the given match type.
func matchHost(host, pattern string, matchType interception.HostMatchType) bool {
	switch matchType {
	case interception.HostMatchTypeExact:
		return strings.EqualFold(host, pattern)
	case interception.HostMatchTypePrefix:
		return strings.HasPrefix(strings.ToLower(host), strings.ToLower(pattern))
	case interception.HostMatchTypeGlob:
		matched, _ := filepath.Match(strings.ToLower(pattern), strings.ToLower(host))
		return matched
	case interception.HostMatchTypeRegex:
		// Regex was the only host match type that did not fold case, while
		// Exact, Prefix and Glob all lowercase both sides — and hostnames are
		// case-insensitive per RFC 4343.
		//
		// THE FOLD IS NOT PURELY "CASE ONLY", and the limit is worth stating
		// rather than glossing: RE2 folds a character class's RANGES before
		// negating it, so under (?i) a pattern containing [^a-z] stops matching
		// A-Z. A host pattern relying on a negated ASCII-letter class therefore
		// matches strictly less than it did. The alternative — lowercasing the
		// input and leaving the pattern verbatim — has the mirror problem: it
		// silently kills any pattern an admin wrote with a capital in it, so the
		// rule never fires and the host is relayed uninspected.
		//
		// shared/policy/domain's Engine, the matcher on the live CONNECT path,
		// takes the same (?i) side for the same reason. The two are fed the
		// same config rows and must agree on them.
		//
		// Paths keep the verbatim matchRegex: paths ARE case-sensitive, and a
		// test pins that so the fold cannot spread.
		return matchRegexFold(pattern, host)
	default:
		return false
	}
}

// matchPathRule tests whether a request path matches any pattern in the path rule.
func matchPathRule(reqPath string, rule *InterceptionPathConfig) bool {
	for _, pattern := range rule.PathPattern {
		if matchPath(reqPath, pattern, rule.MatchType) {
			return true
		}
	}
	return false
}

// matchPath tests a single path against a pattern using the given match type.
func matchPath(reqPath, pattern string, matchType interception.PathMatchType) bool {
	switch matchType {
	case interception.PathMatchTypeExact:
		return reqPath == pattern
	case interception.PathMatchTypePrefix:
		return strings.HasPrefix(reqPath, pattern)
	case interception.PathMatchTypeGlob:
		matched, _ := filepath.Match(pattern, reqPath)
		return matched
	case interception.PathMatchTypeRegex:
		return matchRegex(pattern, reqPath)
	default:
		return false
	}
}

// regexCache caches compiled regexes to avoid recompilation on every match.
// Bounded to maxRegexCache entries; when full, the cache is cleared to prevent
// unbounded memory growth from config churn.
const maxRegexCache = 512

var (
	regexMu    sync.RWMutex
	regexCache = make(map[string]*regexp.Regexp)
)

// matchRegex compiles (with caching) and matches a regex pattern against input.
// Returns false on compilation error (should have been caught at config validation time).
func matchRegex(pattern, input string) bool {
	regexMu.RLock()
	re, ok := regexCache[pattern]
	regexMu.RUnlock()
	if ok {
		return re.MatchString(input)
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}

	regexMu.Lock()
	if len(regexCache) >= maxRegexCache {
		regexCache = make(map[string]*regexp.Regexp)
	}
	regexCache[pattern] = compiled
	regexMu.Unlock()

	return compiled.MatchString(input)
}

// matchRegexFold matches case-insensitively by compiling the pattern with the
// inline (?i) flag. The prefixed pattern is what reaches the cache, so a host
// rule and a path rule sharing the same pattern text stay separate entries
// rather than one of them inheriting the other's flags.
func matchRegexFold(pattern, input string) bool {
	return matchRegex("(?i)"+pattern, input)
}

// HostMatchSpecificity returns a rank for host match type tiebreaking.
func HostMatchSpecificity(mt interception.HostMatchType) int {
	switch mt {
	case interception.HostMatchTypeExact:
		return 4
	case interception.HostMatchTypePrefix:
		return 3
	case interception.HostMatchTypeGlob:
		return 2
	case interception.HostMatchTypeRegex:
		return 1
	default:
		return 0
	}
}
