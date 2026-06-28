package validators

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// The raw-body prefilter (contentPrescan) lets the proxy skip structured
// extraction when it proves no rule can match. Its ONE load-bearing safety
// property is SOUNDNESS: it must never report "no match" for a body whose
// extracted content the real matcher would actually flag — that would forward
// unscanned (possibly sensitive) content. This is the differential gate that
// pins it: for every (pattern set, content) pair, a real hit MUST imply the
// prefilter said "may match". Coverage (how often it skips) is a perf concern;
// a single soundness violation is a compliance leak.

// wrapJSONNoEscape embeds text as the user-message content of an OpenAI-shape
// body. text must contain no `"` or `\` so the body carries no JSON escape —
// the regime in which the proxy trusts a prefilter no-match (the content bytes
// then appear verbatim and contiguously inside the raw body).
func wrapJSONNoEscape(text string) []byte {
	return []byte(`{"model":"m","messages":[{"role":"user","content":"` + text + `"}]}`)
}

func buildPats(exprs []struct{ expr, flags string }) []matcher.Pattern {
	pats := make([]matcher.Pattern, 0, len(exprs))
	for i, e := range exprs {
		pats = append(pats, matcher.Pattern{ID: i, Expr: e.expr, Flags: e.flags})
	}
	return pats
}

func TestContentPrescan_SoundnessDifferential(t *testing.T) {
	// A rule set deliberately mixing the anchor forms that would break a naive
	// raw scan: ^/$ text anchors, (?m) line anchors, \b word boundaries, and
	// case-insensitive flags. The anchor strip must turn each into a superset so
	// a content-start / word-boundary match is never missed by the raw scan.
	patSets := map[string][]struct{ expr, flags string }{
		"plain":         {{`password`, "i"}, {`\d{3}-\d{2}-\d{4}`, ""}},
		"text-anchored": {{`^SECRET`, ""}, {`KEY$`, ""}},
		"line-anchored": {{`(?m)^CONFIDENTIAL`, ""}},
		"word-boundary": {{`\bTOKEN\b`, ""}},
		"case-insens":   {{`confidential`, "i"}},
		"mixed":         {{`^BEGIN`, ""}, {`\bAPIKEY\b`, "i"}, {`secret\d{2}`, "i"}},
	}

	// Content fragments, including ones positioned to defeat anchors: a literal
	// at the very start of the segment (so ^ fires in the segment but not in the
	// raw body, where the segment is preceded by JSON structure), at the end, and
	// surrounded by word boundaries.
	contents := []string{
		"hello world nothing here",
		"SECRET starts the segment",                 // ^SECRET fires in segment, not raw — strip must catch
		"value ends with the KEY",                   // KEY$ at segment end
		"CONFIDENTIAL leading line",                 // (?m)^CONFIDENTIAL
		"a TOKEN in the middle",                     // \bTOKEN\b
		"My Confidential note",                      // case-insensitive
		"BEGIN here and APIKEY plus secret42",       // multiple
		"my password is in here",                    // plain literal
		"call 123-45-6789 now",                      // SSN-like \d{3}-\d{2}-\d{4}
		"benign filler system gateway model tokens", // benchmark-like benign words
		"APIKEYISTOOLONG no boundary",               // \b should NOT fire (no boundary) — neither side flags
	}

	for setName, exprs := range patSets {
		pats := buildPats(exprs)
		m, bad := buildMatcher(pats)
		if len(bad) > 0 {
			t.Fatalf("%s: unexpected bad pattern: %v", setName, bad)
		}
		pre := newContentPrescan(pats)
		if pre.prescan == nil {
			t.Fatalf("%s: prescan failed to build (would force-extract always, defeating the optimization)", setName)
		}
		for _, text := range contents {
			body := wrapJSONNoEscape(text)
			segments := []string{text} // the extraction of a no-escape single-message body

			realHits := matchedSet(m, segments)
			mayMatch := pre.MayMatchRaw(body)

			// SOUNDNESS: a real hit must imply the prefilter said "may match".
			if len(realHits) > 0 && !mayMatch {
				t.Errorf("SOUNDNESS VIOLATION %s / %q: real matcher hit %d but prefilter said no-match (would skip extraction and leak)",
					setName, text, len(realHits))
			}
		}
	}
}

