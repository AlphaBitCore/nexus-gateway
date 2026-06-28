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
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
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
	RuleID   string `json:"ruleId"`
	Category string `json:"category"`
	// Severity is the rule-pack authoring severity. The canonical authoring
	// enum validated by rulepack.ValidatePack (yaml.go) is "hard" | "soft" |
	// "warn". At runtime severity gates ENFORCEMENT only (severityEnforces):
	// "hard" and "soft" enforce — the bound hook's onMatch.Action is applied;
	// "warn", "info", "" and any unknown value are observe-only (tags, never a
	// decision change). The action (redact vs block) comes from the hook's
	// onMatch.Action, not the severity tier. A typo therefore degrades to
	// observe-only, surfaced via Tags rather than silently enforcing.
	Severity string   `json:"severity"`
	Pattern  string   `json:"pattern"`
	Flags    string   `json:"flags,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

// compiledRule carries a source rule plus its owning install so BlockingRule
// attribution is zero-allocation on the hot path. The pattern itself is
// compiled and owned by the engine's Matcher (matching is one scan over all
// rules, not a per-rule regex call); this struct holds only the metadata the
// decision layer needs after a match is reported.
type compiledRule struct {
	installID   string
	packName    string
	packVersion string
	rule        rulePackRule
}

// RulePackEngine is the hook implementation backed by rule-pack installs.
// It evaluates each compiled rule against every text segment in order.
//
// Decision resolution: the rule's severity is a hint; the operator's
// onMatch.action is the ceiling. The effective decision is the
// strictest of the two — info-severity rules emit tags only and are never
// blocked regardless of onMatch; hard-severity always blocks; soft-severity
// respects the onMatch override.
//
// Applies to all text-carrying endpoints, text modality only, via the
// embedded TextOnlyContentScanning helper.
type RulePackEngine struct {
	core.TextOnlyContentScanning
	contentPrescan // raw-body prefilter (core.RawContentPrescanner)
	cfg            *core.HookConfig
	rules          []compiledRule
	matcher        matcher.Matcher
	onMatch        core.OnMatchConfig
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
// Installs with enabled=false are dropped at construction time.
//
// Fail-posture (availability-first): a single rule whose pattern fails to
// compile is SKIPPED+LOGGED, not fatal — one bad rule degrades to "that rule
// off", never "the whole pack (or the whole pipeline) off". This mirrors the
// per-hook graceful degradation in pipeline.resolve() and the fail-open
// default in pipeline.executeOneHook. Authoring-time validation
// (rulepack.ValidatePack) is the canonical gate that rejects a bad pattern
// with a 400 before it ever reaches the runtime; this skip is the
// defence-in-depth backstop for a rule that slipped through (e.g. a pattern
// that compiles under one regexp build but not another).
func NewRulePackEngine(cfg *core.HookConfig) (core.Hook, error) {
	return newRulePackEngineWith(cfg, defaultCompiler())
}

// matcherCompiler builds a Matcher from a pattern set. It is the injection
// point that lets the same rule set run through either engine: the production
// factory uses the RE2 compiler today (the Vectorscan build-tag variant swaps
// it in), and the differential test runs the Vectorscan-built engine against
// the RE2 oracle in one process.
type matcherCompiler func([]matcher.Pattern) (matcher.Matcher, []matcher.BadPattern)

func newRulePackEngineWith(cfg *core.HookConfig, compile matcherCompiler) (core.Hook, error) {
	installs, err := parseRulePackInstalls(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("rulepack-engine: %w", err)
	}
	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("rulepack-engine: %w", err)
	}
	// Absent onMatch keeps ParseOnMatch's block default: an enforcing (hard/soft)
	// rule bound to a hook with no explicit action blocks by default — the
	// security-safe posture. Operators opt into redact or pure-observe by setting
	// onMatch.action explicitly; the action is always the hook's policy, never
	// derived from the rule severity tier (severity only gates enforce-vs-observe).

	// Flatten installs into rule metadata (install-order then rule-order) and a
	// parallel pattern set keyed by the rule's index. The Matcher owns
	// compilation; a pattern that fails to compile is reported in `bad` and
	// simply never produces a hit — the rule is inert (today's skip-and-log
	// fail-posture) without a separate per-rule compile here.
	compiled := make([]compiledRule, 0, len(installs)*8)
	pats := make([]matcher.Pattern, 0, len(installs)*8)
	for _, inst := range installs {
		if !inst.Enabled {
			continue
		}
		for _, r := range inst.Rules {
			pats = append(pats, matcher.Pattern{ID: len(compiled), Expr: r.Pattern, Flags: r.Flags})
			compiled = append(compiled, compiledRule{
				installID:   inst.InstallID,
				packName:    inst.PackName,
				packVersion: inst.PackVersion,
				rule:        r,
			})
		}
	}

	m, bad := compile(pats)
	for _, b := range bad {
		cr := compiled[b.ID]
		slog.Warn("rulepack-engine: skipping rule with uncompilable pattern (degrading to this rule off)",
			"installId", cr.installID,
			"packName", cr.packName,
			"ruleId", cr.rule.RuleID,
			"error", b.Err,
		)
	}

	return &RulePackEngine{
		contentPrescan: newContentPrescan(pats),
		cfg:            cfg,
		rules:          compiled,
		matcher:        m,
		onMatch:        onMatch,
	}, nil
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

	segments := input.TextSegmentsWith(e.cfg.ProjectionOptions())

	// One scan over all rules, both directions handled upstream. firstOnly:
	// a detect/block decision only needs to know whether a rule fired in a
	// segment, not how many times. The decision resolution below is engine-
	// agnostic and byte-identical to the per-rule MatchString loop it replaces.
	// complete is false only when the cgo matcher truncated mid-scan (alloc
	// failure) — a partial hit set the redaction path must treat as fail-unsafe.
	var hits []matcher.Hit
	complete := true
	if cs, ok := e.matcher.(matcher.CompleteScanner); ok {
		hits, complete = cs.ScanComplete(segments, true)
	} else {
		hits = e.matcher.Scan(segments, true)
	}
	matched := make(map[[2]int]struct{})
	for _, h := range hits {
		matched[[2]int{h.ID, h.Seg}] = struct{}{}
	}
	core.ObserveContentScan(e.cfg.ImplementationID, len(matched))

	// A redact hook turns matches into masking spans instead of a block
	// decision. The fast cgo matcher above is compiled without start-of-match,
	// so it reports WHICH rules fired but no offsets — redaction re-localises
	// the matched rules with their cached RE2 pattern (or EVERY rule when the
	// scan was incomplete, so a dropped hit never leaves PII unmasked).
	if e.onMatch.Action == core.ActionRedact {
		return e.executeRedact(input, result, matched, complete, start)
	}

	for ri := range e.rules {
		cr := e.rules[ri]
		for si := range segments {
			if _, ok := matched[[2]int{ri, si}]; !ok {
				continue
			}
			// Severity gates ENFORCEMENT; the bound hook's onMatch.Action decides
			// the ACTION. hard/soft enforce (apply the hook's action); warn/info/
			// unknown observe (tags only, keep scanning). A match on an
			// approve-policy hook is observe-only too — so severity never escalates
			// past the operator's chosen action (a hard rule on a redact hook
			// redacts, it does not block).
			if !severityEnforces(cr.rule.Severity) || e.onMatch.Action == core.ActionApprove {
				result.Tags = core.AppendTag(result.Tags, "rulepack:"+cr.packName)
				result.Tags = core.AppendTag(result.Tags, "rule:"+cr.rule.RuleID)
				continue
			}
			result.Decision = core.DecisionForAction(e.onMatch.Action)
			result.Action = e.onMatch.Action
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

// Close releases resources held by the engine's matcher — for the Vectorscan
// matcher, the compiled database and scratch pool. It is invoked when a config
// swap evicts this engine instance (the RE2 matcher holds no native resources,
// so this is a no-op there). Idempotent and safe to call while requests are
// still resolving the engine: the matcher drains in-flight scans before freeing.
func (e *RulePackEngine) Close() error {
	_ = e.closePrescan()
	if c, ok := e.matcher.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
