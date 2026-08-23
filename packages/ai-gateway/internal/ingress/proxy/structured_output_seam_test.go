package proxy

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// captureRouter records the RoutingContext it was handed, so a test can assert
// what the proxy actually put in it rather than what the proxy meant to.
type captureRouter struct{ got *routingcore.RoutingContext }

func (c *captureRouter) ResolveTargets(_ context.Context, rctx *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	c.got = rctx
	return &routingcore.RouteResult{}, nil
}

// The other half of the seam two mutants exposed. `requestRequiresStructuredOutput`
// has its own table test, and the capability filter has its own — but replacing
// the assignment in resolveRoute with a literal `false` left both green while
// the router lost the requirement entirely. This asserts the value TRAVELS.
func TestResolveRoute_CarriesTheStructuredOutputRequirement(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"a json_schema request arrives at the router as a requirement",
			`{"model":"auto","messages":[{"role":"user","content":"hi"}],` +
				`"response_format":{"type":"json_schema","json_schema":{"name":"v"}}}`, true},
		{"a json_object request does not constrain the pool",
			`{"model":"auto","messages":[{"role":"user","content":"hi"}],` +
				`"response_format":{"type":"json_object"}}`, false},
		{"a plain request does not constrain the pool",
			`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &captureRouter{}
			h := &Handler{deps: &Deps{Router: cap}}
			rctxFull := requestcontext.NewBuilder().
				WithRawBody([]byte(tt.body)).
				// The canonical must be present: the requirement is read only
				// when a smart rule could actually pick the model, since an
				// explicitly named model is the caller's own choice.
				WithNormalized(&normcore.NormalizedPayload{Kind: normcore.KindAIChat}).
				Build()

			if _, err := h.resolveRoute(context.Background(), rctxFull, "auto",
				typology.EndpointKindChat); err != nil {
				t.Fatalf("resolveRoute: %v", err)
			}
			if cap.got == nil {
				t.Fatal("the router was never called, so nothing was asserted")
			}
			if cap.got.RequiresStructuredOutput != tt.want {
				t.Errorf("RoutingContext.RequiresStructuredOutput = %v, want %v — the proxy did "+
					"not carry the requirement to the router",
					cap.got.RequiresStructuredOutput, tt.want)
			}
		})
	}
}

// leanRouter is a Router that also answers the lazy-canonical probe, so a test
// can put resolveRoute on the path where NO smart rule could apply.
type leanRouter struct {
	captureRouter
	needsCanonical bool
}

func (l *leanRouter) RequestNeedsCanonical(context.Context, string) bool { return l.needsCanonical }

// The FALSE side of the `canonReq != nil` guard, which nothing exercised.
//
// On the lean path no rule tree contains a smart strategy, so the canonical is
// never materialised and the only consumer of this flag — the smart capability
// filter — cannot run. Reading the requirement there would buy four gjson
// lookups per request on the hot path for a value nothing reads.
//
// Asserted because a guard with no test is a guard that can be deleted or
// inverted silently: inverted, every lean request starts paying for the parse;
// deleted, the flag's meaning quietly changes from "the router needs this" to
// "the body has this", and the next reader of the field inherits the wrong one.
func TestResolveRoute_leanPathDoesNotReadTheRequirement(t *testing.T) {
	const schemaBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"v"}}}`

	for _, tc := range []struct {
		name           string
		needsCanonical bool
		want           bool
	}{
		{"lean path: the canonical is never materialised", false, false},
		{"smart path: the same body IS read", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := &leanRouter{needsCanonical: tc.needsCanonical}
			h := &Handler{deps: &Deps{Router: router}, lazyCanonical: true}
			rctxFull := requestcontext.NewBuilder().
				WithRawBody([]byte(schemaBody)).
				WithNormalized(&normcore.NormalizedPayload{Kind: normcore.KindAIChat}).
				Build()

			if _, err := h.resolveRoute(context.Background(), rctxFull, "gpt-4o",
				typology.EndpointKindChat); err != nil {
				t.Fatalf("resolveRoute: %v", err)
			}
			if router.got == nil {
				t.Fatal("the router was never called, so nothing was asserted")
			}
			// Both halves are asserted together: without the true case the false
			// one is satisfied by a resolveRoute that never sets the flag at all.
			if router.got.RequiresStructuredOutput != tc.want {
				t.Errorf("RequiresStructuredOutput = %v, want %v (RequestNeedsCanonical=%v)",
					router.got.RequiresStructuredOutput, tc.want, tc.needsCanonical)
			}
			if (router.got.Request != nil) != tc.needsCanonical {
				t.Errorf("rctx.Request non-nil = %v, want %v — the flag and the canonical "+
					"must be gated by the same decision, or the guard is testing "+
					"something other than what it reads",
					router.got.Request != nil, tc.needsCanonical)
			}
		})
	}
}
