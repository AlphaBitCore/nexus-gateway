//go:build vectorscan

package matcher

import (
	"regexp"
	"regexp/syntax"
	"strconv"
)

// detectionRepeatCap bounds a wide repeat in the DETECTION database. It mirrors
// the save-time linter's wideBoundedMax (the RFC-5321 local-part limit + 1): a
// repeat bounded at or below this prefilters acceptably, while an unbounded or
// larger one defeats Vectorscan's literal prefilter and falls to a per-position
// scan that explodes on long alnum/base64 runs (measured 50KB random alnum:
// ~1.7ms vs ~0.19ms; a 50KB base64 vision payload is the common trigger).
const detectionRepeatCap = 64

// reUnbounded matches an unbounded counted repeat `{n,}`; reBounded matches a
// two-sided counted repeat `{n,m}`. Both are anchored on a non-backslash byte so
// an escaped literal brace `\{` is never rewritten. The leading group is the
// preserved preceding byte (or start-of-string).
var (
	reUnbounded = regexp.MustCompile(`(^|[^\\])\{(\d+),\}`)
	reBounded   = regexp.MustCompile(`(^|[^\\])\{(\d+),(\d+)\}`)
)

// boundForDetection caps wide counted repeats to detectionRepeatCap for the
// DETECTION pass ONLY. It is applied to the expression handed to hs_compile; the
// rule's original Pattern is never touched, so the RE2 redaction pass (which
// re-localises every fired rule with the ORIGINAL pattern) still masks the full
// secret span — detection is bounded, redaction is complete.
//
// Soundness: detection is presence-only and the matched span is not consumed by
// any decision (rulepack_engine keys its hit set on (ruleID, segment), not the
// span). Capping `X{n,}` to `X{n,cap}` preserves whether the rule FIRES for every
// input the original matched, because the engine still finds a match window
// anywhere in the text — including a window ending at the buffer end for a
// trailing `$`. The sole shape where capping could drop a hit is a pattern
// anchored at BOTH ends whose spanning repeat exceeds the cap (e.g. `^X{4,}$`
// over a >cap run); anchoredSpanning detects that and leaves such patterns
// untouched.
//
// Implementation is a guarded string rewrite rather than an AST round-trip:
// regexp/syntax.String() re-folds `(?i)` into Unicode case classes (e.g. adds
// the long-s and Kelvin runes to `[A-Za-z]`), which forces Vectorscan into its
// much slower UTF-8 path and defeats the whole optimisation. The string rewrite
// changes only the repeat bounds and nothing else.
//
// Fail-safe: a parse error (used only for the anchor check) leaves the
// expression unchanged. Only `{n,}` / `{n,m>cap}` with `n<=cap` are rewritten;
// `n>cap` is left as-is (capping it would be an invalid min>max repeat).
func boundForDetection(expr string) string {
	if !reUnbounded.MatchString(expr) && !reBounded.MatchString(expr) {
		return expr
	}
	if re, err := syntax.Parse(expr, syntax.Perl); err == nil && anchoredSpanning(re) {
		return expr
	}
	out := reUnbounded.ReplaceAllStringFunc(expr, func(m string) string {
		sub := reUnbounded.FindStringSubmatch(m)
		n, _ := strconv.Atoi(sub[2])
		if n > detectionRepeatCap {
			return m
		}
		return sub[1] + "{" + sub[2] + "," + strconv.Itoa(detectionRepeatCap) + "}"
	})
	out = reBounded.ReplaceAllStringFunc(out, func(m string) string {
		sub := reBounded.FindStringSubmatch(m)
		n, _ := strconv.Atoi(sub[2])
		hi, _ := strconv.Atoi(sub[3])
		if hi <= detectionRepeatCap || n > detectionRepeatCap {
			return m
		}
		return sub[1] + "{" + sub[2] + "," + strconv.Itoa(detectionRepeatCap) + "}"
	})
	return out
}

// anchoredSpanning reports whether the top-level pattern is anchored at both the
// start and the end, in which case capping a spanning repeat could drop a hit on
// an over-cap run. Conservative: any pattern that both begins with a start anchor
// and ends with an end anchor is left untouched.
func anchoredSpanning(re *syntax.Regexp) bool {
	if re.Op != syntax.OpConcat || len(re.Sub) < 2 {
		return false
	}
	first := re.Sub[0].Op
	last := re.Sub[len(re.Sub)-1].Op
	begins := first == syntax.OpBeginText || first == syntax.OpBeginLine
	ends := last == syntax.OpEndText || last == syntax.OpEndLine
	return begins && ends
}
