// Package rulepack: lint.go — save-time linting for rule-pack patterns.
//
// The linter runs when an author saves a pattern (UI/API) and reports, beyond
// "does it compile": Vectorscan-compatibility hazards and performance hazards.
// It is engine-agnostic (pure Go RE2 + regexp/syntax AST analysis) so it runs
// anywhere; the cgo Vectorscan path can layer an exact hs_expression_info check
// on top, but the AST heuristics here already catch the high-value cases:
//
//   - HARD (won't run on the accelerator at all): backreference / lookaround —
//     RE2 itself rejects these, so they only ever reach the RE2 residual.
//   - DIVERGENCE: a bare end-anchor `$` (or `\Z`) outside multiline mode matches
//     before a trailing newline under Vectorscan (PCRE) but only at end-of-text
//     under RE2 — such a pattern must be routed to the RE2 residual, not served
//     by Vectorscan, or it will over-match.
//   - PERFORMANCE: a large/unbounded repeat over a wide character class with NO
//     required literal (e.g. `[A-Za-z0-9+/]{200,}`) — Vectorscan's literal
//     prefilter (Teddy/FDR) cannot accelerate it, so it scans the slow path on
//     every byte even on benign input. This is the class that turned out to
//     dominate scan cost in practice.
package rulepack

import (
	"fmt"
	"regexp/syntax"
)

// LintSeverity classifies a lint finding.
type LintSeverity string

const (
	// LintError: the pattern cannot be served by the accelerator at all (RE2
	// rejects it, or it cannot compile). Authoring should treat this as a hard
	// stop or an explicit residual-only acceptance.
	LintError LintSeverity = "error"
	// LintWarn: the pattern compiles but has a Vectorscan divergence or a
	// performance hazard the author should know about.
	LintWarn LintSeverity = "warn"
)

// LintFinding is one issue found in a pattern.
type LintFinding struct {
	Severity LintSeverity `json:"severity"`
	Code     string       `json:"code"`
	Message  string       `json:"message"`
}

// PatternLint is the full lint result for one pattern.
type PatternLint struct {
	Compiles bool          `json:"compiles"`
	Findings []LintFinding `json:"findings"`
}

// requiredLiteralRunLen returns the length (in runes) of the longest literal
// run that MUST appear in any match — the factor Vectorscan's literal prefilter
// keys on. 0 means the pattern has no guaranteed literal (prefilter-defeating).
func requiredLiteralRunLen(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass:
		// A caseless single letter parses to a 2-rune class (e.g. [Pp]); a
		// genuinely literal char. Hyperscan prefilters such caseless literals,
		// so count a small class (<=2 runes) as one literal char.
		if literalCharLen(re) == 1 {
			return 1
		}
		return 0
	case syntax.OpConcat:
		// Adjacent literal-equivalent chars concatenate; track the longest
		// contiguous run of guaranteed literals across the sequence.
		best, run := 0, 0
		for _, sub := range re.Sub {
			if lc := literalCharLen(sub); lc > 0 {
				run += lc
				if run > best {
					best = run
				}
				continue
			}
			run = 0
			if n := requiredLiteralRunLen(sub); n > best {
				best = n
			}
		}
		return best
	case syntax.OpAlternate:
		// A literal is guaranteed only if EVERY branch guarantees one.
		min := -1
		for _, sub := range re.Sub {
			n := requiredLiteralRunLen(sub)
			if min < 0 || n < min {
				min = n
			}
		}
		if min < 0 {
			return 0
		}
		return min
	case syntax.OpCapture:
		return requiredLiteralRunLen(re.Sub[0])
	case syntax.OpPlus:
		return requiredLiteralRunLen(re.Sub[0]) // 1+ → inner runs at least once
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return requiredLiteralRunLen(re.Sub[0])
		}
		return 0
	default:
		// Star / Quest / CharClass / AnyChar / anchors / etc.: not guaranteed.
		return 0
	}
}

