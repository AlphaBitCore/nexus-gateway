package executor

import (
	"context"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// API is the subset of [TargetExecutor] the AI Gateway handler depends
// on. Production wires *TargetExecutor directly; tests substitute fakes
// to drive error arms (e.g. ErrAllTargetsExhausted, transport panics,
// 4xx provider envelopes) without standing up a real provider + upstream
// HTTP server. Kept small and additive-only — the handler should never
// reach for low-level executor internals (resolver, adapters, health,
// stats) directly.
type API interface {
	// Execute walks targets using base as the client-originated request,
	// honoring the supplied RetryPolicy. Mirrors
	// (*TargetExecutor).Execute exactly.
	Execute(
		ctx context.Context,
		targets []routingcore.RoutingTarget,
		base provcore.Request,
		policy cfgpolicy.RetryPolicy,
	) *ExecutionResult
	// ExecuteWithPreparedBody is Execute with the body for targets[0]'s
	// first attempt already produced by Adapter.PrepareBody. Mirrors
	// (*TargetExecutor).ExecuteWithPreparedBody exactly.
	ExecuteWithPreparedBody(
		ctx context.Context,
		targets []routingcore.RoutingTarget,
		base provcore.Request,
		policy cfgpolicy.RetryPolicy,
		preparedBody []byte,
		preparedRewrites []string,
	) *ExecutionResult
}

// Compile-time assertion that the production type satisfies the API.
var _ API = (*TargetExecutor)(nil)
