//go:build vectorscan

package matcher

import "testing"

func TestBoundForDetection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "unbounded wide repeat is capped",
			in:   `AKIA[A-Za-z0-9/+]{16,}`,
			want: `AKIA[A-Za-z0-9/+]{16,64}`,
		},
		{
			name: "over-bounded repeat above cap is lowered",
			in:   `[^>]{0,160}`,
			want: `[^>]{0,64}`,
		},
		{
			name: "bounded at-or-below cap is left alone",
			in:   `[A-Za-z]{2,24}`,
			want: `[A-Za-z]{2,24}`,
		},
		{
			name: "min above cap is left unbounded (capping would be min>max)",
			in:   `sntrys_[A-Za-z0-9]{80,}`,
			want: `sntrys_[A-Za-z0-9]{80,}`,
		},
		{
			name: "exact repeat untouched",
			in:   `[0-9]{16}`,
			want: `[0-9]{16}`,
		},
		{
			name: "escaped literal brace is never rewritten",
			in:   `price\{4,\}dollars`,
			want: `price\{4,\}dollars`,
		},
		{
			name: "fully anchored spanning pattern is skipped",
			in:   `^[A-Za-z0-9]{4,}$`,
			want: `^[A-Za-z0-9]{4,}$`,
		},
		{
			name: "narrow repeat is harmlessly capped (presence preserved)",
			in:   `\.{4,}`,
			want: `\.{4,64}`,
		},
		{
			name: "no counted repeat is a no-op",
			in:   `(?i)\bconfidential\b`,
			want: `(?i)\bconfidential\b`,
		},
		{
			name: "two-sided cap keeps the min",
			in:   `[\w.-]{1,255}`,
			want: `[\w.-]{1,64}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundForDetection(tc.in); got != tc.want {
				t.Errorf("boundForDetection(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
		})
	}
}
