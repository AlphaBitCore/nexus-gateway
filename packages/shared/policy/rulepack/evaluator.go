// Package rulepack: evaluator.go — pattern match loop over a prebuilt Pack.
package rulepack

import (
	"fmt"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Evaluate applies rules against the text blocks and returns one Match
// per rule that hits. Patterns are cached via core.CompilePattern so the
// first build of a pack warms the process-wide LRU; subsequent calls are
// near-zero-alloc.
//
// Invalid regexes are silently skipped — this is the runtime hot-path
// variant where availability matters more than diagnostics (LoadYAML /
// ValidatePack is the canonical authoring gate, so a runtime rule that
// fails to compile already slipped past validation). Operator-facing
// dry-run / preview surfaces MUST call EvaluateWithErrors instead so a
// broken pattern is reported rather than swallowed.
//
// Match order is stable: iterates rules in input order, blocks in input
// order. MatchedText carries the first 256 chars of the first hit per
// rule — callers are responsible for further redaction if the match may
// carry PII / secrets.
func Evaluate(pack Pack, rules []Rule, blocks []core.ContentBlock) []Match {
	matches, _ := EvaluateWithErrors(pack, rules, blocks)
	return matches
}

// EvaluateWithErrors is the dry-run / preview variant of Evaluate. It returns
// the same Match list AND a per-rule compile-error list so an operator testing
// a rulepack learns that a pattern is broken instead of seeing it silently
// "never match". A rule whose pattern fails to compile contributes a RuleError
// (carrying its index, ruleId, and the compiler message) and is skipped for
// matching — exactly the failure the dry-run API previously swallowed.
//
// compileErrs is nil (not an empty slice) when every rule compiled, so callers
// can branch on `len(compileErrs) > 0` or `compileErrs == nil` interchangeably.
func EvaluateWithErrors(pack Pack, rules []Rule, blocks []core.ContentBlock) (matches []Match, compileErrs []RuleError) {
	out := make([]Match, 0, len(rules))
	for ri, r := range rules {
		re, err := core.CompilePattern(r.Pattern, r.Flags)
		if err != nil {
			compileErrs = append(compileErrs, RuleError{
				Index:  ri,
				RuleID: r.RuleID,
				Reason: fmt.Sprintf("invalid pattern: %v", err),
			})
			continue
		}
		for _, b := range blocks {
			if b.Type != "" && b.Type != "text" {
				continue
			}
			loc := re.FindStringIndex(b.Text)
			if loc == nil {
				continue
			}
			matched := b.Text[loc[0]:loc[1]]
			if len(matched) > 256 {
				matched = matched[:256]
			}
			out = append(out, Match{
				PackName:    pack.Name,
				PackVersion: pack.Version,
				RuleLocalID: r.RuleID,
				Category:    r.Category,
				Severity:    r.Severity,
				Labels:      append([]string(nil), r.Labels...),
				MatchedText: matched,
			})
			break // one match per rule per evaluator call — sufficient for audit
		}
	}
	return out, compileErrs
}
