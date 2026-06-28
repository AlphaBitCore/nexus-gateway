package matcher

import "regexp/syntax"

// StripAnchors rewrites a pattern into a position-independent SUPERSET by
// replacing every zero-width assertion — ^ and $ (line and text forms), \A, \z,
// \Z, \b, \B — with the empty match. The result matches everywhere the original
// could and strictly more, because removing an assertion can only widen the
// accepted set.
//
// It exists to build a raw-body PREFILTER. The content-scanning hooks normally
// scan the structured text the traffic adapter extracts from the request body
// (an expensive gjson parse). A stripped pattern scanned over the RAW body
// bytes answers the cheaper question "could any rule match somewhere in here?":
// when the body carries no JSON backslash escape — so each extracted segment is
// a verbatim, contiguous substring of the raw body — a no-match by the stripped
// superset proves the original pattern matches none of those substrings either,
// so the structured extraction can be skipped without changing any decision.
//
// The anchors are dropped rather than honored because the raw body embeds the
// content inside JSON structure: a `^`/`$` honored against the whole buffer
// would anchor at the buffer edges, not at the segment edges the real scan uses,
// which could miss a real match (an unsound prefilter). Dropping them keeps the
// prefilter a true superset.
//
// Returns an error if expr does not parse under RE2/PerlX. The caller must then
// treat the owning hook as "always may-match" (never skip extraction), so a
// pattern the prefilter cannot represent never causes a silent miss.
func StripAnchors(expr string) (string, error) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return "", err
	}
	// Only patterns with a POSITION anchor (^ $ \A \z \Z, line or text) need
	// rewriting — those are what behave differently against the raw body (where
	// the content sits inside JSON structure, not at a buffer/line edge). Word
	// boundaries (\b/\B) are POSITION-EQUIVALENT in the raw body — the content is
	// flanked by JSON quotes/punctuation (non-word chars), so a boundary present
	// in the extracted text is present in the raw bytes too — so they are kept,
	// not stripped, and stay sound.
	//
	// Anchorless patterns are returned VERBATIM (no syntax.String() round-trip).
	// This is essential: the round-trip re-renders optional groups like
	// `ST(?:REET)?` as `ST(?:REET|(?:))`, and the literal `(?:)` empty branch
	// makes Vectorscan reject the pattern → silent demotion to the slow RE2
	// residual. The real engine compiles the ORIGINAL string directly, so an
	// untouched pattern compiles identically there. Only genuinely-anchored
	// patterns pay the round-trip (and the rare residual fallback if their
	// rewritten form trips the same artifact — a handful, not the whole set).
	if !hasPositionAnchor(re) {
		return expr, nil
	}
	return stripAssertions(re).String(), nil
}

// hasPositionAnchor reports whether the tree contains a ^/$/\A/\z/\Z position
// anchor (line or text). Word boundaries are intentionally excluded.
func hasPositionAnchor(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText:
		return true
	}
	for _, sub := range re.Sub {
		if hasPositionAnchor(sub) {
			return true
		}
	}
	return false
}

// stripAssertions walks the parsed regexp tree, replacing each zero-width
// assertion node with an empty-match node, then DROPS those empty-match nodes
// out of any concatenation so the regenerated string is clean (`SECRET`, not
// `(?:)SECRET`). The clean form matters: a literal `(?:)` empty sub-expression
// makes Hyperscan/Vectorscan reject the pattern, which silently demotes it to
// the slow RE2 residual — turning the whole raw-body prefilter into a pure-Go
// regexp scan (observed at 90%+ CPU). Keeping the output VS-compilable is what
// makes the prefilter cheap. Literal, class, and quantifier nodes are left
// untouched so the prefilter still requires the pattern's concrete bytes.
func stripAssertions(re *syntax.Regexp) *syntax.Regexp {
	switch re.Op {
	case syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText:
		// Word boundaries (\b/\B) are deliberately NOT stripped — they are
		// position-equivalent in the raw body, so keeping them preserves both
		// soundness and Vectorscan-compilability.
		return &syntax.Regexp{Op: syntax.OpEmptyMatch}
	}
	if len(re.Sub) == 0 {
		return re
	}
	for i, sub := range re.Sub {
		re.Sub[i] = stripAssertions(sub)
	}
	// In a concatenation, the stripped anchors are now empty-match nodes that
	// contribute nothing to the language — remove them so `.String()` does not
	// emit `(?:)` fragments that Vectorscan cannot compile.
	if re.Op == syntax.OpConcat {
		kept := make([]*syntax.Regexp, 0, len(re.Sub))
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpEmptyMatch {
				continue
			}
			kept = append(kept, sub)
		}
		switch len(kept) {
		case 0:
			return &syntax.Regexp{Op: syntax.OpEmptyMatch}
		case 1:
			return kept[0]
		default:
			re.Sub = kept
		}
	}

	// In an alternation, an empty-match branch came from a stripped anchor that
	// was itself one alternative (e.g. `(?:^|;|X)` → `(?:(?:)|;|X)`). An empty
	// alternative makes the WHOLE group optional, so `(?:|A|B)` ≡ `(?:A|B)?` —
	// rewrite to the quantified form, which is the superset-correct shape AND
	// Vectorscan-compilable (no literal `(?:)`).
	if re.Op == syntax.OpAlternate {
		kept := make([]*syntax.Regexp, 0, len(re.Sub))
		hadEmpty := false
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpEmptyMatch {
				hadEmpty = true
				continue
			}
			kept = append(kept, sub)
		}
		if !hadEmpty {
			return re
		}
		var inner *syntax.Regexp
		switch len(kept) {
		case 0:
			return &syntax.Regexp{Op: syntax.OpEmptyMatch}
		case 1:
			inner = kept[0]
		default:
			re.Sub = kept
			inner = re
		}
		return &syntax.Regexp{Op: syntax.OpQuest, Sub: []*syntax.Regexp{inner}}
	}
	return re
}
