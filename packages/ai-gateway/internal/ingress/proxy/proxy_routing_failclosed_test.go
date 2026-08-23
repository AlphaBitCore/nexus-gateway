package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// erroringRouter fails every resolution with a scripted error.
type erroringRouter struct{ err error }

func (e erroringRouter) ResolveTargets(context.Context, *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return nil, e.err
}

// TestResolveRouteOrPassthrough_ARealResolverErrorDoesNotBecomeAPassthrough.
//
// The parallel handlers (speech-to-text, video, realtime) degrade to "serve the
// model the caller asked for" when NO routing rule matched, so a deployment
// without rules authored for those endpoints still works. That degradation is
// keyed on one specific signal — an EMPTY no-compatible-provider error, which
// is how the resolver says "no rules enabled".
//
// Every other error is a fault: a strategy that failed to evaluate, a
// catalogue read that errored inside the target lookup. Degrading on those
// serves the caller's model and drops the admin's routing entirely, and the
// request returns 200 — so the rule that was supposed to redirect this traffic
// to a cheaper or compliant provider is bypassed with nothing in the response
// to say so. The bypass is invisible precisely because it succeeds.
//
// A non-empty no-compatible-provider error is included because it is the near
// miss: same type, opposite meaning. It says the resolver DID have candidates
// and rejected them all on capability, which is a real answer and must surface.
func TestResolveRouteOrPassthrough_ARealResolverErrorDoesNotBecomeAPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "a strategy that failed to evaluate",
			err:  errors.New("strategy evaluation failed: catalogue unavailable"),
		},
		{
			name: "candidates existed and every one was rejected on capability",
			err: &routingcore.NoCompatibleProviderError{
				Available: []routingcore.CandidateCapability{{Provider: "openai", Model: "text-embedding-3-small"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(&Deps{
				Router: erroringRouter{err: tc.err},
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				// Models is nil: if the fail-closed guard were removed, the
				// passthrough would be attempted and this test would see the
				// passthrough's own error instead of the resolver's — which is
				// the substitution the assertion below detects.
			})
			r := freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
			rctxFull := requestcontext.NewBuilder().
				WithEndpoint(string(typology.EndpointKindChat)).
				WithHeaders(r.Header).
				Build()

			res, err := h.resolveRouteOrPassthrough(context.Background(), rctxFull,
				Ingress{WireShape: typology.WireShapeOpenAIChat}, "gpt-4o", typology.EndpointKindChat)

			if err == nil {
				t.Fatalf("a resolver fault was absorbed and the request resolved to %+v — the "+
					"caller's own model is served, the admin's routing rule never applies, and "+
					"the 200 gives nobody a reason to look", res)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want the resolver's own error — replacing it with the "+
					"passthrough's error hides which layer actually failed", err)
			}
		})
	}
}

// TestResolveRouteOrPassthrough_TheNoRulesSignalStillDegrades.
//
// The other half of the same rule, adjacent so the difference between the two
// errors is impossible to read as an accident. An EMPTY no-compatible-provider
// error means no rules are enabled at all, and that MUST fall through to the
// requested-model passthrough — otherwise a deployment that has authored no
// rules for speech-to-text gets a hard failure on every request, while chat and
// image work fine, and nothing explains the difference.
func TestResolveRouteOrPassthrough_TheNoRulesSignalStillDegrades(t *testing.T) {
	display := "deepseek"
	h := NewHandler(&Deps{
		Router: erroringRouter{err: &routingcore.NoCompatibleProviderError{}},
		Models: fallbackModelLookupStub{model: &store.Model{
			Enabled: true, ProviderEnabled: true, Status: "active",
			ID:                  "model-1",
			Name:                "gpt-4o",
			ProviderID:          "provider-1",
			ProviderDisplayName: &display,
			ProviderModelID:     "gpt-4o",
		}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	rctxFull := requestcontext.NewBuilder().
		WithEndpoint(string(typology.EndpointKindChat)).
		WithHeaders(r.Header).
		Build()

	res, err := h.resolveRouteOrPassthrough(context.Background(), rctxFull,
		Ingress{BodyFormat: provcore.FormatOpenAI}, "gpt-4o", typology.EndpointKindChat)
	if err != nil {
		t.Fatalf("the no-rules signal did not degrade: %v — an endpoint with no authored "+
			"rules fails outright while the endpoints that have them keep working", err)
	}
	if len(res.AllTargets()) != 1 || res.AllTargets()[0].ModelID != "model-1" {
		t.Fatalf("targets = %+v, want the requested model served directly", res.AllTargets())
	}
	if res.RuleID != "passthrough-fallback" {
		t.Errorf("ruleID = %q, want passthrough-fallback — the traffic row must say the "+
			"request was served without a rule, not attribute it to one", res.RuleID)
	}
}

// TestStageRouting_ANamedModelTheKeyLacksIsRefusedNotPassedThrough.
//
// The refusal and the passthrough are one decision apart. Passthrough exists
// for "no rule matched — serve the model the caller asked for", and the model
// the caller asked for is exactly what this key is denied. Treating the refusal
// as a routing miss answers the request from that model, which reverses the
// decision that was just made and does it silently: the client sees a 200 from
// the model it named, and the allow list it violated is nowhere in the
// exchange.
func TestStageRouting_ANamedModelTheKeyLacksIsRefusedNotPassedThrough(t *testing.T) {
	display := "openai"
	h := NewHandler(&Deps{
		Router: erroringRouter{err: &routingcore.ModelNotAllowedError{RequestedModel: "gpt-4o"}},
		// A catalogue that WOULD serve the model, so a fall-through to the
		// passthrough succeeds rather than failing for an unrelated reason —
		// otherwise this test could pass on the wrong error.
		Models: fallbackModelLookupStub{model: &store.Model{
			Enabled: true, ProviderEnabled: true, Status: "active",
			ID: "model-1", Name: "gpt-4o", ProviderID: "provider-1",
			ProviderDisplayName: &display, ProviderModelID: "gpt-4o",
		}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	rctxFull := requestcontext.NewBuilder().
		WithEndpoint(string(typology.EndpointKindChat)).
		WithHeaders(r.Header).
		Build()

	res, err := h.resolveRouteOrPassthrough(context.Background(), rctxFull,
		Ingress{BodyFormat: provcore.FormatOpenAI}, "gpt-4o", typology.EndpointKindChat)

	if err == nil {
		t.Fatalf("the refusal was absorbed and the request resolved to %+v — the caller is "+
			"served by the very model their key forbids, and nothing says the allow list was "+
			"crossed", res)
	}
	var notAllowed *routingcore.ModelNotAllowedError
	if !errors.As(err, &notAllowed) {
		t.Errorf("err = %v, want the model-not-allowed refusal to reach the handler intact — "+
			"replaced by a routing error it becomes a 404 about a model that exists", err)
	}
}
