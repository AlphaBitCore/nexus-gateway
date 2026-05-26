package aiguard

import (
	"strings"
	"unicode"
)

// canonicalizeForCacheKey lowercases, collapses runs of whitespace to a single
// space, and trims. Unicode case folding via unicode.ToLower (locale-
// insensitive; acceptable for enterprise AI detection tagging).
//
// The output is the input to the sha256 cache key; a stable normalization
// is more important than a maximally-canonical one.
func canonicalizeForCacheKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	started := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if started {
				inSpace = true
			}
			continue
		}
		if inSpace {
			b.WriteByte(' ')
			inSpace = false
		}
		b.WriteRune(unicode.ToLower(r))
		started = true
	}
	return b.String()
}
