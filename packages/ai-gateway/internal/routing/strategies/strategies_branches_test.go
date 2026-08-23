// Package strategies — strategies_gap_test.go covers branches not reached by
// the existing test files.
//
// Named failure modes:
//   - RegisterAllStrategies with non-nil smartDeps → SmartStrategy registered
//   - SingleStrategy.Evaluate: lookup error → soft nil return
//   - ConditionalStrategy.Evaluate: no branch matched + no default → nil return
//   - FallbackStrategy.Evaluate: recurse error propagates
//   - LoadbalanceStrategy.Evaluate: recurse error propagates
package strategies

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// RegisterAllStrategies: non-nil smartDeps registers SmartStrategy

func TestRegisterAllStrategies_withSmartDeps_registersSmartStrategy(t *testing.T) {
	reg := NewStrategyRegistry()
	smartDeps := &SmartDeps{
		Store:     &fakeSmartStore{},
		Lookup:    mockLookup,
		RouterLLM: &fakeDecider{},
		Logger:    nil,
	}
	RegisterAllStrategies(reg, mockLookup, nil, smartDeps)
	reg.Freeze()

	// "smart" strategy should be registered; an unknown node type triggers ErrUnknown.
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "smart", ProviderID: "", ModelID: "auto"},
		&core.RoutingContext{RequestedModel: core.RequestedModel{ID: "auto"}},
		&[]core.TraceEntry{},
	)
	// Smart requires a non-empty model list, so it may return an error or no
	// targets — but never "unknown strategy type", which would mean it was not
	// registered at all.
	//
	// The check used to look for ErrMaxDepth, which an unregistered type never
	// produced; it asserted the wrong thing and passed for the wrong reason.
	// The comment beside it always said what was meant.
	if err != nil && strings.Contains(err.Error(), "unknown strategy type") {
		t.Errorf("smart strategy is not registered: %v", err)
	}
}

// SingleStrategy.Evaluate: lookup error → soft nil return

func TestSingleStrategy_lookupError_returnsNilTargets(t *testing.T) {
	errLookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return nil, errors.New("provider offline")
	}
	s := &SingleStrategy{lookup: errLookup}
	var trace []core.TraceEntry
	targets, err := s.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"},
		&core.RoutingContext{},
		&trace,
	)
	if err != nil {
		t.Errorf("expected nil error (soft failure), got %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets on lookup error, got %d", len(targets))
	}
	if len(trace) != 1 || !contains(trace[0].Decision, "lookup failed") {
		t.Errorf("expected lookup-failed trace, got %+v", trace)
	}
}

// ConditionalStrategy.Evaluate: no branch matched, no default

func TestConditionalStrategy_noBranchNoDefault_returnsNilTargets(t *testing.T) {
	s := &ConditionalStrategy{}
	var trace []core.TraceEntry
	targets, err := s.Evaluate(
		context.Background(),
		// Conditions list is empty, Default is nil → falls through to "no branch matched, no default"
		core.StrategyNode{Type: "conditional", Conditions: nil, Default: nil},
		&core.RoutingContext{RequestedModel: core.RequestedModel{Type: "chat"}},
		&trace,
	)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets, got %d", len(targets))
	}
	if len(trace) != 1 || !contains(trace[0].Decision, "no branch matched, no default") {
		t.Errorf("expected no-branch-no-default trace, got %+v", trace)
	}
}

// FallbackStrategy.Evaluate: recurse error propagates

// TestFallbackStrategy_leafLookupError_propagates: an error resolving one of
// the chain's entries reaches the caller rather than being folded into a
// shorter chain.
//
// Same property the recursion-error test asserted, on the mechanism that
// replaced it. A chain entry used to point at a nested strategy and now names a
// leaf, but either way an unresolvable entry means the chain the admin wrote is
// not the chain being flown — and silently flying a shorter one is how a
// fallback stops being one without anybody noticing.
func TestFallbackStrategy_leafLookupError_propagates(t *testing.T) {
	errLookup := errors.New("catalog unavailable")
	s := &FallbackStrategy{lookup: func(_ context.Context, _, _ string) (*core.RoutingTarget, error) {
		return nil, errLookup
	}}
	var trace []core.TraceEntry
	_, err := s.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "fallback",
			Targets: []core.StrategyNode{
				{Type: "single", ProviderID: "p", ModelID: "m"},
			},
		},
		&core.RoutingContext{},
		&trace,
	)
	if !errors.Is(err, errLookup) {
		t.Errorf("expected the lookup error to surface, got %v", err)
	}
}

// LoadbalanceStrategy.Evaluate: recurse error propagates

// TestLoadbalanceStrategy_leafLookupError_propagates: when the chosen bucket's
// target cannot be resolved, the error reaches the caller rather than being
// swallowed into "no targets".
//
// The property is the same one the recursion-error test asserted; only the
// mechanism moved. A weighted entry used to point at a nested strategy the
// registry evaluated, and now it names a leaf this strategy resolves directly —
// but either way, an error there means the rule cannot be served, and reporting
// it as an empty result would make a broken lookup indistinguishable from a
// deliberately empty rule.
func TestLoadbalanceStrategy_leafLookupError_propagates(t *testing.T) {
	errLookup := errors.New("catalog unavailable")
	s := &LoadbalanceStrategy{lookup: func(_ context.Context, _, _ string) (*core.RoutingTarget, error) {
		return nil, errLookup
	}}
	var trace []core.TraceEntry
	_, err := s.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "loadbalance",
			Weighted: []core.WeightedTarget{
				{Weight: 100, Node: core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}},
			},
		},
		&core.RoutingContext{},
		&trace,
	)
	if !errors.Is(err, errLookup) {
		t.Errorf("expected the lookup error to surface, got %v", err)
	}
}

// ABSplitStrategy.Evaluate: zero weight in single-element list

func TestABSplitStrategy_zeroWeightSingleTarget_returnsNoTargets(t *testing.T) {
	s := &ABSplitStrategy{lookup: mockLookup}
	var trace []core.TraceEntry
	targets, err := s.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "ab_split",
			ABTargets: []core.ABTarget{
				{ProviderID: "openai", ModelID: "gpt-4", Weight: 0},
			},
		},
		&core.RoutingContext{},
		&trace,
	)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets for all-zero weights, got %d", len(targets))
	}
}

// Ensure llm package import is used.
var _ llm.Decider = &fakeDecider{}
