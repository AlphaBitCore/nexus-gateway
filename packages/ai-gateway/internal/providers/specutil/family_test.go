package specutil_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

func TestMatchesFamily(t *testing.T) {
	cases := []struct {
		model  string
		family string
		want   bool
		why    string
	}{
		{"gpt-5.4", "gpt-5.4", true, "the id is the family itself"},
		{"gpt-5.4-mini", "gpt-5.4", true, "'-' is a version separator"},
		{"claude-opus-4-6-20251101", "claude-opus-4-6", true, "dated snapshot of the family"},
		{"kimi-k2-thinking-turbo", "kimi-k2-thinking", true, "'-' separator"},
		{"claude-2.1", "claude-2", true, "'.' is a version separator"},

		// The whole reason this is not strings.HasPrefix: a longer version
		// number must not inherit the shorter family's carve-out.
		{"gpt-5.40", "gpt-5.4", false, "5.40 is not the 5.4 family"},
		{"claude-20-opus", "claude-2", false, "generation 20 is not generation 2"},
		{"kimi-k25", "kimi-k2", false, "k25 is not k2"},

		{"gpt-4o", "gpt-5", false, "different family"},
		{"gpt", "gpt-5", false, "shorter than the family"},
		{"", "gpt-5", false, "empty id"},
		// An empty family must not sweep the whole catalog into a quirk rule:
		// only an equally empty id "is" it.
		{"gpt-5", "", false, "an empty family names no family"},
		{"", "", true, "both empty is the degenerate equality"},
	}
	for _, tc := range cases {
		if got := specutil.MatchesFamily(tc.model, tc.family); got != tc.want {
			t.Errorf("MatchesFamily(%q, %q) = %v, want %v — %s", tc.model, tc.family, got, tc.want, tc.why)
		}
	}
}

func TestGenerationAtLeast(t *testing.T) {
	cases := []struct {
		model  string
		prefix string
		min    int
		want   bool
		why    string
	}{
		{"gpt-5", "gpt-", 5, true, "exactly the minimum generation"},
		{"gpt-5.4-mini", "gpt-", 5, true, "minor version and tier ride along"},
		{"gpt-6", "gpt-", 5, true, "an unreleased generation is inside the rule"},
		{"gpt-12", "gpt-", 5, true, "multi-digit generation"},
		{"gpt-4o", "gpt-", 5, false, "generation 4 is below the minimum"},
		{"gpt-4.1", "gpt-", 5, false, "4.1 is generation 4"},

		{"deepseek-v4-pro", "deepseek-v", 4, true, "evidenced generation"},
		{"deepseek-v5", "deepseek-v", 4, true, "later generation"},
		{"deepseek-v3.1-thinking", "deepseek-v", 4, false, "below the evidenced generation"},

		// Not a generation at all. These must not be swept into a quirk rule
		// on the strength of sharing a prefix.
		{"gpt-", "gpt-", 5, false, "bare prefix, no number"},
		{"gpt-turbo", "gpt-", 5, false, "letters where the generation goes"},
		{"deepseek-vision", "deepseek-v", 4, false, "the 'v' is part of a word, not a version"},
		{"claude-opus-4-6", "gpt-", 5, false, "different vendor line"},
		{"", "gpt-", 5, false, "empty id"},
	}
	for _, tc := range cases {
		if got := specutil.GenerationAtLeast(tc.model, tc.prefix, tc.min); got != tc.want {
			t.Errorf("GenerationAtLeast(%q, %q, %d) = %v, want %v — %s",
				tc.model, tc.prefix, tc.min, got, tc.want, tc.why)
		}
	}
}
