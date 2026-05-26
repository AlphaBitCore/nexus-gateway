package rulepack_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// fakeLister is a drop-in InstallLister implementation for enricher tests.
// It returns the configured EffectiveRuleSets keyed by hook ID; any hook ID
// not present yields an empty slice.
type fakeLister struct {
	sets map[string][]rulepack.EffectiveRuleSet
	err  error
}

func (f *fakeLister) LoadEffectiveSetsForHook(_ context.Context, hookID string) ([]rulepack.EffectiveRuleSet, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sets[hookID], nil
}

func TestEnrich_InjectsInstallsForRulePackConsumers(t *testing.T) {
	store := &fakeLister{sets: map[string][]rulepack.EffectiveRuleSet{
		"cs-hook": {{
			Install: rulepack.Install{ID: "inst-1", PackName: "safety", Enabled: true},
			Pack: rulepack.Pack{
				Name:    "safety",
				Version: "1.0.0",
				Rules: []rulepack.Rule{{
					RuleID: "r-1", Category: "safety", Severity: "hard", Pattern: `\bpayload\b`,
				}},
			},
		}},
	}}

	cfgs := []core.HookConfig{
		{ID: "cs-hook", ImplementationID: "content-safety", Config: map[string]any{}},
		{ID: "ip-hook", ImplementationID: "ip-access-filter", Config: map[string]any{}},
	}

	out, err := rulepack.Enrich(context.Background(), store, cfgs)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	got, ok := out[0].Config["_rulePackInstalls"]
	if !ok {
		t.Fatal("content-safety HookConfig missing _rulePackInstalls after enrich")
	}
	installs, ok := got.([]any)
	if !ok || len(installs) != 1 {
		t.Fatalf("_rulePackInstalls = %T %v, want []any len 1", got, got)
	}

	if _, ok := out[1].Config["_rulePackInstalls"]; ok {
		t.Error("non-rulepack-consumer hook should not be enriched")
	}
}

func TestEnrich_SkipsWhenNoInstallsBound(t *testing.T) {
	store := &fakeLister{sets: map[string][]rulepack.EffectiveRuleSet{}}
	cfgs := []core.HookConfig{
		{ID: "cs-hook", ImplementationID: "content-safety", Config: map[string]any{"categories": map[string]any{"violence": true}}},
	}
	out, err := rulepack.Enrich(context.Background(), store, cfgs)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if _, present := out[0].Config["_rulePackInstalls"]; present {
		t.Error("hook with no installs should not have _rulePackInstalls injected")
	}
	// Legacy field must survive unchanged.
	if _, ok := out[0].Config["categories"]; !ok {
		t.Error("legacy config field lost during Enrich")
	}
}

func TestEnrich_PropagatesStoreError(t *testing.T) {
	store := &fakeLister{err: errors.New("db down")}
	_, err := rulepack.Enrich(context.Background(), store, []core.HookConfig{
		{ID: "h", ImplementationID: "content-safety", Config: map[string]any{}},
	})
	if err == nil {
		t.Fatal("expected error from store, got nil")
	}
}
