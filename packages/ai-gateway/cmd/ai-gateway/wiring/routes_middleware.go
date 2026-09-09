// routes_middleware.go — the outer HTTP middleware chain, split out of routes.go
// to keep that file under its size ratchet. applyMiddleware wraps the mounted
// mux with the connection stage, request logging, panic recovery, optional CORS,
// tracing, and request-id propagation — in the order the data plane requires.
package wiring

import (
	"context"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// applyMiddleware wraps the mounted route handler with the ai-gateway middleware
// chain and returns the fully-wrapped handler.
func applyMiddleware(mux http.Handler, deps RouteDeps) http.Handler {
	h := mux
	h = middleware.ConnectionStage(
		func(ctx context.Context) (*pipeline.PolicyResolver, error) {
			return deps.HookConfigCache.Resolver(ctx), nil
		},
		5*time.Second, 30*time.Second, "AI_GATEWAY", deps.Logger,
	)(h)
	h = middleware.Logger(deps.Logger)(h)
	h = middleware.Recovery(deps.Logger)(h)
	if deps.Config.CORS.Enabled {
		// CORS assembly (composed request-header allowlist) lives in cors.go.
		h = corsMiddleware(deps)(h)
	}
	h = telemetry.HTTPTrace("nexus-ai-gateway")(h)
	// RequestID wraps outside HTTPTrace so the resolved request id is on the
	// request context before the server span is created: for a caller who sent
	// no traceparent, the tracer's IDGenerator derives the root span's trace id
	// from it, so the span is findable from the id we hand that caller back.
	h = middleware.RequestID(h)
	return h
}
