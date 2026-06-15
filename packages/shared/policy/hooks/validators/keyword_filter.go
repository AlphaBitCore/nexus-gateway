package validators

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// keywordPattern is a single compiled pattern with its metadata.
type keywordPattern struct {
	re       *regexp.Regexp
	category string
}

// KeywordFilter scans normalised content against a set of regex patterns.
// All matches drive the same onMatch.inflightAction; per-pattern severity
// granularity requires rule packs instead.
// Applies to all text-carrying endpoints, text modality only, via
// the embedded TextOnlyContentScanning helper.
type KeywordFilter struct {
	core.TextOnlyContentScanning
	cfg           *core.HookConfig
	patterns      []keywordPattern
	caseSensitive bool
	onMatch       core.OnMatchConfig
}

// NewKeywordFilter constructs a KeywordFilter from declarative config.
//
// Config shape:
//
//	{
//	  "patterns": [{"pattern": "regex", "category": "string"}],
//	  "caseSensitive": false,
//	  "onMatch": {"inflightAction":"block-hard", "storageAction":"redact"}
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

	compiled := make([]keywordPattern, 0, len(patternList))
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

		re, err := core.CompilePattern(pat, flagsForCaseSensitive(caseSensitive))
		if err != nil {
			return nil, fmt.Errorf("keyword-filter: pattern[%d] invalid regex %q: %w", i, pat, err)
		}
		compiled = append(compiled, keywordPattern{
			re:       re,
			category: category,
		})
	}

	return &KeywordFilter{
		cfg:           cfg,
		patterns:      compiled,
		caseSensitive: caseSensitive,
		onMatch:       onMatch,
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

	for _, text := range input.TextSegmentsWith(kf.cfg.ProjectionOptions()) {
		for idx := range kf.patterns {
			p := &kf.patterns[idx]
			if p.re.MatchString(text) {
				result.Decision = core.DecisionForInflight(kf.onMatch.InflightAction)
				result.Reason = fmt.Sprintf("keyword matched: %s", p.category)
				result.ReasonCode = "KEYWORD_BLOCKED"
				// Self-stamp the storage policy: the pipeline stamps it only
				// for non-Approve decisions, so an "approve inflight,
				// redact/drop storage" match would otherwise persist the
				// matched content. Keyword matches carry no spans, so a
				// redact storage policy degrades to drop-content at the
				// audit writer (fail-safe: never store what we cannot redact).
				result.StorageAction = kf.onMatch.StorageAction
				result.LatencyMs = int(time.Since(start).Milliseconds())
				return result, nil
			}
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
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
