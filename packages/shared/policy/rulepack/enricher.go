// Package rulepack: enricher.go — wires rule-pack installs into the
// shared hook-config loading path. The data-plane services (ai-gateway,
// compliance-proxy, agent) all load HookConfig rows from Postgres and
// build pipelines from them. Hooks powered by rule packs (the unified
// rulepack-engine, as well as legacy content-safety / keyword-filter /
// pii-detector that are migrating onto the same engine) need their
// effective rule set embedded into HookConfig.Config at load time —
// pipeline build time is on the request hot path and cannot query the
// DB per request.
//
// Enricher bridges shared/store (HookConfig loader) and shared/rulepack
// (Store) without forcing either to depend on the other. Consumers
// construct it with both, then call Enrich right after LoadHookConfigs.
package rulepack

import (
	"context"
	"fmt"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// RulePackConsumer lists the implementation IDs that should have
// effective rule sets injected into their HookConfig. New implementations
// that consume rule packs should be added here so the enricher knows to
// look them up.
//
// Declared as a map of sentinel structs (rather than a string slice) for
// O(1) membership tests in Enrich's inner loop without runtime reflection.
var RulePackConsumer = map[string]struct{}{
	"rulepack-engine": {},
	// Legacy hooks migrating to rule-pack-backed evaluation. They can
	// still run on their own config (backward compatible), but whenever
	// an install is bound to their hook ID the engine path takes over.
	"content-safety": {},
	"keyword-filter": {},
	"pii-detector":   {},
}

// InstallLister is the minimal Store interface Enrich needs. Declared
// here (not in store.go) so tests can inject a fake without spinning up
// Postgres.
type InstallLister interface {
	LoadEffectiveSetsForHook(ctx context.Context, hookID string) ([]EffectiveRuleSet, error)
}

// Enrich annotates each HookConfig whose ImplementationID is a
// RulePackConsumer with the slice of EffectiveRuleSets bound to its
// hook ID. The slice is written into `cfg.Config["_rulePackInstalls"]`
// in the shape the rulepack-engine factory understands (see
// rulepack_engine.go). HookConfigs with no installs bound are left
// untouched so their legacy behavior is preserved.
//
// Enrich mutates the input slice in place AND returns it, so callers
// can use either the returned value or the original variable.
//
// Enrich is best-effort. A single hook whose installs fail to load is
// skipped (the hook keeps its legacy bespoke config) and the failure is
// logged + counted via the package-level
// `nexus_rulepack_enrich_failure_total{hook,reason}` metric (registered
// by callers — see compliance-proxy main). The reload as a whole still
// succeeds so a single corrupted install does not take out every hook
// for an entire data plane. Bulk DB outages still error out, since a
// per-call DB failure manifests on every call and we want to surface
// that loudly rather than silently degrading every hook.
func Enrich(ctx context.Context, store InstallLister, cfgs []core.HookConfig) ([]core.HookConfig, error) {
	failures := 0
	consumerCalls := 0
	for i := range cfgs {
		cfg := &cfgs[i]
		if _, ok := RulePackConsumer[cfg.ImplementationID]; !ok {
			continue
		}
		consumerCalls++
		sets, err := store.LoadEffectiveSetsForHook(ctx, cfg.ID)
		if err != nil {
			failures++
			if EnrichFailures != nil {
				EnrichFailures(cfg.ID, cfg.ImplementationID, err)
			}
			// Skip this hook; leave Config untouched so the legacy
			// path keeps the hook usable.
			continue
		}
		if len(sets) == 0 {
			continue
		}
		if cfg.Config == nil {
			cfg.Config = map[string]any{}
		}
		cfg.Config["_rulePackInstalls"] = toRulePackInstallPayload(sets)
	}
	// If every consumer hook failed we still want to know — return an
	// aggregate error in that pathological case so callers can surface
	// the full DB outage to admins. Mixed success/failure is silent
	// per-hook (logged via the metric callback).
	if consumerCalls > 0 && failures == consumerCalls {
		return cfgs, fmt.Errorf("rulepack.Enrich: all %d consumer hook(s) failed to load installs", consumerCalls)
	}
	return cfgs, nil
}

// EnrichFailures is an optional observability callback. When set by the
// host service (compliance-proxy / ai-gateway / agent), Enrich invokes
// it once per skipped hook with the hook id, implementation id, and
// underlying error. Hosts wire it to a Prometheus counter labelled
// `{hook, reason}` plus a structured error log.
var EnrichFailures func(hookID, implID string, err error)

// toRulePackInstallPayload converts EffectiveRuleSets into the shape
// the core.rulePackInstall JSON consumer expects. We render as []any
// so the value round-trips through JSON cleanly (the loader path may
// serialize to thingclient shadow for push delivery).
func toRulePackInstallPayload(sets []EffectiveRuleSet) []any {
	out := make([]any, 0, len(sets))
	for _, s := range sets {
		rules := make([]any, 0, len(s.Pack.Rules))
		for _, r := range s.Pack.Rules {
			rules = append(rules, map[string]any{
				"ruleId":   r.RuleID,
				"category": r.Category,
				"severity": r.Severity,
				"pattern":  r.Pattern,
				"flags":    r.Flags,
				"labels":   r.Labels,
			})
		}
		out = append(out, map[string]any{
			"installId":   s.Install.ID,
			"packName":    s.Pack.Name,
			"packVersion": s.Pack.Version,
			"enabled":     s.Install.Enabled,
			"rules":       rules,
		})
	}
	return out
}
