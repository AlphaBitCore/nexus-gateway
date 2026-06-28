package validators

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// KeywordFilter scans normalised content against a set of regex patterns.
// All matches drive the same onMatch.action; per-pattern severity
// granularity requires rule packs instead. Matching is executed through the
// Matcher seam (Vectorscan under -tags vectorscan), with the per-pattern
// category metadata retained for the match reason.
// Applies to all text-carrying endpoints, text modality only, via
// the embedded TextOnlyContentScanning helper.
type KeywordFilter struct {
	core.TextOnlyContentScanning
	contentPrescan // raw-body prefilter (core.RawContentPrescanner)
	cfg            *core.HookConfig
	categories     []string // indexed by pattern ID
	matcher        matcher.Matcher
	caseSensitive  bool
	onMatch        core.OnMatchConfig
}

// NewKeywordFilter constructs a KeywordFilter from declarative config.
//
// Config shape:
//
//	{
//	  "patterns": [{"pattern": "regex", "category": "string"}],
//	  "caseSensitive": false,
//	  "onMatch": {"action":"block"}
//	}
//
// When `_rulePackInstalls` is attached to the config, the factory
// delegates entirely to NewRulePackEngine.
func NewKeywordFilter(cfg *core.HookConfig) (core.Hook, error) {
	if _, ok := cfg.Config["_rulePackInstalls"]; ok {
		return NewRulePackEngine(cfg)
	}
	caseSensitive, _ := cfg.Config["caseSensitive"].(bool)

	rawPatterns, ok := cfg.Config["patterns"]
	if !ok {
		return nil, fmt.Errorf("keyword-filter: missing 'patterns' in config")
	}
	patternList, ok := rawPatterns.([]any)
	if !ok {
		return nil, fmt.Errorf("keyword-filter: 'patterns' must be an array")
	}

	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("keyword-filter: %w", err)
	}

	flags := flagsForCaseSensitive(caseSensitive)
	categories := make([]string, 0, len(patternList))
	pats := make([]matcher.Pattern, 0, len(patternList))
	for i, raw := range patternList {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("keyword-filter: pattern[%d] must be an object", i)
		}
		pat, _ := m["pattern"].(string)
		if pat == "" {
			return nil, fmt.Errorf("keyword-filter: pattern[%d] has empty pattern string", i)
		}
		category, _ := m["category"].(string)
		pats = append(pats, matcher.Pattern{ID: len(categories), Expr: pat, Flags: flags})
		categories = append(categories, category)
	}

	// Reject at construction if any pattern cannot compile (keyword-filter's
	// historical strict posture — unlike rulepack-engine which skips). bad holds
	// only RE2-uncompilable patterns regardless of the active engine.
	mtch, bad := buildMatcher(pats)
	if len(bad) > 0 {
		return nil, fmt.Errorf("keyword-filter: pattern[%d] invalid regex: %w", bad[0].ID, bad[0].Err)
	}

	return &KeywordFilter{
		contentPrescan: newContentPrescan(pats),
		cfg:            cfg,
		categories:     categories,
		matcher:        mtch,
		caseSensitive:  caseSensitive,
		onMatch:        onMatch,
	}, nil
}

// Execute scans each text segment against all compiled patterns.
// First match wins and emits the onMatch-derived decision.
func (kf *KeywordFilter) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           kf.cfg.ID,
		ImplementationID: kf.cfg.ImplementationID,
		HookName:         kf.cfg.Name,
		Decision:         core.Approve,
	}

	segments := input.TextSegmentsWith(kf.cfg.ProjectionOptions())
	matched := matchedSet(kf.matcher, segments)
	core.ObserveContentScan(kf.cfg.ImplementationID, len(matched))

	// Segment-major, pattern-minor first-match-wins (preserved verbatim): the
	// matched-set lookup replaces the per-pattern MatchString call, so the engine
	// is swappable without changing which (segment,pattern) wins.
	for si := range segments {
		for pi := range kf.categories {
			if _, ok := matched[[2]int{pi, si}]; !ok {
				continue
			}
			result.Decision = core.DecisionForAction(kf.onMatch.Action)
			result.Reason = fmt.Sprintf("keyword matched: %s", kf.categories[pi])
			result.ReasonCode = "KEYWORD_BLOCKED"
			// Keyword matches carry no spans, so a redact/block action has
			// nothing to mask: the audit writer degrades to the drop
			// placeholder (fail-safe: never store what we cannot redact).
			result.Action = kf.onMatch.Action
			result.LatencyMs = int(time.Since(start).Milliseconds())
			return result, nil
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// Close releases the matcher's resources (the Vectorscan database, if any) when
// a config swap evicts this hook. No-op for the RE2 matcher.
func (kf *KeywordFilter) Close() error {
	_ = kf.closePrescan()
	if c, ok := kf.matcher.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// flagsForCaseSensitive maps the keyword-filter caseSensitive config bool
// onto core.CompilePattern's flag string: "" for case-sensitive, "i" for
// case-insensitive.
func flagsForCaseSensitive(caseSensitive bool) string {
	if caseSensitive {
		return ""
	}
	return "i"
}
