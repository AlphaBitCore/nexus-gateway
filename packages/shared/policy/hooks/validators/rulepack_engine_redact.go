package validators

import (
	"fmt"
	"sort"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// addressedText pairs a Normalized text segment with its span content address
// (e.g. "messages.0.content.1"), mirroring the projection walk so a redaction
// span can point at the exact block it masks.
type addressedText struct {
	address string
	text    string
	// blockType tags the flat ModifiedContent block this segment produces.
	// "text" (default) for message content / reasoning / tool-result text;
	// "tool_use" for a tool-call argument leaf, so the positional consumers
	// (contentBlocksToNormalized, applyModifiedContentToNormalized,
	// SpansFromModifiedContent) SKIP it — the structured TransformSpan carries
	// the masking and the flat text list stays aligned with the wire's text
	// slots (R1/R5: tool_use must never enter the flat list as text).
	blockType string
}

// addressedSegments walks the Normalized payload in projection order and returns
// every text segment with its content address. Mirrors the same Messages/Inputs
// traversal the text projection uses so the addresses line up with what the
// matcher scanned.
func (e *RulePackEngine) addressedSegments(input *core.HookInput) []addressedText {
	if input == nil || input.Normalized == nil {
		return nil
	}
	projOpts := e.cfg.ProjectionOptions()
	var out []addressedText
	if input.Normalized.Kind == normalize.KindAIEmbedding {
		for ii, inp := range input.Normalized.Inputs {
			if inp != "" {
				out = append(out, addressedText{address: fmt.Sprintf("inputs.%d", ii), text: inp})
			}
		}
		return out
	}
	for mi, m := range input.Normalized.Messages {
		for ci, b := range m.Content {
			switch b.Type {
			case normalize.ContentText:
				out = append(out, addressedText{address: fmt.Sprintf("messages.%d.content.%d", mi, ci), text: b.Text, blockType: "text"})
			case normalize.ContentReasoning:
				if projOpts.IncludeReasoning && b.Text != "" {
					out = append(out, addressedText{address: fmt.Sprintf("messages.%d.content.%d", mi, ci), text: b.Text, blockType: "text"})
				}
			case normalize.ContentToolResult:
				if b.ToolResult != nil {
					out = append(out, addressedText{address: fmt.Sprintf("messages.%d.content.%d.toolResult", mi, ci), text: b.ToolResult.Output, blockType: "text"})
				}
			case normalize.ContentToolUse:
				// One addressed segment per STRING leaf of the structured
				// tool-call Input, ordinal-addressed so resolveTextRef can
				// re-walk to the same leaf. Mirrors the projection order
				// exactly (both derive from ToolUseStringLeaves).
				if b.ToolUse != nil {
					for _, lf := range normalize.ToolUseStringLeaves(b.ToolUse.Input) {
						if lf.Value == "" {
							continue // mirrors projection's non-empty filter
						}
						out = append(out, addressedText{
							address:   fmt.Sprintf("messages.%d.content.%d.toolUse.input.%d", mi, ci, lf.Ordinal),
							text:      lf.Value,
							blockType: "tool_use",
						})
					}
				}
			}
		}
	}
	return out
}

// executeRedact converts every enforced match into a precise TransformSpan.
// Detection already ran on the fast cgo matcher (matched); here we re-localise
// only the rules that actually fired, with their cached RE2 pattern, over the
// addressed segments. Benign traffic matches nothing, so this RE2 path stays
// cold — detection keeps the cgo matcher's speed and redaction pays RE2 only for
// the few rules that hit. Severity still gates enforcement: warn/info matches
// only tag (observe), they do not mask.
func (e *RulePackEngine) executeRedact(input *core.HookInput, result *core.HookResult, matched map[[2]int]struct{}, complete bool, start time.Time) (*core.HookResult, error) {
	var matchedRules map[int]struct{}
	if complete {
		matchedRules = make(map[int]struct{}, len(matched))
		for k := range matched {
			matchedRules[k[0]] = struct{}{}
		}
	}
	addressed := e.addressedSegments(input)

	// perSeg accumulates each segment's redaction offsets so the redacted text
	// (ModifiedContent) can be built once at the end — the proxy rewrites the
	// forwarded + stored body from ModifiedContent, not from TransformSpans.
	type redaction struct {
		start, end  int
		replacement string
	}
	perSeg := make([][]redaction, len(addressed))

	var spans []normalize.TransformSpan
	for ri := range e.rules {
		cr := e.rules[ri]
		if complete {
			// Trust the matcher's hit set.
			if _, ok := matchedRules[ri]; !ok {
				continue
			}
		} else {
			// FAIL-SAFE: the scan truncated, so the matcher's hit set may be missing
			// this rule. Treat EVERY rule as a candidate and re-confirm with RE2
			// before tagging/masking — a dropped hit must never leave PII unmasked.
			re, err := core.CompilePattern(cr.rule.Pattern, cr.rule.Flags)
			if err != nil {
				continue
			}
			hit := false
			for _, seg := range addressed {
				if re.MatchString(seg.text) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		result.Tags = core.AppendTag(result.Tags, "rulepack:"+cr.packName)
		result.Tags = core.AppendTag(result.Tags, "rule:"+cr.rule.RuleID)
		if cr.rule.Category != "" {
			result.Tags = core.AppendTag(result.Tags, "category:"+cr.rule.Category)
		}
		for _, label := range cr.rule.Labels {
			result.Tags = core.AppendTag(result.Tags, label)
		}
		if !severityEnforces(cr.rule.Severity) {
			continue // warn/info/unknown severity → observe-only (tags), no mask
		}
		re, err := core.CompilePattern(cr.rule.Pattern, cr.rule.Flags)
		if err != nil {
			continue // unmappable pattern: never silently block, just skip masking
		}
		repl := core.ResolveReplacement(e.onMatch.Replacement, cr.rule.RuleID)
		ruleSpans := 0
		for si := range addressed {
			seg := addressed[si]
			for _, loc := range re.FindAllStringIndex(seg.text, -1) {
				spans = append(spans, normalize.TransformSpan{
					Source:         normalize.SourceHook,
					SourceID:       cr.rule.RuleID,
					Action:         normalize.ActionRedact,
					ContentAddress: seg.address,
					Start:          loc[0],
					End:            loc[1],
					Replacement:    repl,
				})
				perSeg[si] = append(perSeg[si], redaction{start: loc[0], end: loc[1], replacement: repl})
				ruleSpans++
			}
		}
		// Attribution: the first rule that actually masked content owns the
		// BlockingRule slot (audit "which rule triggered the redaction"), mirroring
		// the block path's first-match-wins attribution.
		if ruleSpans > 0 && result.BlockingRule == nil {
			result.BlockingRule = &core.BlockingRule{
				Pack:        cr.packName,
				PackVersion: cr.packVersion,
				RuleID:      cr.rule.RuleID,
				Category:    cr.rule.Category,
				Severity:    cr.rule.Severity,
				Labels:      append([]string(nil), cr.rule.Labels...),
			}
		}
	}

	if len(spans) > 0 {
		// Build the redacted content blocks the proxy rewrites the forwarded and
		// stored body from. Apply each segment's redactions in descending start
		// order so an earlier replacement never shifts a later offset; the spans
		// keep the original (pre-replacement) offsets.
		modified := make([]core.ContentBlock, len(addressed))
		for si := range addressed {
			text := addressed[si].text
			rs := perSeg[si]
			sort.Slice(rs, func(a, b int) bool { return rs[a].start > rs[b].start })
			for _, r := range rs {
				if r.start >= 0 && r.end <= len(text) && r.start < r.end {
					text = text[:r.start] + r.replacement + text[r.end:]
				}
			}
			bt := addressed[si].blockType
			if bt == "" {
				bt = "text"
			}
			modified[si] = core.ContentBlock{Role: "user", Type: bt, Text: text}
		}
		result.Decision = core.Modify
		result.Action = core.ActionRedact
		result.Reason = "rule-pack match: content redacted"
		result.ReasonCode = "RULEPACK_REDACTED"
		result.ModifiedContent = modified
		result.TransformSpans = spans
	}
	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// severityEnforces reports whether a rule severity makes a match ENFORCING —
// i.e. the bound hook's onMatch.Action is applied — versus observe-only (tags
// only, no decision change). hard and soft enforce; warn, info, the empty
// string, and any unknown value observe. Defaulting unknown/empty to
// observe-only is the typo-safe posture: a misspelled severity can never
// silently start enforcing on live traffic — the operator still sees the match
// via the `rule:…` tag. The action itself (redact vs block) is the hook's
// onMatch.Action, never derived from the severity tier.
func severityEnforces(s string) bool {
	switch s {
	case "hard", "soft":
		return true
	default:
		return false
	}
}
