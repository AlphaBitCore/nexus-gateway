package moonshot

import "testing"

// TestApplyRewrites_FixedTempModels asserts that the
// kimi-k2.5 / kimi-k2.6 thinking models strip caller-supplied
// temperature + top_p (the upstream rejects any non-1 temperature
// with HTTP 400 "invalid temperature: only 1 is allowed for this
// model.") so the upstream's mandatory default applies.
func TestApplyRewrites_FixedTempModels(t *testing.T) {
	t.Parallel()

	for _, m := range []string{"kimi-k2.5", "kimi-k2.6"} {
		t.Run(m, func(t *testing.T) {
			payload := map[string]any{
				"model":       m,
				"temperature": 0.3,
				"top_p":       0.9,
			}
			rewrites := ApplyRewrites(payload, m)
			if _, ok := payload["temperature"]; ok {
				t.Errorf("temperature must be stripped on %s", m)
			}
			if _, ok := payload["top_p"]; ok {
				t.Errorf("top_p must be stripped on %s", m)
			}
			wantRewrites := map[string]bool{
				"temperature→removed": false,
				"top_p→removed":       false,
			}
			for _, r := range rewrites {
				wantRewrites[r] = true
			}
			for k, seen := range wantRewrites {
				if !seen {
					t.Errorf("missing rewrite %q on %s (got %v)", k, m, rewrites)
				}
			}
		})
	}
}

// TestApplyRewrites_OtherModels asserts the strip is targeted —
// moonshot-v1-*, kimi-k2-thinking, and any future kimi model that
// isn't in the fixed-temp family must accept the caller's temperature
// unchanged.
func TestApplyRewrites_OtherModels(t *testing.T) {
	t.Parallel()

	for _, m := range []string{"kimi-k2-thinking", "moonshot-v1-128k", "moonshot-v1-32k", "moonshot-v1-8k"} {
		t.Run(m, func(t *testing.T) {
			payload := map[string]any{
				"model":       m,
				"temperature": 0.3,
				"top_p":       0.9,
			}
			rewrites := ApplyRewrites(payload, m)
			if len(rewrites) != 0 {
				t.Errorf("expected no rewrites for %s, got %v", m, rewrites)
			}
			if got, ok := payload["temperature"].(float64); !ok || got != 0.3 {
				t.Errorf("temperature=%v want 0.3 on %s", payload["temperature"], m)
			}
			if got, ok := payload["top_p"].(float64); !ok || got != 0.9 {
				t.Errorf("top_p=%v want 0.9 on %s", payload["top_p"], m)
			}
		})
	}
}

// TestIsFixedTempModel pins the prefix-list for the
// fixed-temperature family.
func TestIsFixedTempModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{"kimi-k2.5", true},
		{"kimi-k2.5-2026-04", true},
		{"kimi-k2.6", true},
		{"kimi-k2.6-mini", true},
		{"kimi-k2-thinking", false},
		{"kimi-k2", false},
		{"moonshot-v1-8k", false},
		{"moonshot-v1-128k", false},
		{"", false},
	}
	for _, tc := range cases {
		got := IsFixedTempModel(tc.model)
		if got != tc.want {
			t.Errorf("IsFixedTempModel(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}
