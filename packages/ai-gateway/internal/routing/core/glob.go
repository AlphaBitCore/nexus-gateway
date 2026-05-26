package core

import (
	"regexp"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

const (
	maxRegexLen   = 200
	maxRegexCache = 512
)

var (
	globRegexMu    sync.RWMutex
	globRegexCache = make(map[string]*regexp.Regexp)
)

// MatchGlob checks if a string matches a glob pattern (only * is supported).
// Uses an internal regex cache to avoid recompiling on every request.
func MatchGlob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	escaped := regexp.QuoteMeta(pattern)
	reStr := "^" + strings.ReplaceAll(escaped, `\*`, ".*") + "$"
	re := getCachedGlobRegex(reStr)
	if re == nil {
		return false
	}
	return re.MatchString(value)
}

func getCachedGlobRegex(pattern string) *regexp.Regexp {
	globRegexMu.RLock()
	r, ok := globRegexCache[pattern]
	globRegexMu.RUnlock()
	if ok {
		return r
	}
	if len(pattern) > maxRegexLen {
		return nil
	}
	r, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	globRegexMu.Lock()
	defer globRegexMu.Unlock()
	if len(globRegexCache) >= maxRegexCache {
		globRegexCache = make(map[string]*regexp.Regexp)
	}
	globRegexCache[pattern] = r
	return r
}

// ModelMatchesAllowedRefs checks if a model is permitted by the VK's allowed models list.
// Empty refs = unrestricted.
func ModelMatchesAllowedRefs(modelID, providerModelID, providerID string, refs []store.AllowedModelRef) bool {
	if len(refs) == 0 {
		return true
	}
	for _, ref := range refs {
		if ref.ProviderID != providerID {
			continue
		}
		if MatchGlob(ref.ModelID, modelID) || MatchGlob(ref.ModelID, providerModelID) {
			return true
		}
	}
	return false
}
