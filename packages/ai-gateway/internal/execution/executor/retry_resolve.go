package executor

import (
	"context"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// reResolveForRetry asks the credential pool for a fresh CallTarget before an
// L2 retry re-sends to the same target.
//
// Why a retry cannot reuse the CallTarget resolved before the loop: the very
// failure that triggered the retry may have opened that credential's circuit.
// A single 429 opens it, and the third consecutive 401/403 opens it too
// (packages/ai-gateway/internal/credentials/stats/buffer.go). Reusing the
// resolved target would therefore re-send straight into a breaker we just
// tripped ourselves, and credential rotation would only ever happen on L3
// failover to a *different* target — leaving a multi-credential pool unable to
// do the one thing it exists for.
//
// Only the credential fields can differ from the pre-loop resolve: providerID
// and modelID are fixed for a given target, so Format, BaseURL and
// ProviderModelID — and the adapter, WireShape and request body derived from
// them — stay valid. The caller swaps only req.Target.
//
// The sticky key is carried through, so when the credential's circuit is still
// closed the pool's consistent hash returns the same credential and the
// re-resolve is cheap. What it costs depends on the pool: a single-credential
// pool short-circuits before any circuit lookup and stays in memory (~138ns,
// 2 allocations, measured), while a pool of two or more reads circuit state
// from Redis and is dominated by that one round trip (~29us). The credential
// list itself is served from a cachelayer snapshot in production, not the
// database.
//
// An error means no candidate is eligible — every credential's circuit is
// open. The caller must drop to L3 rather than spend the rest of its L2 budget
// re-asking a pool that has nothing to give.
func (e *TargetExecutor) reResolveForRetry(ctx context.Context, target routingcore.RoutingTarget, stickyKey string) (provcore.CallTarget, error) {
	return e.resolver.Resolve(ctx, target.ProviderID, target.ModelID, provtarget.ResolveHints{StickyKey: stickyKey})
}
