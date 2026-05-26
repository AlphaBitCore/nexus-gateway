package validators

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// categoryKeywords maps each content-safety category to its default detection keywords.
// These are compiled into case-insensitive word-boundary patterns at hook creation time.
var categoryKeywords = map[string][]string{
	"violence": {
		"kill", "murder", "attack", "assault", "bomb", "weapon",
		"shoot", "stab", "execute", "slaughter", "massacre",
	},
	"hate_speech": {
		"hate speech", "racial slur", "bigotry", "supremacist",
		"ethnic cleansing", "genocide", "discrimination",
	},
	"self_harm": {
		"self-harm", "suicide", "cut myself", "end my life",
		"self injury", "self mutilation",
	},
	"sexual": {
		"explicit sexual", "pornography", "sexual content",
		"nude", "sexually explicit",
	},
	"illegal": {
		"illegal drug", "drug trafficking", "money laundering",
		"human trafficking", "terrorism", "fraud scheme",
		"counterfeit", "smuggling",
	},
}

// categoryPattern is a compiled pattern set for one category.
type categoryPattern struct {
	name     string
	patterns []*regexp.Regexp
}

// ContentSafety evaluates content against category-based keyword lists.
// The decision on match is derived from onMatch.inflightAction.
// Applies to all text-carrying endpoints, text modality only, via
// the embedded TextOnlyContentScanning helper.
type ContentSafety struct {
	core.TextOnlyContentScanning
	cfg        *core.HookConfig
	categories []categoryPattern
	onMatch    core.OnMatchConfig
}

// NewContentSafety constructs a ContentSafety hook from declarative config.
//
// Config shape:
//
//	{
//	  "categories": {"violence": true, "hate_speech": true},
//	  "onMatch": {"inflightAction":"block-hard","storageAction":"redact"}
//	}
//
// When `_rulePackInstalls` is attached the factory delegates to
// NewRulePackEngine for unified rule-pack evaluation.
func NewContentSafety(cfg *core.HookConfig) (core.Hook, error) {
	if _, ok := cfg.Config["_rulePackInstalls"]; ok {
		return NewRulePackEngine(cfg)
	}
	rawCats, ok := cfg.Config["categories"]
	if !ok {
		return nil, fmt.Errorf("content-safety: missing 'categories' in config")
	}
	catMap, ok := rawCats.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("content-safety: 'categories' must be a map")
	}

	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("content-safety: %w", err)
	}

	var cats []categoryPattern
	for name, rawEnabled := range catMap {
		enabled, _ := rawEnabled.(bool)
		if !enabled {
			continue
		}
		keywords, found := categoryKeywords[name]
		if !found {
			return nil, fmt.Errorf("content-safety: unknown category %q", name)
		}
		var compiled []*regexp.Regexp
		for _, kw := range keywords {
			pattern := `\b` + regexp.QuoteMeta(kw) + `\b`
			re, err := core.CompilePattern(pattern, "i")
			if err != nil {
				return nil, fmt.Errorf("content-safety: failed to compile pattern for %q keyword %q: %w", name, kw, err)
			}
			compiled = append(compiled, re)
		}
		cats = append(cats, categoryPattern{name: name, patterns: compiled})
	}

	return &ContentSafety{
		cfg:        cfg,
		categories: cats,
		onMatch:    onMatch,
	}, nil
}

// Execute scans content blocks against all enabled category keyword lists.
func (cs *ContentSafety) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           cs.cfg.ID,
		ImplementationID: cs.cfg.ImplementationID,
		HookName:         cs.cfg.Name,
		Decision:         core.Approve,
	}

	for _, text := range input.TextSegmentsWith(cs.cfg.ProjectionOptions()) {
		for idx := range cs.categories {
			cat := &cs.categories[idx]
			for _, re := range cat.patterns {
				if re.MatchString(text) {
					result.Decision = core.DecisionForInflight(cs.onMatch.InflightAction)
					result.Reason = fmt.Sprintf("content safety violation: %s", cat.name)
					result.ReasonCode = "CONTENT_SAFETY_VIOLATION"
					result.Tags = core.AppendTag(result.Tags, "severity:restricted")
					result.Tags = core.AppendTag(result.Tags, "detector:content-safety")
					result.Tags = core.AppendTag(result.Tags, "category:"+cat.name)
					result.LatencyMs = int(time.Since(start).Milliseconds())
					return result, nil
				}
			}
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}
