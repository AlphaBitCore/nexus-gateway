package proxy

import (
	"context"
	"testing"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// probeRouter implements RouteResolver AND the model-match-aware content-rule
// probe. needs is returned verbatim, regardless of the model argument, so a test
// can simulate "a smart rule matches this model" / "no smart rule matches".
type probeRouter struct{ needs bool }

func (probeRouter) ResolveTargets(context.Context, *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return nil, nil
}
func (p probeRouter) RequestNeedsCanonical(context.Context, string) bool { return p.needs }

// noProbeRouter implements only RouteResolver (no content-rule probe), modeling
// a resolver/mocks that predate the method.
type noProbeRouter struct{}

func (noProbeRouter) ResolveTargets(context.Context, *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return nil, nil
}

// TestHandler_smartRouteNeedsCanonical locks the routing-side canonical gate.
// It materializes the request canonical for the router ONLY when a smart rule
// could match the requested model. Fail-safe to true when the kill-switch is off
// or the router lacks the probe; the response cache is NOT part of this gate (it
// pulls the canonical independently in the cache stage).
func TestHandler_smartRouteNeedsCanonical(t *testing.T) {
	tests := []struct {
		name   string
		lazy   bool
		router RouteResolver
		want   bool
	}{
		{"flag off forces compute (kill-switch)", false, probeRouter{needs: false}, true},
		{"flag off ignores a nil router", false, nil, true},
		{"lazy + no matching smart rule -> skip", true, probeRouter{needs: false}, false},
		{"lazy + matching smart rule -> compute", true, probeRouter{needs: true}, true},
		{"lazy + router without probe -> fail-safe compute", true, noProbeRouter{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{Router: tt.router}, lazyCanonical: tt.lazy}
			if got := h.smartRouteNeedsCanonical(context.Background(), "gpt-4o"); got != tt.want {
				t.Fatalf("smartRouteNeedsCanonical = %v, want %v", got, tt.want)
			}
		})
	}
}
