package strategies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// ErrMaxDepth indicates strategy evaluation exceeded the recursion limit.
var ErrMaxDepth = errors.New("router: max strategy depth exceeded")

// Strategy is the interface all routing strategies implement.
type Strategy interface {
	// Type returns the strategy type name (e.g. "single", "fallback").
	Type() string
	// Evaluate evaluates the strategy node and returns routing targets.
	Evaluate(
		ctx context.Context,
		node core.StrategyNode,
		rctx *core.RoutingContext,
		trace *[]core.TraceEntry,
		depth int,
		recurse RecurseFunc,
	) ([]core.RoutingTarget, error)
}

// RecurseFunc evaluates a child strategy node, incrementing depth.
type RecurseFunc func(
	ctx context.Context,
	node core.StrategyNode,
	rctx *core.RoutingContext,
	trace *[]core.TraceEntry,
	depth int,
) ([]core.RoutingTarget, error)

// StrategyRegistry maps strategy type names to their implementations.
// After Freeze is called, no further registrations are allowed.
type StrategyRegistry struct {
	mu         sync.RWMutex
	strategies map[string]Strategy
	frozen     bool
}

// NewStrategyRegistry creates an empty strategy registry.
func NewStrategyRegistry() *StrategyRegistry {
	return &StrategyRegistry{strategies: make(map[string]Strategy)}
}

// Register adds a strategy implementation. Panics on duplicate or if frozen.
func (r *StrategyRegistry) Register(s Strategy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic("router: registry is frozen")
	}
	name := s.Type()
	if _, exists := r.strategies[name]; exists {
		panic(fmt.Sprintf("router: duplicate strategy %q", name))
	}
	r.strategies[name] = s
}

// Freeze prevents further registrations.
func (r *StrategyRegistry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Evaluate recursively evaluates a strategy node tree.
func (r *StrategyRegistry) Evaluate(
	ctx context.Context,
	node core.StrategyNode,
	rctx *core.RoutingContext,
	trace *[]core.TraceEntry,
	depth int,
) ([]core.RoutingTarget, error) {
	if depth > core.MaxRoutingDepth {
		return nil, ErrMaxDepth
	}

	r.mu.RLock()
	s, ok := r.strategies[node.Type]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("router: unknown strategy type %q", node.Type)
	}

	return s.Evaluate(ctx, node, rctx, trace, depth, r.recurse)
}

// recurse is the RecurseFunc passed to strategy implementations.
func (r *StrategyRegistry) recurse(
	ctx context.Context,
	node core.StrategyNode,
	rctx *core.RoutingContext,
	trace *[]core.TraceEntry,
	depth int,
) ([]core.RoutingTarget, error) {
	return r.Evaluate(ctx, node, rctx, trace, depth+1)
}

// SmartDeps groups the dependencies injected into the smart strategy.
//
// The router-LLM call path is now driven entirely through
// [provtarget.Resolver] + [providers.Registry]; there is no inline
// credential or base-URL lookup. SmartStore is kept for candidate
// enumeration only, and Lookup continues to produce the final
// [core.RoutingTarget] returned to the caller (the routing engine's native
// shape).
// SmartDeps holds the dependencies SmartStrategy needs. RouterLLM is
// the injected decision engine — the strategy does not import provider
// adapters or the provtarget resolver directly; those concerns live
// behind the routerllm.Decider interface and its production
// AdapterDecider implementation.
type SmartDeps struct {
	Store     core.SmartStore
	Lookup    core.TargetLookup
	RouterLLM llm.Decider
	Logger    *slog.Logger
}

// RegisterAllStrategies registers all built-in strategy implementations.
// If smartDeps is non-nil the "smart" strategy (model=auto) is also registered.
func RegisterAllStrategies(reg *StrategyRegistry, lookup core.TargetLookup, smartDeps *SmartDeps) {
	reg.Register(&SingleStrategy{lookup: lookup})
	reg.Register(&FallbackStrategy{})
	reg.Register(&LoadbalanceStrategy{})
	reg.Register(&ConditionalStrategy{})
	reg.Register(&ABSplitStrategy{lookup: lookup})
	reg.Register(&PolicyStrategy{})
	if smartDeps != nil {
		reg.Register(&SmartStrategy{deps: *smartDeps})
	}
}
