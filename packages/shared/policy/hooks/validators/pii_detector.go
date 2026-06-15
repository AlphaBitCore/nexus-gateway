package validators

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// piiPattern is a compiled PII detection rule.
type piiPattern struct {
	id          string
	re          *regexp.Regexp
	luhn        bool   // when true, matches are additionally validated with the Luhn algorithm
	replacement string // replacement text used when action is "redact"
}

// PiiDetector scans content for personally identifiable information and
// either short-circuits the pipeline with REJECT_* or replaces matches
// in-place. The pipeline decision is derived from onMatch.inflightAction.
// Applies to all text-carrying endpoints (chat, embeddings, stt,
// image_generation, tts, video_generation), text modality only, via the
// embedded TextOnlyContentScanning helper.
type PiiDetector struct {
	core.TextOnlyContentScanning
	cfg      *core.HookConfig
	patterns []piiPattern
	onMatch  core.OnMatchConfig
}

// NewPiiDetector constructs a PiiDetector from declarative config.
//
// Config shape:
//
//	{
//	  "patternDefinitions": [
//	    {"id":"email","regex":"\\b[...]\\b","flags":"i","luhn":false}
//	  ],
//	  "onMatch": {
//	    "inflightAction": "block-hard"|"block-soft"|"redact"|"approve",
//	    "storageAction":  "redact"|"keep"|"drop-content",
//	    "replacement":    "[REDACTED_<RULE_ID>]"
//	  }
//	}
//
// Per-pattern `replacement` overrides onMatch.Replacement for that pattern's
// hits in redact mode. inflightAction defaults to block-hard.
//
// When `_rulePackInstalls` is attached the factory delegates to
// NewRulePackEngine.
func NewPiiDetector(cfg *core.HookConfig) (core.Hook, error) {
	if _, ok := cfg.Config["_rulePackInstalls"]; ok {
		return NewRulePackEngine(cfg)
	}
	rawPatterns, ok := cfg.Config["patternDefinitions"]
	if !ok {
		return nil, fmt.Errorf("pii-detector: 'patternDefinitions' is required")
	}
	patternList, ok := rawPatterns.([]any)
	if !ok {
		return nil, fmt.Errorf("pii-detector: 'patternDefinitions' must be an array")
	}

	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("pii-detector: %w", err)
	}

	patterns := make([]piiPattern, 0, len(patternList))
	for i, raw := range patternList {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pii-detector: patternDefinitions[%d] must be an object", i)
		}

		id, _ := m["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("pii-detector: patternDefinitions[%d] 'id' is required", i)
		}

		regexSrc, _ := m["regex"].(string)
		if regexSrc == "" {
			return nil, fmt.Errorf("pii-detector: patternDefinitions[%d] 'regex' is required", i)
		}

		flagsStr, _ := m["flags"].(string)
		cacheFlags, err := translatePiiFlagsForCache(flagsStr)
		if err != nil {
			return nil, fmt.Errorf("pii-detector: patternDefinitions[%d] %w", i, err)
		}

		re, err := core.CompilePattern(regexSrc, cacheFlags)
		if err != nil {
			return nil, fmt.Errorf("pii-detector: patternDefinitions[%d] invalid regex: %w", i, err)
		}

		luhn, _ := m["luhn"].(bool)

		// Per-pattern replacement takes precedence over onMatch template.
		replacement, _ := m["replacement"].(string)
		if replacement == "" {
			replacement = core.ResolveReplacement(onMatch.Replacement, id)
		}

		patterns = append(patterns, piiPattern{
			id:          id,
			re:          re,
			luhn:        luhn,
			replacement: replacement,
		})
	}

	return &PiiDetector{
		cfg:      cfg,
		patterns: patterns,
		onMatch:  onMatch,
	}, nil
}

// translatePiiFlagsForCache converts pii-detector's JS-style flag string into
// the subset accepted by core.CompilePattern. 'g' is silently stripped (Go's
// FindAll is globally-scoped by default); i/m/s pass through as flag letters;
// duplicates collapse; any other flag character returns an error.
func translatePiiFlagsForCache(flags string) (string, error) {
	if flags == "" {
		return "", nil
	}
	seen := map[rune]bool{}
	var out []rune
	for _, f := range flags {
		if seen[f] {
			continue
		}
		seen[f] = true
		switch f {
		case 'g':
			// Go's FindAllString already matches all occurrences — g is a no-op.
		case 'i', 'm', 's':
			out = append(out, f)
		default:
			return "", fmt.Errorf("unsupported flag %q (supported: g, i, m, s)", string(f))
		}
	}
	return string(out), nil
}

