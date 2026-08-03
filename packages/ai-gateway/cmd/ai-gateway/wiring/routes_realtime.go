// routes_realtime.go — the realtime voice relay route, split out of
// routes.go for the file-size ratchet.
package wiring

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// mountRealtimeRoutes registers the realtime voice WebSocket relay. A
// parallel handler (ServeRealtime), not ServeProxy: a long-lived
// bidirectional WS session has no request body, no cache, no codec, and no
// per-request hook pipeline. WireShapeNone — no codec ever dispatches on
// this route; the OpenAI BodyFormat drives the pre-upgrade error envelope
// shape and the provider-side auth scheme.
func mountRealtimeRoutes(mux *http.ServeMux, proxyHandler *proxy.Handler) {
	mux.HandleFunc("GET /v1/realtime", proxyHandler.ServeRealtime(proxy.Ingress{
		WireShape:  typology.WireShapeNone,
		BodyFormat: provcore.FormatOpenAI,
	}))
}
