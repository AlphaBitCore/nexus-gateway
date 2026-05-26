// Package validators: rulepack_engine.go — runtime evaluator that unifies
// the content-safety / keyword-filter / pii-detector execution paths behind
// the shared rule-pack data model.
//
// The factory expects the loader to have pre-resolved every active rule-pack
// install bound to this hook config and embedded the effective rule set into
// cfg.Config["_rulePackInstalls"]. This keeps Execute pure (no DB handle)
// while letting all data-plane services share one cache-invalidation path.
package validators

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// rulePackInstall is the runtime projection of a single rule-pack install
// bound to this hook. Consumed by the loader-provided config payload —
// see compliance-proxy / ai-gateway loaders that inject this.
type rulePackInstall struct {
	InstallID   string         `json:"installId"`
	PackName    string         `json:"packName"`
	PackVersion string         `json:"packVersion"`
	Enabled     bool           `json:"enabled"`
	Rules       []rulePackRule `json:"rules"`
}

// rulePackRule mirrors rulepack.Rule but is local to avoid an import cycle
// (rulepack depends on hooks for ContentBlock).
type rulePackRule struct {
	RuleID   string   `json:"ruleId"`
	Category string   `json:"category"`
	Severity string   `json:"severity"` // "hard" | "soft" | "info"
	Pattern  string   `json:"pattern"`
	Flags    string   `json:"flags,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

// compiledRule pairs a source rule with its compiled regex and owning
// install so BlockingRule attribution is zero-allocation on the hot path.
type compiledRule struct {
	installID   string
	packName    string
	packVersion string
	rule        rulePackRule
	re          *regexp.Regexp
}

// RulePackEngine is the hook implementation backed by rule-pack installs.
// It evaluates each compiled rule against every text segment in order.
//
// Decision resolution: the rule's severity is a hint; the operator's
// onMatch.inflightAction is the ceiling. The effective decision is the
// strictest of the two — info-severity rules emit tags only and are never
// blocked regardless of onMatch; hard-severity always blocks; soft-severity
// respects the onMatch override.
//
// Applies to all text-carrying endpoints, text modality only, via the
// embedded TextOnlyContentScanning helper.
type RulePackEngine struct {
	core.TextOnlyContentScanning
	cfg     *core.HookConfig
	rules   []compiledRule
	onMatch core.OnMatchConfig
}

// NewRulePackEngine is the factory registered under "rulepack-engine".
//
// Expected config shape (produced by the hook config loader, not authored
// by operators directly):
//
//	{
//	  "_rulePackInstalls": [
//	    {
//	      "installId": "…",
//	      "packName":  "safety-default",
//	      "packVersion": "1.0.0",
//	      "enabled":  true,
//	      "rules": [
//	        {"ruleId":"…","category":"safety","severity":"hard","pattern":"…","flags":"i","labels":["…"]}
//	      ]
//	    }
//	  ]
//	}
//
// Installs with enabled=false are dropped at construction time. A single
// invalid regex makes the whole pack fail — a hook silently skipping rules
// is harder to diagnose than a construction-time error.
func NewRulePackEngine(cfg *core.HookConfig) (core.Hook, error) {
	installs, err := parseRulePackInstalls(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("rulepack-engine: %w", err)
	}
	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("rulepack-engine: %w", err)
	}
	// Absent onMatch means rule severity drives the decision (no operator
	// ceiling). Other content hooks default to block-hard because they have
	// no per-rule severity signal.
	if _, explicit := cfg.Config["onMatch"]; !explicit {
		onMatch.InflightAction = core.InflightApprove
	}

	compiled := make([]compiledRule, 0, len(installs)*8)
	for _, inst := range installs {
		if !inst.Enabled {
			continue
		}
		for _, r := range inst.Rules {
			re, err := core.CompilePattern(r.Pattern, r.Flags)
			if err != nil {
				return nil, fmt.Errorf("rulepack-engine: install %s rule %s: %w",
					inst.InstallID, r.RuleID, err)
			}
			compiled = append(compiled, compiledRule{
				installID:   inst.InstallID,
				packName:    inst.PackName,
				packVersion: inst.PackVersion,
				rule:        r,
				re:          re,
			})
		}
	}

	return &RulePackEngine{cfg: cfg, rules: compiled, onMatch: onMatch}, nil
}

// parseRulePackInstalls reads the loader-injected `_rulePackInstalls` slot
// from the HookConfig.Config map. Accepts either the already-typed
// `[]rulePackInstall` (unit tests) or the generic `[]any` shape that comes
// back from JSON unmarshal into `map[string]any`.
func parseRulePackInstalls(cfg map[string]any) ([]rulePackInstall, error) {
	raw, ok := cfg["_rulePackInstalls"]
	if !ok || raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case []rulePackInstall:
		return v, nil
	case []any:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal _rulePackInstalls: %w", err)
		}
		var out []rulePackInstall
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("unmarshal _rulePackInstalls: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("_rulePackInstalls: unsupported type %T", raw)
	}
}

// Execute iterates each compiled rule against each content block. The
// first rule that produces a match wins. Match precedence is by rule
// order within the install (and install order across the bound set);
// severity does NOT re-order the scan because operators rely on rule
// order to express "check this pack first" semantics.
func (e *RulePackEngine) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()
	result := &core.HookResult{
		HookID:           e.cfg.ID,
		ImplementationID: e.cfg.ImplementationID,
		HookName:         e.cfg.Name,
		Decision:         core.Approve,
	}

	opts := e.cfg.ProjectionOptions()
	for _, cr := range e.rules {
		for _, text := range input.TextSegmentsWith(opts) {
			if !cr.re.MatchString(text) {
				continue
			}
			severity := severityToDecision(cr.rule.Severity)
			if severity == core.Approve {
				// info severity — stamp a tag, keep scanning. onMatch.inflightAction
				// does NOT promote info matches to blocks; that would surprise
				// operators who deliberately mark rules informational.
				result.Tags = core.AppendTag(result.Tags, "rulepack:"+cr.packName)
				result.Tags = core.AppendTag(result.Tags, "rule:"+cr.rule.RuleID)
				continue
			}
			// Strictest-wins between rule severity and operator onMatch ceiling.
			decision := strictestDecision(severity, core.DecisionForInflight(e.onMatch.InflightAction))
			result.Decision = decision
			result.Reason = fmt.Sprintf("rule-pack match: %s/%s (%s)",
				cr.packName, cr.rule.RuleID, cr.rule.Category)
			result.ReasonCode = "RULEPACK_MATCH"
			result.BlockingRule = &core.BlockingRule{
				Pack:        cr.packName,
				PackVersion: cr.packVersion,
				RuleID:      cr.rule.RuleID,
				Category:    cr.rule.Category,
				Severity:    cr.rule.Severity,
				Labels:      append([]string(nil), cr.rule.Labels...),
			}
			result.Tags = core.AppendTag(result.Tags, "rulepack:"+cr.packName)
			result.Tags = core.AppendTag(result.Tags, "rule:"+cr.rule.RuleID)
			if cr.rule.Category != "" {
				result.Tags = core.AppendTag(result.Tags, "category:"+cr.rule.Category)
			}
			for _, label := range cr.rule.Labels {
				result.Tags = core.AppendTag(result.Tags, label)
			}
			result.LatencyMs = int(time.Since(start).Milliseconds())
			return result, nil
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// strictestDecision picks the more-blocking decision between two Decisions.
// Ordering (strictest → most permissive): RejectHard > BlockSoft > Modify > Approve.
// Used by rulepack-engine to reconcile rule-severity vs onMatch ceiling.
func strictestDecision(a, b core.Decision) core.Decision {
	rank := func(d core.Decision) int {
		switch d {
		case core.RejectHard:
			return 4
		case core.BlockSoft:
			return 3
		case core.Modify:
			return 2
		case core.Approve:
			return 1
		}
		return 0
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// severityToDecision maps the rule-pack severity string onto the hook
// Decision enum. Unknown severities default to Approve so a typo in a
// rule cannot accidentally reject every request — the operator sees it
// via Tags (`rule:…`) instead.
func severityToDecision(s string) core.Decision {
	switch s {
	case "hard":
		return core.RejectHard
	case "soft":
		return core.BlockSoft
	case "info", "":
		return core.Approve
	default:
		return core.Approve
	}
}
