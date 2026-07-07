package matcher

import (
	"regexp/syntax"
	"unicode"
	"unicode/utf8"
)

// MaxMatchBytes returns a conservative UPPER BOUND, in bytes, on the longest
// contiguous text a single match of `expr` can span, and whether that bound is
// FINITE. It feeds the Model-A streaming engine's flush-before-deliver lookahead
// (modela.Config.MaxPatternBytes): a value straddling a held-unit boundary must be
// scanned over a window at least this wide before the leading unit is delivered, or
// it can leak across two raw deliveries. Correctness direction is asymmetric:
//
//   - OVER-estimating is safe — a wider lookahead only costs extra (sound) scans.
//   - UNDER-estimating reopens the boundary leak.
//
// So every node rounds UP (char classes count utf8.UTFMax bytes), and any UNBOUNDED
// repeat — `*`, `+`, or `{n,}` — makes the whole match unbounded (`bounded=false`).
// Treating `{n,}` as unbounded (rather than mirroring the vectorscan detection cap of
// detectionRepeatCap) is deliberate: the cap is `//go:build vectorscan` only, so the
// re2 fallback build does NOT cap, and a value an unbounded pattern matches can be
// window-sized regardless — that is the disclosed over-window best-effort surface, for
// which the finite lookahead cannot help anyway. An unbounded pattern therefore does
// NOT contribute to MaxPatternBytes (the caller leaves such patterns best-effort).
//
// A parse error yields (0, false): an unparseable pattern is treated as unbounded
// (conservative — it never shrinks the lookahead).
func MaxMatchBytes(expr string) (n int, bounded bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return 0, false
	}
	return maxMatchBytes(re)
}

func maxMatchBytes(re *syntax.Regexp) (int, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		// Sum each literal rune's UTF-8 width — the true byte length (over-estimating ASCII
		// at utf8.UTFMax would inflate the lookahead toward the tail window). A case-folded
		// literal needs the orbit walk: Go stores `(?i)k` as a single OpLiteral carrying only
		// the MINIMUM fold rune ('K'/'k'), yet it matches U+212A (Kelvin, 3 bytes), so the
		// width must be taken over the whole simple-fold orbit or the bound under-counts.
		fold := re.Flags&syntax.FoldCase != 0
		n := 0
		for _, r := range re.Rune {
			n += foldedRuneBytes(r, fold)
		}
		return n, true
	case syntax.OpCharClass:
		// One char from the class: its widest member's UTF-8 byte width (an ASCII-only
		// class is 1 byte, NOT 4). UTF-8 width is monotonic in code point, so the widest
		// rune in each [lo,hi] range is hi.
		w := 0
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if b := runeBytes(re.Rune[i+1]); b > w {
				w = b
			}
		}
		if w == 0 {
			w = utf8.UTFMax // empty/degenerate class: conservative
		}
		return w, true
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return utf8.UTFMax, true // any rune → up to 4 bytes
	case syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return 0, true // zero-width
	case syntax.OpCapture:
		return maxMatchBytes(re.Sub[0])
	case syntax.OpQuest:
		// 0 or 1 occurrence → the max is one occurrence.
		return maxMatchBytes(re.Sub[0])
	case syntax.OpStar, syntax.OpPlus:
		return 0, false // unbounded
	case syntax.OpConcat:
		total := 0
		for _, sub := range re.Sub {
			sn, sb := maxMatchBytes(sub)
			if !sb {
				return 0, false
			}
			total += sn
		}
		return total, true
	case syntax.OpAlternate:
		best := 0
		for _, sub := range re.Sub {
			sn, sb := maxMatchBytes(sub)
			if !sb {
				return 0, false
			}
			if sn > best {
				best = sn
			}
		}
		return best, true
	case syntax.OpRepeat:
		// `{n,m}` is bounded by m; `{n,}` (Max == -1) is unbounded.
		if re.Max < 0 {
			return 0, false
		}
		sn, sb := maxMatchBytes(re.Sub[0])
		if !sb {
			return 0, false
		}
		return sn * re.Max, true
	default:
		// OpNoMatch and any future op: treat as unbounded (conservative).
		return 0, false
	}
}

// runeBytes is the UTF-8 byte width of r, conservatively utf8.UTFMax for an invalid or
// out-of-range rune (e.g. a surrogate) so the bound never under-counts.
func runeBytes(r rune) int {
	if n := utf8.RuneLen(r); n > 0 {
		return n
	}
	return utf8.UTFMax
}

// foldedRuneBytes is the widest UTF-8 byte width r can match. When the literal is NOT
// case-folded it is just runeBytes(r); when it IS folded it is the max over r's
// simple-fold orbit (e.g. {'K','k',U+212A} → 3), because a folded OpLiteral stores only
// the orbit's minimum rune but matches any orbit member — taking only runeBytes(r) would
// under-count (the reopened-boundary-leak failure mode).
func foldedRuneBytes(r rune, fold bool) int {
	w := runeBytes(r)
	if !fold {
		return w
	}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if b := runeBytes(f); b > w {
			w = b
		}
	}
	return w
}

// MaxBoundedMatchBytes returns the largest finite MaxMatchBytes across exprs (0 when
// none are bounded), and whether ANY expr was unbounded. The bounded max sizes the
// Model-A lookahead so every bounded-length enforceable value straddling a unit
// boundary is caught; unbounded patterns are reported so the caller can disclose them
// as best-effort (a value they match can exceed the window).
func MaxBoundedMatchBytes(exprs []string) (maxBounded int, anyUnbounded bool) {
	for _, e := range exprs {
		n, bounded := MaxMatchBytes(e)
		if !bounded {
			anyUnbounded = true
			continue
		}
		if n > maxBounded {
			maxBounded = n
		}
	}
	return maxBounded, anyUnbounded
}