// TestContentPrescan_Effectiveness asserts the prefilter actually SKIPS on
// benign traffic (the whole point) and does NOT skip when content matches — the
// behaviour the A/B win depends on. Soundness is covered above; this guards
// against a degenerate prefilter that always says "may match" (correct but
// useless) or that strips a literal away (would over-skip — caught by soundness).
func TestContentPrescan_Effectiveness(t *testing.T) {
	pats := buildPats([]struct{ expr, flags string }{
		{`^SECRET`, ""}, {`\bTOKEN\b`, ""}, {`password`, "i"}, {`\d{3}-\d{2}-\d{4}`, ""},
	})
	pre := newContentPrescan(pats)
	if pre.prescan == nil {
		t.Fatal("prescan failed to build")
	}

	benign := wrapJSONNoEscape("the quick brown fox summarize this document please")
	if pre.MayMatchRaw(benign) {
		t.Errorf("benign body should be prefiltered (skip extraction), but prefilter said may-match")
	}

	for _, hit := range []string{
		"SECRET at the start",
		"a TOKEN here",
		"my Password value",
		"ssn 123-45-6789",
	} {
		if !pre.MayMatchRaw(wrapJSONNoEscape(hit)) {
			t.Errorf("content %q matches a rule but prefilter said no-match (soundness leak)", hit)
		}
	}
}

// TestStripAnchors_IsSuperset is the unit-level proof that anchor stripping only
// widens the language: for representative anchored patterns, every string the
// original matches the stripped form also matches (and the stripped form drops
// the position dependence so it matches inside a larger buffer too).
func TestStripAnchors_IsSuperset(t *testing.T) {
	cases := []struct {
		expr     string
		matchSeg string // string the original (anchored) pattern matches as a whole segment
	}{
		{`^SECRET`, "SECRET here"},
		{`KEY$`, "the KEY"},
		{`(?m)^CONFIDENTIAL`, "CONFIDENTIAL line"},
		{`\bTOKEN\b`, "a TOKEN x"},
	}
	for _, c := range cases {
		stripped, err := matcher.StripAnchors(c.expr)
		if err != nil {
			t.Fatalf("StripAnchors(%q): %v", c.expr, err)
		}
		// Original matches the segment.
		om := matchedSet(mustMatcher(t, c.expr, ""), []string{c.matchSeg})
		if len(om) == 0 {
			t.Fatalf("setup: original %q did not match %q", c.expr, c.matchSeg)
		}
		// Stripped matches the SAME content embedded mid-buffer (anchors gone).
		embedded := "PREFIX_" + c.matchSeg + "_SUFFIX"
		sm := matchedSet(mustMatcher(t, stripped, ""), []string{embedded})
		if len(sm) == 0 {
			t.Errorf("stripped %q (from %q) failed to match embedded %q — not a superset", stripped, c.expr, embedded)
		}
	}
}

func mustMatcher(t *testing.T, expr, flags string) matcher.Matcher {
	t.Helper()
	m, bad := buildMatcher([]matcher.Pattern{{ID: 0, Expr: expr, Flags: flags}})
	if len(bad) > 0 {
		t.Fatalf("compile %q: %v", expr, bad[0].Err)
	}
	return m
}

// TestStripAnchors_BadPatternConservative ensures an unparseable pattern yields
// an error so newContentPrescan degrades to the conservative (always-extract)
// posture rather than silently dropping the rule from the prefilter.
func TestStripAnchors_BadPatternConservative(t *testing.T) {
	// A backreference is valid PCRE/Vectorscan-ish but rejected by RE2/syntax —
	// the prefilter cannot represent it, so the owning hook must never skip.
	if _, err := matcher.StripAnchors(`(a)\1`); err == nil {
		t.Skip("StripAnchors accepted backreference; environment regex engine differs")
	}
	pats := buildPats([]struct{ expr, flags string }{{`password`, ""}, {`(a)\1`, ""}})
	pre := newContentPrescan(pats)
	if pre.prescan != nil {
		t.Errorf("prescan built despite an unrepresentable pattern; must be nil (conservative always-extract)")
	}
	if !pre.MayMatchRaw([]byte(`{"x":"totally benign"}`)) {
		t.Errorf("conservative prescan must report may-match (force extraction) when it could not be built")
	}
}
