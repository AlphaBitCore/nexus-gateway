package matcher

import (
	"regexp"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// re2Matcher is the pure-Go implementation. Each pattern is compiled to its own
// `regexp.Regexp` via core.CompilePattern — the SAME compilation the rule-pack
// engine uses today, so RE2 match results are byte-identical to the current
// engine (the differential-test invariant).
type re2Matcher struct {
	pats []re2Pat
}

type re2Pat struct {
	id int
	re *regexp.Regexp
}

// CompileRE2 builds an RE2 matcher. Patterns that fail to compile are skipped
// and returned in `bad`; the returned Matcher contains every pattern that did
// compile. Mirrors the engine's per-rule skip+log fail-posture.
func CompileRE2(pats []Pattern) (Matcher, []BadPattern) {
	m := &re2Matcher{pats: make([]re2Pat, 0, len(pats))}
	var bad []BadPattern
	for _, p := range pats {
		re, err := core.CompilePattern(p.Expr, p.Flags)
		if err != nil {
			bad = append(bad, BadPattern{ID: p.ID, Err: err})
			continue
		}
		m.pats = append(m.pats, re2Pat{id: p.ID, re: re})
	}
	return m, bad
}

func (m *re2Matcher) Scan(segments []string, firstOnly bool) []Hit {
	var hits []Hit
	for _, p := range m.pats {
		for si, seg := range segments {
			if firstOnly {
				if loc := p.re.FindStringIndex(seg); loc != nil {
					hits = append(hits, Hit{ID: p.id, Seg: si, Start: loc[0], End: loc[1]})
				}
				continue
			}
			for _, loc := range p.re.FindAllStringIndex(seg, -1) {
				hits = append(hits, Hit{ID: p.id, Seg: si, Start: loc[0], End: loc[1]})
			}
		}
	}
	return hits
}
