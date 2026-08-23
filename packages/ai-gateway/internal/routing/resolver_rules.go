package routing

import (
	"context"
	"fmt"

	"github.com/goccy/go-json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// selectRules picks the rule that serves the request and collects everything
// the walk may fall back to.
//
// It is separate from Resolve because it is a separate question. Resolve
// assembles a request's facts and hands back a plan; which rule answers, and
// what stands behind it, is decided here and nowhere else.
func (r *Resolver) selectRules(
	ctx context.Context,
	matched []*store.RoutingRule,
	rctx *core.RoutingContext,
	plan *core.RoutingPlan,
) {
	// The highest-priority match that actually RESOLVES SOMETHING becomes the
	// primary rule, not simply the highest-priority match.
	//
	// Claiming the slot and producing nothing is a way to disable every rule
	// beneath you, and it happens without an error to notice: a strategy that
	// yields no targets, a `single` whose model was disabled, a config the
	// registry cannot evaluate. The operator sees routing quietly stop and has
	// nothing to read. A rule with nothing to offer has nothing to defend
	// either, so the next match is tried instead — which is also the shape
	// rule advancement takes once a plan can be exhausted at dispatch.
	var primaryRule *store.RoutingRule
	var primaryTargets []core.RoutingTarget
	var lowerRules []core.RoutingTarget
	for _, rule := range matched {
		var node core.StrategyNode
		if err := json.Unmarshal(rule.Config, &node); err != nil {
			r.yieldSlot(plan, rule, "its strategy configuration does not parse: "+err.Error())
			continue
		}
		// Everything this rule's evaluation says about itself is stamped with
		// the rule that said it. Without that, a rule that resolves a target
		// and then yields leaves "resolved <target>" in a trace shared with the
		// winner, and the operator reads a target that was never routed to as
		// though it were the decision.
		before := len(plan.Trace)
		targets, err := r.registry.Evaluate(ctx, node, rctx, &plan.Trace)
		attributeTrace(plan.Trace[before:], rule)
		if err != nil {
			r.yieldSlot(plan, rule, "its strategy could not be evaluated: "+err.Error())
			continue
		}
		// The virtual key's allowlist is applied before the rule is judged to
		// have produced something: a rule whose every target this key cannot
		// reach has resolved nothing FOR THIS REQUEST, and holding the slot on
		// the strength of targets that will be filtered away would disable the
		// rules below it for exactly the keys that needed them.
		filtered := r.vkAccess.Keep(targets, rctx)
		if len(filtered) == 0 {
			if len(targets) == 0 {
				r.yieldSlot(plan, rule, "it resolved no target")
			} else {
				r.yieldSlot(plan, rule, fmt.Sprintf(
					"all %d of its targets are outside this key's allowed models", len(targets)))
			}
			continue
		}
		if primaryRule == nil {
			primaryRule, primaryTargets = rule, filtered
			for i := range primaryTargets {
				primaryTargets[i].RuleID = rule.ID
			}
			continue
		}
		// Every matching rule below the primary is a backup, in priority order.
		//
		// There is no separate species for this: a rule an admin wrote at lower
		// priority IS the alternative for when the rule above it cannot serve
		// the request. Collecting them here is what gives the walk somewhere to
		// advance TO — and the walk may only advance when every target of the
		// rule above has been eliminated, so an ordinary transient failure
		// never leaks traffic across a rule boundary.
		for i := range filtered {
			filtered[i].Source = "recovery"
			filtered[i].RuleID = rule.ID
		}
		// Held back, not appended here. The primary rule's OWN chain is
		// assembled after this loop, and a rule's own backups come before a
		// different rule's answer — appending in loop order put a lower rule's
		// target ahead of the chain the primary rule carries.
		lowerRules = append(lowerRules, filtered...)
	}

	if primaryRule != nil {
		plan.RuleID = primaryRule.ID
		plan.RuleName = primaryRule.Name
		// Carry the primary rule's per-rule RetryPolicy JSON forward.
		// The handler field-merges it on top of the YAML default before
		// invoking the executor. Fallback rules' retry policies are
		// intentionally ignored — only the primary rule's policy
		// determines L2/L3 behavior for the routed targets.
		plan.RuleRetryPolicyJSON = primaryRule.RetryPolicy

		for i := range primaryTargets {
			primaryTargets[i].Source = "primary"
		}
		plan.Targets = primaryTargets

		// Recovery targets from inline fallback chain.
		if len(primaryRule.FallbackChain) > 0 {
			var chain []core.FallbackChainEntry
			// best-effort: a malformed fallback chain just yields no recovery
			// targets; the primary plan still routes normally.
			_ = json.Unmarshal(primaryRule.FallbackChain, &chain)
			var fbTargets []core.RoutingTarget
			for _, entry := range chain {
				target, err := r.lookupTarget(ctx, entry.ProviderID, entry.ModelID)
				if err == nil {
					fbTargets = append(fbTargets, *target)
				}
			}
			// Inline fallback-chain recovery targets MUST satisfy the
			// VK's allowedModels allowlist, exactly like the primary (above) and
			// lower-priority-rule paths. Without this Filter a FallbackChain
			// entry pointing at a provider/model outside the VK's allowlist would
			// be dispatched — and its upstream credential consumed — on primary
			// failure, silently escaping the per-VK model-scope boundary.
			filtered := r.vkAccess.Keep(fbTargets, rctx)
			for i := range filtered {
				filtered[i].Source = "fallback"
				// The chain belongs to the rule that carries it, so it is
				// dispatched as part of that rule rather than as the next one:
				// an admin writing a fallback chain is naming this rule's own
				// backups, not a different rule.
				filtered[i].RuleID = primaryRule.ID
			}
			plan.RecoveryTargets = append(plan.RecoveryTargets, filtered...)
		}
	}

	// The rules below the primary, after the primary's own chain.
	plan.RecoveryTargets = append(plan.RecoveryTargets, lowerRules...)

}

// yieldSlot records a rule stepping aside, in the one place an operator can
// read it.
//
// A rule can match the request and still produce nothing to route to: its
// configuration will not parse, its strategy cannot be evaluated, it resolves
// no target, or every target it resolved is outside the calling key's allowed
// models. In each case the rule below it is tried instead — which is right, and
// was invisible. The rule stays enabled and green in the admin UI, the plan
// names a different rule, and "why didn't my rule fire?" has no field to read.
//
// A WARN goes to the service log for the operator who has one; the pipeline
// trace goes to traffic_event.routing_trace, which is the channel the admin
// actually has. The last case logs at Debug because it is per-key and expected
// at volume, but it still records a trace entry: for the one key it happened
// to, it is the whole explanation.
func (r *Resolver) yieldSlot(plan *core.RoutingPlan, rule *store.RoutingRule, why string) {
	msg := fmt.Sprintf("rule %q (%s) yielded the primary slot: %s", rule.Name, rule.ID, why)
	if strings.HasPrefix(why, "all ") {
		r.logger.Debug("routing: "+msg, "ruleId", rule.ID)
	} else {
		r.logger.Warn("routing: "+msg, "ruleId", rule.ID)
	}
	plan.PipelineTrace = append(plan.PipelineTrace, core.PipelineTraceEntry{
		Stage:    1,
		Decision: msg,
	})
}

// attributeTrace stamps the rule that produced each entry.
//
// Strategies do not know which rule they are evaluating for, so they leave
// these fields empty and every entry in the trace looks like it belongs to the
// plan. Once more than one rule is evaluated per request — which is what trying
// the next match means — an unattributed trace is worse than a short one: the
// losing rule's reasoning reads as the winner's.
func attributeTrace(entries []core.TraceEntry, rule *store.RoutingRule) {
	for i := range entries {
		entries[i].RuleID = rule.ID
		entries[i].RuleName = rule.Name
	}
}

// namedModelIsAllowed reports whether the caller's named model is on the key's
// allow list.
//
// It resolves the model row because the allow list is matched on three
// identities — the catalog UUID, the provider's own model id, and the provider
// — and a caller may have named the model by code or alias. Matching the raw
// request string against the list instead would refuse a key that IS allowed
// the model whenever the caller spelled it a way the list does not.
//
// A lookup failure keeps the request: the catalog is momentarily unreadable,
// which is not evidence the key lacks permission, and the target-side check
// still stands between the request and any model it may not use.
func (r *Resolver) namedModelIsAllowed(ctx context.Context, modelID string, allowed []store.AllowedModelRef) bool {
	m, err := r.db.GetModel(ctx, modelID)
	if err != nil || m == nil {
		return true
	}
	return core.ModelMatchesAllowedRefs(m.ID, m.ProviderModelID, m.ProviderID, allowed)
}
