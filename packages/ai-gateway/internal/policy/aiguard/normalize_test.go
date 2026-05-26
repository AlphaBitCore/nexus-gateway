package aiguard

import "testing"

func TestNormalizeContent(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"lowercases_ascii", "Hello World", "hello world"},
		{"collapses_whitespace", "a   b\tc\n\nd", "a b c d"},
		{"trims_leading_trailing", "   hi   ", "hi"},
		{"preserves_punctuation", "Hello, World!", "hello, world!"},
		{"handles_empty", "", ""},
		{"handles_only_whitespace", "   \t\n  ", ""},
		{"handles_unicode_case", "PRIVÉ", "privé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalizeForCacheKey(tc.in)
			if got != tc.want {
				t.Errorf("canonicalizeForCacheKey(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