// Execute scans content blocks for PII matches.
func (pd *PiiDetector) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           pd.cfg.ID,
		ImplementationID: pd.cfg.ImplementationID,
		HookName:         pd.cfg.Name,
		Decision:         core.Approve,
	}

	if pd.onMatch.InflightAction == core.InflightRedact {
		return pd.executeRedact(input, result, start)
	}
	return pd.executeScan(input, result, start)
}

// executeScan implements the non-rewriting inflight paths (approve /
// block-hard / block-soft). The inflight body is never modified, but a match
// still stamps the hook's own storageAction onto the result — the pipeline
// only stamps it for non-Approve decisions, so without the self-stamp an
// "approve inflight, redact/drop storage" policy would silently persist the
// matched content. When storageAction is redact the scan additionally
// collects the full TransformSpan set (the same collector the inflight
// redact path uses): the storage rewrite is span-driven, and with no spans
// it would have nothing to apply.
func (pd *PiiDetector) executeScan(input *core.HookInput, result *core.HookResult, start time.Time) (*core.HookResult, error) {
	if pd.onMatch.StorageAction == core.StorageRedact {
		_, spans := pd.collectRedactions(input)
		if len(spans) > 0 {
			result.Decision = core.DecisionForInflight(pd.onMatch.InflightAction)
			result.Reason = fmt.Sprintf("PII detected: %s", spans[0].SourceID)
			result.ReasonCode = "PII_DETECTED"
			result.Tags = core.AppendTag(result.Tags, "compliance:pii")
			result.Tags = core.AppendTag(result.Tags, "severity:confidential")
			result.TransformSpans = spans
			result.StorageAction = pd.onMatch.StorageAction
		}
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	// keep / drop-content storage needs no spans — short-circuit on first match.
	for _, text := range input.TextSegmentsWith(pd.cfg.ProjectionOptions()) {
		for idx := range pd.patterns {
			p := &pd.patterns[idx]
			matches := p.re.FindAllString(text, -1)
			for _, match := range matches {
				if p.luhn && !luhnValid(match) {
					continue
				}
				result.Decision = core.DecisionForInflight(pd.onMatch.InflightAction)
				result.Reason = fmt.Sprintf("PII detected: %s", p.id)
				result.ReasonCode = "PII_DETECTED"
				result.Tags = core.AppendTag(result.Tags, "compliance:pii")
				result.Tags = core.AppendTag(result.Tags, "severity:confidential")
				result.StorageAction = pd.onMatch.StorageAction
				result.LatencyMs = int(time.Since(start).Milliseconds())
				return result, nil
			}
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// executeRedact replaces all PII matches across the projection and
// emits structured TransformSpans alongside the transitional
// ModifiedContent. Spans precisely address each redacted byte range
// (Source=hook, SourceID=pattern.id, Action=redact); pipeline storage
// rewrite and inflight rewrite both consume the spans uniformly.
// ModifiedContent stays for ai-gateway's existing RewriteRequestBody
// path until cp/agent fully adopt the span-driven rewrite.
func (pd *PiiDetector) executeRedact(input *core.HookInput, result *core.HookResult, start time.Time) (*core.HookResult, error) {
	modified, spans := pd.collectRedactions(input)

	if len(spans) > 0 {
		result.Decision = core.Modify
		result.Reason = "PII redacted"
		result.ReasonCode = "PII_REDACTED"
		result.Tags = core.AppendTag(result.Tags, "compliance:pii")
		result.Tags = core.AppendTag(result.Tags, "severity:confidential")
		result.ModifiedContent = modified
		result.TransformSpans = spans
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// RedetectText re-locates this detector's matches within one text block of
// a storage-bound payload, restricted to the requested rule IDs. It backs
// the storage-time redaction retry: hook-time span addresses can fail to
// resolve on the storage-time normalized payload (cross-format requests
// project the same content at different addresses), and the audit writer
// has no access to the compiled patterns — the pipeline exports them
// through this method as a redact.Redetector closure on its result.
// Matches carry offsets, rule attribution, and the replacement marker
// only — never the matched content.
func (pd *PiiDetector) RedetectText(text string, ruleIDs []string) []redact.Match {
	if text == "" || len(ruleIDs) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(ruleIDs))
	for _, id := range ruleIDs {
		want[id] = struct{}{}
	}
	var out []redact.Match
	for i := range pd.patterns {
		p := &pd.patterns[i]
		if _, ok := want[p.id]; !ok {
			continue
		}
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			if p.luhn && !luhnValid(text[loc[0]:loc[1]]) {
				continue
			}
			out = append(out, redact.Match{RuleID: p.id, Start: loc[0], End: loc[1], Replacement: p.replacement})
		}
	}
	return out
}

// addressedSegment pairs a projection text segment with its span content
// address inside the Normalized payload.
type addressedSegment struct {
	address string
	text    string
}

// collectRedactions walks the Normalized payload in projection order and
// returns the redacted content blocks plus the TransformSpans addressing
// every match (Source=hook, SourceID=pattern.id, Action=redact, offsets in
// the original text). Empty spans means no match (or no normalized input).
// Shared by the inflight-redact path and the storage-only-redact scan.
func (pd *PiiDetector) collectRedactions(input *core.HookInput) ([]core.ContentBlock, []normalize.TransformSpan) {
	if input.Normalized == nil {
		return nil, nil
	}
	projOpts := pd.cfg.ProjectionOptions()
	segments := input.TextSegmentsWith(projOpts)
	if len(segments) == 0 {
		return nil, nil
	}

	// Walk the Normalized payload in projection order so spans get the
	// right content addresses. The walk mirrors the projection: reasoning
	// blocks join only when the hook's scope opted in (IncludeReasoning),
	// matching what TextSegmentsWith exposed above.
	addressed := make([]addressedSegment, 0, len(segments))
	// KindAIEmbedding payloads carry text in Inputs (not Messages).
	// Address each input as "inputs.<index>" so span tracking is accurate.
	if input.Normalized.Kind == normalize.KindAIEmbedding {
		for ii, inp := range input.Normalized.Inputs {
			if inp != "" {
				addressed = append(addressed, addressedSegment{
					address: fmt.Sprintf("inputs.%d", ii),
					text:    inp,
				})
			}
		}
	} else {
		for mi, m := range input.Normalized.Messages {
			for ci, b := range m.Content {
				switch b.Type {
				case normalize.ContentText:
					addressed = append(addressed, addressedSegment{
						address: fmt.Sprintf("messages.%d.content.%d", mi, ci),
						text:    b.Text,
					})
				case normalize.ContentReasoning:
					if projOpts.IncludeReasoning && b.Text != "" {
						addressed = append(addressed, addressedSegment{
							address: fmt.Sprintf("messages.%d.content.%d", mi, ci),
							text:    b.Text,
						})
					}
				case normalize.ContentToolResult:
					if b.ToolResult != nil {
						addressed = append(addressed, addressedSegment{
							address: fmt.Sprintf("messages.%d.content.%d.toolResult", mi, ci),
							text:    b.ToolResult.Output,
						})
					}
				}
			}
		}
	}

	modified := make([]core.ContentBlock, len(addressed))
	spans := make([]normalize.TransformSpan, 0)

	for i, seg := range addressed {
		text := seg.text
		// Collect per-pattern match offsets in *original* text so spans
		// reference the pre-replacement byte ranges; apply replacements
		// to the working text in descending offset order.
		type segMatch struct {
			ruleID, replacement string
			start, end          int
		}
		var matches []segMatch
		for idx := range pd.patterns {
			p := &pd.patterns[idx]
			for _, loc := range p.re.FindAllStringIndex(seg.text, -1) {
				if p.luhn && !luhnValid(seg.text[loc[0]:loc[1]]) {
					continue
				}
				matches = append(matches, segMatch{
					ruleID:      p.id,
					replacement: p.replacement,
					start:       loc[0],
					end:         loc[1],
				})
			}
		}
		// Sort matches by descending start so successive replacements
		// don't shift earlier offsets.
		for a := 1; a < len(matches); a++ {
			for b := a; b > 0 && matches[b].start > matches[b-1].start; b-- {
				matches[b], matches[b-1] = matches[b-1], matches[b]
			}
		}
		for _, m := range matches {
			text = text[:m.start] + m.replacement + text[m.end:]
			spans = append(spans, normalize.TransformSpan{
				Source:         normalize.SourceHook,
				SourceID:       m.ruleID,
				Action:         normalize.ActionRedact,
				ContentAddress: seg.address,
				Start:          m.start,
				End:            m.end,
				Replacement:    m.replacement,
			})
		}
		modified[i] = core.ContentBlock{Role: "user", Type: "text", Text: text}
	}

	if len(spans) == 0 {
		return nil, nil
	}
	return modified, spans
}

// luhnValid checks a numeric string (ignoring spaces and hyphens) with the Luhn algorithm.
func luhnValid(s string) bool {
	// Pre-allocate for typical card number length (16 digits + separators).
	digits := make([]int, 0, 20)
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			digits = append(digits, int(ch-'0'))
		}
	}
	if len(digits) == 0 {
		return false
	}

	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}
