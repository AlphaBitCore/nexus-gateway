package validators

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// piiPattern is a compiled PII detection rule.
type piiPattern struct {
	id          string
	re          *regexp.Regexp
	luhn        bool   // when true, matches are additionally validated with the Luhn algorithm
	replacement string // replacement text used when action is "redact"
}

// PiiDetector scans content for personally identifiable information and
// dispatches on onMatch.action: approve detects for tagging only, redact
// rewrites matches in-place, block rejects (and still masks the stored copy).
// Applies to all text-carrying endpoints (chat, embeddings, stt,
// image_generation, tts, video_generation), text modality only, via the
// embedded TextOnlyContentScanning helper.
type PiiDetector struct {
	core.TextOnlyContentScanning
	contentPrescan // raw-body prefilter (core.RawContentPrescanner)
	cfg            *core.HookConfig
	patterns       []piiPattern
	// matcher runs the detection pass (Vectorscan under -tags vectorscan): one
	// scan over all patterns gates whether any pattern fired at all. On benign
	// traffic (the common case) it short-circuits to Approve with zero RE2 work.
	// The per-pattern RE2 in `patterns` is still used for precise redaction
	// offsets + Luhn validation on the matched subset — Vectorscan reports no
	// sub-match spans, and Luhn needs the matched text (locked hybrid: Vectorscan
	// detects, RE2 extracts).
	matcher matcher.Matcher
	onMatch core.OnMatchConfig
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
//	    "action":      "approve"|"redact"|"block",
//	    "replacement": "[REDACTED_<RULE_ID>]"
//	  }
//	}
//
// Per-pattern `replacement` overrides onMatch.Replacement for that pattern's
// hits in redact/block mode. action defaults to block.
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
	pats := make([]matcher.Pattern, 0, len(patternList))
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
		pats = append(pats, matcher.Pattern{ID: len(pats), Expr: regexSrc, Flags: cacheFlags})
	}

	// The detection matcher mirrors the per-pattern RE2 set; since every pattern
	// already RE2-compiled above, no pattern is bad here.
	mtch, _ := buildMatcher(pats)

	return &PiiDetector{
		contentPrescan: newContentPrescan(pats),
		cfg:            cfg,
		patterns:       patterns,
		matcher:        mtch,
		onMatch:        onMatch,
	}, nil
}

// Close releases the detection matcher's resources (the Vectorscan database, if
// any) when a config swap evicts this hook. No-op for the RE2 matcher.
func (pd *PiiDetector) Close() error {
	_ = pd.closePrescan()
	if c, ok := pd.matcher.(io.Closer); ok {
		return c.Close()
	}
	return nil
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

// Execute scans content blocks for PII matches and dispatches on the hook's
// single onMatch.action:
//   - redact: rewrite the payload (Modify) and emit the spans that mask the
//     forwarded / returned / stored copies.
//   - block:  reject (RejectHard) and emit the spans that mask the stored
//     redacted copy (the request itself is rejected, never forwarded).
//   - approve: detect for tagging only — forward and store as-is.
func (pd *PiiDetector) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           pd.cfg.ID,
		ImplementationID: pd.cfg.ImplementationID,
		HookName:         pd.cfg.Name,
		Decision:         core.Approve,
	}

	// Detection gate (Vectorscan under -tags vectorscan): one scan over all
	// patterns. If nothing fired, no PII is present — return Approve without any
	// RE2 work (the benign hot path). Only when a pattern fires do we run the
	// per-pattern RE2 below for precise offsets + Luhn validation. A pattern that
	// fires here but fails Luhn still resolves to Approve via the action paths,
	// so the gate never over-reports.
	gateMatched := matchedSet(pd.matcher, input.TextSegmentsWith(pd.cfg.ProjectionOptions()))
	core.ObserveContentScan(pd.cfg.ImplementationID, len(gateMatched))
	if len(gateMatched) == 0 {
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	switch pd.onMatch.Action {
	case core.ActionRedact:
		return pd.executeRedact(input, result, start)
	case core.ActionApprove:
		return pd.executeApprove(input, result, start)
	default: // ActionBlock
		return pd.executeBlock(input, result, start)
	}
}

// executeApprove detects PII for tagging only and leaves the payload
// untouched (Approve forwards and stores as-is). Short-circuits on the first
// match; no spans are collected.
func (pd *PiiDetector) executeApprove(input *core.HookInput, result *core.HookResult, start time.Time) (*core.HookResult, error) {
	for _, text := range input.TextSegmentsWith(pd.cfg.ProjectionOptions()) {
		for idx := range pd.patterns {
			p := &pd.patterns[idx]
			for _, match := range p.re.FindAllString(text, -1) {
				if p.luhn && !luhnValid(match) {
					continue
				}
				result.Reason = fmt.Sprintf("PII detected: %s", p.id)
				result.ReasonCode = "PII_DETECTED"
				result.Tags = core.AppendTag(result.Tags, "compliance:pii")
				result.Tags = core.AppendTag(result.Tags, "severity:confidential")
				result.LatencyMs = int(time.Since(start).Milliseconds())
				return result, nil
			}
		}
	}
	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// executeBlock rejects on a match and collects the TransformSpans that drive
// the stored-copy redaction (block stores the redacted copy so an auditor can
// review what tripped without the raw sensitive bytes).
func (pd *PiiDetector) executeBlock(input *core.HookInput, result *core.HookResult, start time.Time) (*core.HookResult, error) {
	_, spans := pd.collectRedactions(input)
	if len(spans) > 0 {
		result.Decision = core.RejectHard
		result.Reason = fmt.Sprintf("PII detected: %s", spans[0].SourceID)
		result.ReasonCode = "PII_DETECTED"
		result.Tags = core.AppendTag(result.Tags, "compliance:pii")
		result.Tags = core.AppendTag(result.Tags, "severity:confidential")
		result.TransformSpans = spans
		result.Action = core.ActionBlock
	}
	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// executeRedact replaces all PII matches across the projection and emits
// structured TransformSpans alongside the transitional ModifiedContent. Spans
// precisely address each redacted byte range (Source=hook, SourceID=pattern.id,
// Action=redact); the same masked body is forwarded, returned, and stored.
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
		result.Action = core.ActionRedact
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}
