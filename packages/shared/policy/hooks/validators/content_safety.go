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

// ContentSafety evaluates content against a compiled set of patterns, each
// attributed to a category and severity. Patterns come from one of two
// declarative sources (see NewContentSafety): inline `patterns` (the
// config-visible, editable built-in default — pack-quality contextual regex)
// or the legacy `categories` toggles backed by categoryKeywords. The decision
// on match is derived from onMatch.action. Matching is executed through
// the Matcher seam (Vectorscan under -tags vectorscan). Applies to all
// text-carrying endpoints, text modality only, via the embedded
// TextOnlyContentScanning helper.
type ContentSafety struct {
	core.TextOnlyContentScanning
	contentPrescan // raw-body prefilter (core.RawContentPrescanner)
	cfg            *core.HookConfig
	patCategory    []string // category attributed to each compiled pattern (by ID)
	patSeverity    []string // severity tag for each compiled pattern (by ID)
	matcher        matcher.Matcher
	onMatch        core.OnMatchConfig
}

// NewContentSafety constructs a ContentSafety hook from declarative config.
//
// Three sources, in priority order:
//
//  1. `_rulePackInstalls` bound → delegates to NewRulePackEngine (an admin
//     chose a rule pack; it takes over, like keyword-filter).
//
//  2. inline `patterns` → the config-visible built-in default, used when no
//     rule pack is chosen. Each entry is {pattern, category, severity?}; this
//     is where the pack-quality contextual regex lives so admins can see/edit
//     it on the hook itself.
//
//  3. legacy `categories` toggles → categoryKeywords fallback (backward compat).
//
//     {
//     "patterns": [{"pattern":"(?i)\\bhow to murder\\b","category":"violence","severity":"soft"}],
//     "onMatch": {"action":"block"}
//     }
func NewContentSafety(cfg *core.HookConfig) (core.Hook, error) {
	if _, ok := cfg.Config["_rulePackInstalls"]; ok {
		return NewRulePackEngine(cfg)
	}

	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("content-safety: %w", err)
	}

	var (
		pats        []matcher.Pattern
		patCategory []string
		patSeverity []string
	)

	if rawPatterns, ok := cfg.Config["patterns"]; ok {
		patternList, ok := rawPatterns.([]any)
		if !ok {
			return nil, fmt.Errorf("content-safety: 'patterns' must be an array")
		}
		for i, raw := range patternList {
			m, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content-safety: pattern[%d] must be an object", i)
			}
			expr, _ := m["pattern"].(string)
			if expr == "" {
				return nil, fmt.Errorf("content-safety: pattern[%d] has empty pattern string", i)
			}
			category, _ := m["category"].(string)
			if category == "" {
				category = "content_safety"
			}
			severity, _ := m["severity"].(string)
			if severity == "" {
				severity = "restricted"
			}
			pats = append(pats, matcher.Pattern{ID: len(pats), Expr: expr, Flags: "i"})
			patCategory = append(patCategory, category)
			patSeverity = append(patSeverity, severity)
		}
	} else {
		rawCats, ok := cfg.Config["categories"]
		if !ok {
			return nil, fmt.Errorf("content-safety: config needs 'patterns' or 'categories'")
		}
		catMap, ok := rawCats.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content-safety: 'categories' must be a map")
		}
		for name, rawEnabled := range catMap {
			enabled, _ := rawEnabled.(bool)
			if !enabled {
				continue
			}
			keywords, found := categoryKeywords[name]
			if !found {
				return nil, fmt.Errorf("content-safety: unknown category %q", name)
			}
			for _, kw := range keywords {
				pats = append(pats, matcher.Pattern{ID: len(pats), Expr: `\b` + regexp.QuoteMeta(kw) + `\b`, Flags: "i"})
				patCategory = append(patCategory, name)
				patSeverity = append(patSeverity, "restricted")
			}
		}
	}

	mtch, bad := buildMatcher(pats)
	if len(bad) > 0 {
		return nil, fmt.Errorf("content-safety: failed to compile pattern id %d: %w", bad[0].ID, bad[0].Err)
	}

	return &ContentSafety{
		contentPrescan: newContentPrescan(pats),
		cfg:            cfg,
		patCategory:    patCategory,
		patSeverity:    patSeverity,
		matcher:        mtch,
		onMatch:        onMatch,
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

	segments := input.TextSegmentsWith(cs.cfg.ProjectionOptions())
	matched := matchedSet(cs.matcher, segments)
	core.ObserveContentScan(cs.cfg.ImplementationID, len(matched))

	// Segment-major, pattern-minor first-match-wins: pattern IDs are assigned in
	// config order, so the earliest configured pattern that hits a segment wins
	// — and is attributed back to its own category + severity.
	for si := range segments {
		for pid := range cs.patCategory {
			if _, ok := matched[[2]int{pid, si}]; !ok {
				continue
			}
			result.Decision = core.DecisionForAction(cs.onMatch.Action)
			result.Reason = fmt.Sprintf("content safety violation: %s", cs.patCategory[pid])
			result.ReasonCode = "CONTENT_SAFETY_VIOLATION"
			result.Tags = core.AppendTag(result.Tags, "severity:"+cs.patSeverity[pid])
			result.Tags = core.AppendTag(result.Tags, "detector:content-safety")
			result.Tags = core.AppendTag(result.Tags, "category:"+cs.patCategory[pid])
			// This detector produces no spans, so a redact/block match
			// has nothing to mask: the audit writer degrades to the drop
			// placeholder (never store what we cannot redact).
			result.Action = cs.onMatch.Action
			result.LatencyMs = int(time.Since(start).Milliseconds())
			return result, nil
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// Close releases the matcher's resources (the Vectorscan database, if any) when
// a config swap evicts this hook. No-op for the RE2 matcher.
func (cs *ContentSafety) Close() error {
	_ = cs.closePrescan()
	if c, ok := cs.matcher.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
