// Package rulepack: evaluator.go — pattern match loop over a prebuilt Pack.
package rulepack

import (
	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Evaluate applies rules against the text blocks and returns one Match
// per rule that hits. Patterns are cached via core.CompilePattern so the
// first build of a pack warms the process-wide LRU; subsequent calls are
// near-zero-alloc.
//
// Invalid regexes are silently skipped (logged at DEBUG by callers) —
// LoadYAML is the canonical validation surface; Evaluator must remain
// panic-free on corrupt input so a single bad rule in one pack does not
// break all evaluation.
//
// Match order is stable: iterates rules in input order, blocks in input
// order. MatchedText carries the first 256 chars of the first hit per
// rule — callers are responsible for further redaction if the match may
// carry PII / secrets.
func Evaluate(pack Pack, rules []Rule, blocks []core.ContentBlock) []Match {
	out := make([]Match, 0, len(rules))
	for _, r := range rules {
		re, err := core.CompilePattern(r.Pattern, r.Flags)
		if err != nil {
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
	return out
}
