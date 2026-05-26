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
		return matchRegex(pattern, host)
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
