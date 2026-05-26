package middleware

import "testing"

// TestFirstNonEmpty_AllPathsCovered locks the firstNonEmpty helper:
// the live JWT path always passes a non-empty subject so the
// "all-empty → ”" fallback is structurally unreachable through
// AdminAuth. Cover it here directly so a future refactor that
// rearranges callers doesn't drop the contract silently.
func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"first_nonempty", []string{"first", "second"}, "first"},
		{"second_nonempty", []string{"", "second"}, "second"},
		{"all_empty", []string{"", "", ""}, ""},
		{"no_args", nil, ""},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			got := firstNonEmpty(tc.in...)
			if got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}
