package strategies

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// testStrategy is a simple Strategy implementation for testing.
type testStrategy struct {
	name string
	fn   func(context.Context, core.StrategyNode, *core.RoutingContext, *[]core.TraceEntry, int, RecurseFunc) ([]core.RoutingTarget, error)
}

func (s *testStrategy) Type() string { return s.name }
func (s *testStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, depth int, recurse RecurseFunc) ([]core.RoutingTarget, error) {
	return s.fn(ctx, node, rctx, trace, depth, recurse)
}

func TestRegistry_RegisterAndEvaluate(t *testing.T) {
	reg := NewStrategyRegistry()
	reg.Register(&testStrategy{
		name: "test",
		fn: func(_ context.Context, node core.StrategyNode, _ *core.RoutingContext, _ *[]core.TraceEntry, _ int, _ RecurseFunc) ([]core.RoutingTarget, error) {
			return []core.RoutingTarget{{ProviderID: node.ProviderID, ModelID: node.ModelID}}, nil
		},
	})

	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "test", ProviderID: "openai", ModelID: "gpt-4"},
		&core.RoutingContext{},
		&[]core.TraceEntry{},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ProviderID != "openai" {
		t.Errorf("unexpected targets: %v", targets)
	}
}

func TestRegistry_UnknownStrategy(t *testing.T) {
	reg := NewStrategyRegistry()
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "nonexistent"},
		&core.RoutingContext{},
		&[]core.TraceEntry{},
		0,
	)
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestRegistry_MaxDepth(t *testing.T) {
	reg := NewStrategyRegistry()
	reg.Register(&testStrategy{
		name: "infinite",
		fn: func(ctx context.Context, _ core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, depth int, recurse RecurseFunc) ([]core.RoutingTarget, error) {
			return recurse(ctx, core.StrategyNode{Type: "infinite"}, rctx, trace, depth)
		},
	})

	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "infinite"},
		&core.RoutingContext{},
		&[]core.TraceEntry{},
		0,
	)
	if !errors.Is(err, ErrMaxDepth) {
		t.Errorf("expected ErrMaxDepth, got %v", err)
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	reg := NewStrategyRegistry()
	noop := &testStrategy{
		name: "dup",
		fn: func(context.Context, core.StrategyNode, *core.RoutingContext, *[]core.TraceEntry, int, RecurseFunc) ([]core.RoutingTarget, error) {
			return nil, nil
		},
	}
	reg.Register(noop)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate")
		}
	}()
	reg.Register(noop)
}

func TestRegistry_FreezeBlocksRegister(t *testing.T) {
	reg := NewStrategyRegistry()
	reg.Freeze()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on register after freeze")
		}
	}()
	reg.Register(&testStrategy{
		name: "late",
		fn: func(context.Context, core.StrategyNode, *core.RoutingContext, *[]core.TraceEntry, int, RecurseFunc) ([]core.RoutingTarget, error) {
			return nil, nil
		},
	})
}
