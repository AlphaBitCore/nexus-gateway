package core

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	regexCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "hooks",
		Name:      "regex_cache_hits_total",
		Help:      "Number of regex pattern cache hits.",
	})
	regexCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "hooks",
		Name:      "regex_cache_misses_total",
		Help:      "Number of regex pattern cache misses.",
	})
)

// defaultRegexCacheCap is the default number of compiled regex patterns
// cached per process. Configurable at startup via SetRegexCacheCap().
const defaultRegexCacheCap = 10000

// regexCache is the process-wide LRU of compiled patterns. Stored behind an
// atomic.Pointer so SetRegexCacheCap can replace the cache atomically while
// CompilePattern readers are in flight.
var regexCache atomic.Pointer[lru.Cache[string, *regexp.Regexp]]

func init() {
	c, err := lru.New[string, *regexp.Regexp](defaultRegexCacheCap)
	if err != nil {
		// lru.New only errors on non-positive size; defaultRegexCacheCap is positive.
		panic(fmt.Sprintf("hooks: failed to init regex cache: %v", err))
	}
	regexCache.Store(c)
}

// SetRegexCacheCap replaces the cache with a new one of the given capacity.
// Call during process startup if the default is unsuitable. Panics on
// non-positive cap. Safe to call concurrently with CompilePattern; the cache
// swap is atomic and readers that loaded the old cache keep it alive via GC.
func SetRegexCacheCap(cap int) {
	c, err := lru.New[string, *regexp.Regexp](cap)
	if err != nil {
		panic(err)
	}
	regexCache.Store(c)
}

// CompilePattern returns a compiled *regexp.Regexp for (pattern, flags),
// hitting a process-wide LRU cache.
//
// flags uses the pii-detector convention:
//
//	"i" — case-insensitive
//	"s" — dot-matches-newline
//	"m" — multi-line
//	"U" — swap greedy/non-greedy
//
// Unknown flags return an error. Flags are canonicalized (deduped + sorted)
// so equivalent flag orderings hit the same cache entry.
//
// *regexp.Regexp is concurrency-safe and immutable; cached instances are
// safe to share across goroutines without further synchronization.
func CompilePattern(pattern, flags string) (*regexp.Regexp, error) {
	canonFlags, err := canonicalizeFlags(flags)
	if err != nil {
		return nil, err
	}
	key := pattern + "\x00" + canonFlags
	cache := regexCache.Load()
	if re, ok := cache.Get(key); ok {
		regexCacheHits.Inc()
		return re, nil
	}
	regexCacheMisses.Inc()

	finalPattern := pattern
	if canonFlags != "" {
		finalPattern = "(?" + canonFlags + ")" + pattern
	}
	re, err := regexp.Compile(finalPattern)
	if err != nil {
		return nil, err
	}
	cache.Add(key, re)
	return re, nil
}

// canonicalizeFlags dedupes, sorts, and validates flag characters.
// Empty input returns empty output without allocation.
func canonicalizeFlags(flags string) (string, error) {
	if flags == "" {
		return "", nil
	}
	seen := make(map[rune]struct{}, len(flags))
	for _, f := range flags {
		switch f {
		case 'i', 's', 'm', 'U':
			seen[f] = struct{}{}
		default:
			return "", fmt.Errorf("hooks: unsupported regex flag %q", f)
		}
	}
	if len(seen) == 0 {
		return "", nil
	}
	out := make([]rune, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return strings.TrimSpace(string(out)), nil
}