// literalCharLen returns how many guaranteed literal characters a node
// contributes to a contiguous run: an OpLiteral contributes its rune count; a
// small char class (<=2 runes, i.e. a caseless single letter that Hyperscan can
// still prefilter) contributes 1; anything else contributes 0.
func literalCharLen(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass:
		runes := 0
		for i := 0; i+1 < len(re.Rune); i += 2 {
			runes += int(re.Rune[i+1]-re.Rune[i]) + 1
			if runes > 2 {
				return 0
			}
		}
		if runes >= 1 && runes <= 2 {
			return 1
		}
	}
	return 0
}

// wideRepeatNoLiteral reports whether the AST contains a large or unbounded
// repeat over a wide character class (>= wideClassMin distinct runes), which is
// the prefilter-defeating slow-path shape when the pattern as a whole has no
// required literal.
func hasWideUnanchoredRepeat(re *syntax.Regexp) bool {
	const wideClassMin = 16 // a char class spanning >=16 runes is "wide"
	// wideBoundedMax: a BOUNDED repeat whose upper bound exceeds this over a wide
	// class is also the slow path, not just unbounded ones. The email-PII rule's
	// original domain `[A-Za-z0-9.-]{1,255}` reads up to 255 arbitrary characters
	// after the `@`, so Vectorscan cannot key the literal prefilter and falls to a
	// per-position scan — measured 246µs/50KB, 140x the peer rules. The threshold
	// sits just above 64 (the RFC-5321 local-part limit) so the structured,
	// prefilterable form `[...]{1,64}@(?:[label].){1,8}[A-Za-z]{2,24}` — whose
	// widest char-class repeat is exactly {1,64} — does NOT trip, while a flat
	// {1,255}-style domain does. Precision over recall: a noisy lint that flags
	// good rules trains authors to ignore it.
	const wideBoundedMax = 64
	var walk func(*syntax.Regexp) bool
	walk = func(n *syntax.Regexp) bool {
		switch n.Op {
		case syntax.OpStar, syntax.OpPlus:
			if isWideClass(n.Sub[0], wideClassMin) {
				return true
			}
		case syntax.OpRepeat:
			// Unbounded ({n,}) OR a large bounded upper ({n,M} with M > 64) over a
			// wide class both defeat the literal prefilter. A tightly bounded repeat
			// (e.g. {2,24}) after even a 2-char literal prefilters acceptably.
			if (n.Max < 0 || n.Max > wideBoundedMax) && isWideClass(n.Sub[0], wideClassMin) {
				return true
			}
		}
		for _, sub := range n.Sub {
			if walk(sub) {
				return true
			}
		}
		return false
	}
	return walk(re)
}

// isWideClass reports whether re is a character class (or any-char) covering at
// least minRunes distinct runes.
func isWideClass(re *syntax.Regexp, minRunes int) bool {
	switch re.Op {
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return true
	case syntax.OpCharClass:
		total := 0
		for i := 0; i+1 < len(re.Rune); i += 2 {
			total += int(re.Rune[i+1]-re.Rune[i]) + 1
		}
		return total >= minRunes
	default:
		return false
	}
}

// classIncludesNewline reports whether a class/any-char node matches '\n'.
func classIncludesNewline(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpAnyChar:
		return true
	case syntax.OpCharClass:
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if re.Rune[i] <= '\n' && '\n' <= re.Rune[i+1] {
				return true
			}
		}
	}
	return false
}

// consumesNewline reports whether a repeat node can absorb a trailing newline
// (e.g. `\s*`), which makes a following `$` behave identically under Vectorscan
// and RE2 (no divergence).
func consumesNewline(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest:
		return classIncludesNewline(re.Sub[0])
	case syntax.OpRepeat:
		return classIncludesNewline(re.Sub[0])
	}
	return false
}

// hasDivergentEndAnchor reports whether the AST contains an end-of-text `$`
// (OpEndText) that is NOT guarded by a preceding newline-consuming repeat —
// i.e. one that actually diverges between Vectorscan (before-final-newline) and
// RE2 (end-of-text). `secret$` diverges; `secret\s*$` does not.
func hasDivergentEndAnchor(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpEndText:
		return true // standalone / unguarded end anchor
	case syntax.OpConcat:
		for i, sub := range re.Sub {
			if sub.Op == syntax.OpEndText {
				if i > 0 && consumesNewline(re.Sub[i-1]) {
					continue // guarded by e.g. \s* — safe
				}
				return true
			}
			if sub.Op != syntax.OpEndText && hasDivergentEndAnchorChild(sub) {
				return true
			}
		}
		return false
	default:
		return hasDivergentEndAnchorChild(re)
	}
}

