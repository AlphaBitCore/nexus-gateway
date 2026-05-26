package proxy

import (
	"context"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/responseprehook"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
)

// buildStreamPreHookCallback returns a streaming.PreHookCallback that
// runs the cumulative raw SSE wire bytes through deps.NormalizeRegistry
// (Tier 1+2+3) and stamps the result onto the HookInput's Normalized
// field so response hooks see the same Registry payload shape that the
// non-stream path uses. Mirrors shared/transport/tlsbump.buildSSEPreHookCallback
// (#90) so the three ingress services (agent / compliance-proxy /
// ai-gateway) all share the same compliance contract.
//
// #93 — implementation delegates to shared
// transport/normalize/responseprehook.Build; this wrapper survives only
// because ai-gateway's Deps struct + adapter-type resolution differs
// from tlsbump's bumpOptions / audCtx — the wrapper does the local
// field plumbing, then hands off the canonical builder.
//
// Returns nil when deps or deps.NormalizeRegistry is unwired —
// LivePipeline then keeps the pre-#91 flat-text fallback. Hot path:
// nil/empty body / Normalize hard error all silently drop; never abort
// hook execution because normalize stumbled (Registry already debug-
// logs the why via its per-tier INFO traces, #87).
func buildStreamPreHookCallback(
	ctx context.Context,
	deps *Deps,
	resolvedAdapterType string,
	endpointPath string,
	acceptHeader string,
) streaming.PreHookCallback {
	if deps == nil {
		return nil
	}
	return responseprehook.Build(responseprehook.Options{
		Ctx:          ctx,
		Registry:     deps.NormalizeRegistry,
		AdapterID:    resolvedAdapterType,
		EndpointPath: endpointPath,
		ContentType:  acceptHeader,
		Direction:    normcore.DirectionResponse,
	})
}
