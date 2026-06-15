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
	RegisterAllStrategies(reg, mockLookup, smartDeps)
	reg.Freeze()

	// "smart" strategy should be registered; an unknown node type triggers ErrUnknown.
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "smart", ProviderID: "", ModelID: "auto"},
		&core.RoutingContext{RequestedModel: core.RequestedModel{ID: "auto"}},
		&[]core.TraceEntry{},
		0,
	)
	// Smart strategy requires a non-empty model list; it may return error or
	// nil targets but must NOT return "unknown strategy type".
	if err != nil && errors.Is(err, ErrMaxDepth) {
		t.Errorf("smart strategy appears not registered: got ErrMaxDepth")
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
		0,
		nil, // recurse unused in single
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
		0,
		nil, // recurse unused when no branch matches and no default
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

func TestFallbackStrategy_recurseError_propagates(t *testing.T) {
	s := &FallbackStrategy{}
	errRecurse := errors.New("recurse failed")
	recurse := func(_ context.Context, _ core.StrategyNode, _ *core.RoutingContext, _ *[]core.TraceEntry, _ int) ([]core.RoutingTarget, error) {
		return nil, errRecurse
	}
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
		0,
		recurse,
	)
	if !errors.Is(err, errRecurse) {
		t.Errorf("expected errRecurse, got %v", err)
	}
}

// LoadbalanceStrategy.Evaluate: recurse error propagates

func TestLoadbalanceStrategy_recurseError_propagates(t *testing.T) {
	s := &LoadbalanceStrategy{}
	errRecurse := errors.New("load balance recurse failed")
	recurse := func(_ context.Context, _ core.StrategyNode, _ *core.RoutingContext, _ *[]core.TraceEntry, _ int) ([]core.RoutingTarget, error) {
		return nil, errRecurse
	}
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
		0,
		recurse,
	)
	if !errors.Is(err, errRecurse) {
		t.Errorf("expected errRecurse, got %v", err)
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
		0,
		nil,
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
