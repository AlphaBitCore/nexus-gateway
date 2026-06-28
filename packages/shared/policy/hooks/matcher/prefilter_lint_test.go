package matcher

import "testing"

func TestDiagnosePrefilter(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		friendly bool
		code     PrefilterCode
	}{
		// Friendly: anchors strip away, the content remains a selective literal/class.
		{"literal", "secret", true, PrefilterFriendly},
		{"anchored literal", "^confidential$", true, PrefilterFriendly},
		{"ssn", `\d{3}-\d{2}-\d{4}`, true, PrefilterFriendly},
		{"word-boundary keyword", `\bpassword\b`, true, PrefilterFriendly},
		{"alternation", `(foo|bar|baz)`, true, PrefilterFriendly},
		// Always-match: anchor-stripped superset matches empty input → no speedup.
		{"dot-star", ".*", false, PrefilterAlwaysMatch},
		{"star quantifier", "a*", false, PrefilterAlwaysMatch},
		{"optional", "x?", false, PrefilterAlwaysMatch},
		{"empty", "", false, PrefilterAlwaysMatch},
		{"all-optional alternation", "(a|)", false, PrefilterAlwaysMatch},
		// Anchor-only shapes: stripping leaves an empty match → always-match. These
		// also exercise stripAssertions' empty-concat / empty-alternation branches.
		{"both-anchors concat", "^$", false, PrefilterAlwaysMatch},
		{"two-anchor alternation", "(^|$)", false, PrefilterAlwaysMatch},
		{"anchor-or-literal alternation", "(^|x)", false, PrefilterAlwaysMatch},
		// Unparseable: not a regex.
		{"open group", "(", false, PrefilterUnparseable},
		{"bad class", "[a-", false, PrefilterUnparseable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DiagnosePrefilter(c.expr)
			if d.Friendly != c.friendly || d.Code != c.code {
				t.Fatalf("DiagnosePrefilter(%q) = {friendly:%v code:%q msg:%q}, want friendly=%v code=%q",
					c.expr, d.Friendly, d.Code, d.Message, c.friendly, c.code)
			}
			if !d.Friendly && d.Message == "" {
				t.Errorf("non-friendly verdict must carry an explanatory message")
			}
			if d.Friendly && (d.Code != PrefilterFriendly || d.Message != "") {
				t.Errorf("friendly verdict must have empty code+message, got code=%q msg=%q", d.Code, d.Message)
			}
		})
	}
}

// TestDiagnosePrefilter_MatchesRuntimeDegrade ensures the lint agrees with the
// runtime prefilter's own degrade decision: a pattern the lint calls friendly must
// anchor-strip without error (the runtime's first degrade condition).
func TestDiagnosePrefilter_MatchesRuntimeDegrade(t *testing.T) {
	for _, expr := range []string{"secret", "^confidential$", `\d{3}-\d{2}-\d{4}`, `\bpassword\b`} {
		if d := DiagnosePrefilter(expr); d.Friendly {
			if _, err := StripAnchors(expr); err != nil {
				t.Fatalf("lint says %q friendly but StripAnchors errored: %v", expr, err)
			}
		}
	}
}