// hasDivergentEndAnchorChild recurses into non-concat nodes.
func hasDivergentEndAnchorChild(re *syntax.Regexp) bool {
	if re.Op == syntax.OpEndText {
		return true
	}
	if re.Op == syntax.OpConcat {
		return hasDivergentEndAnchor(re)
	}
	for _, sub := range re.Sub {
		if hasDivergentEndAnchor(sub) {
			return true
		}
	}
	return false
}

// flagsToSyntax maps the rule-pack flag string (i/s/m/U) to syntax flags by
// wrapping the expression, matching how the runtime compiles patterns.
func wrapWithFlags(pattern, flags string) (string, error) {
	canon := ""
	seen := map[rune]bool{}
	for _, f := range flags {
		switch f {
		case 'i', 's', 'm', 'U':
			if !seen[f] {
				seen[f] = true
				canon += string(f)
			}
		default:
			return "", fmt.Errorf("unsupported regex flag %q", string(f))
		}
	}
	if canon == "" {
		return pattern, nil
	}
	return "(?" + canon + ")" + pattern, nil
}

// RuleLint pairs a rule id with its lint findings.
type RuleLint struct {
	RuleID   string        `json:"ruleId"`
	Findings []LintFinding `json:"findings"`
}

// LintPack lints every rule in a pack and returns only the rules that have
// findings, so authors see Vectorscan-compatibility + performance advisories at
// save/preview time alongside the parse warnings. Returns an empty (non-nil)
// slice when every rule is clean.
func LintPack(pack *Pack) []RuleLint {
	out := []RuleLint{}
	if pack == nil {
		return out
	}
	for _, r := range pack.Rules {
		l := LintPattern(r.Pattern, r.Flags)
		if len(l.Findings) > 0 {
			out = append(out, RuleLint{RuleID: r.RuleID, Findings: l.Findings})
		}
	}
	return out
}

// LintPattern lints a single rule pattern + flags and returns findings.
func LintPattern(pattern, flags string) PatternLint {
	out := PatternLint{}
	wrapped, ferr := wrapWithFlags(pattern, flags)
	if ferr != nil {
		out.Findings = append(out.Findings, LintFinding{LintError, "bad_flag", ferr.Error()})
		return out
	}
	re, err := syntax.Parse(wrapped, syntax.Perl)
	if err != nil {
		// RE2 rejects it (backreference, lookaround, bad syntax) — residual-only
		// at best; most likely an authoring error.
		out.Findings = append(out.Findings, LintFinding{
			LintError, "uncompilable",
			"pattern does not compile under RE2 (backreference / lookaround / syntax error): " + err.Error(),
		})
		return out
	}
	out.Compiles = true
	// NOTE: do not Simplify() — it factors common prefixes out of alternations
	// (e.g. `(?:password|passwd|pwd)` -> `p(?:assword|asswd|wd)`), which hides the
	// per-branch literals Hyperscan's multi-literal (Teddy) prefilter keys on and
	// produces false "no literal" warnings. The raw parse preserves them.

	multiline := false
	for _, f := range flags {
		if f == 'm' {
			multiline = true
		}
	}
	if !multiline && hasDivergentEndAnchor(re) {
		out.Findings = append(out.Findings, LintFinding{
			LintWarn, "anchor_divergence",
			"a bare `$` end-anchor matches before a trailing newline under Vectorscan but only at end-of-text under RE2; this pattern is routed to the RE2 residual to preserve semantics (use `\\s*$` or `(?m)` if a line anchor is intended)",
		})
	}
	if requiredLiteralRunLen(re) < 2 && hasWideUnanchoredRepeat(re) {
		out.Findings = append(out.Findings, LintFinding{
			LintWarn, "no_literal_prefilter",
			"large/unbounded repeat over a wide character class with no required literal (>=3 chars); Vectorscan's literal prefilter cannot accelerate it, so it scans the slow path on every request — anchor it to a literal, bound the repeat tightly, or detect this shape with a cheap length/entropy check instead of a regex",
		})
	}
	return out
}
