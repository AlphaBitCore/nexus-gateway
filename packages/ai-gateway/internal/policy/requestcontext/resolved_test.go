package requestcontext

import (
	"context"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestResolve_AllNonNil_ReturnsWrappedPointers(t *testing.T) {
	id := &vkauth.VKMeta{ID: "vk-1"}
	payload := &normalize.NormalizedPayload{Kind: normalize.KindAIChat}
	rc := NewBuilder().WithIdentity(id).WithNormalized(payload).Build()

	route := &routingcore.RouteResult{RuleID: "rule-1"}
	ptc := &passthrough.Config{Enabled: true, BypassHooks: true}

	rr := Resolve(rc, route, ptc)
	if rr == nil {
		t.Fatal("Resolve returned nil")
	}
	if rr.Base() != rc {
		t.Errorf("Base() = %p, want %p", rr.Base(), rc)
	}
	if rr.Route() != route {
		t.Errorf("Route() = %p, want %p", rr.Route(), route)
	}
	if rr.Passthrough() != ptc {
		t.Errorf("Passthrough() = %p, want %p", rr.Passthrough(), ptc)
	}
}

func TestResolve_NilInputs_AreRetained(t *testing.T) {
	rr := Resolve(nil, nil, nil)
	if rr == nil {
		t.Fatal("Resolve(nil,nil,nil) returned nil; ResolvedRequest itself should still be constructed")
	}
	if rr.Base() != nil {
		t.Errorf("Base() = %v, want nil", rr.Base())
	}
	if rr.Route() != nil {
		t.Errorf("Route() = %v, want nil", rr.Route())
	}
	if rr.Passthrough() != nil {
		t.Errorf("Passthrough() = %v, want nil", rr.Passthrough())
	}
}

func TestResolvedRequest_Delegates_ForwardToBase(t *testing.T) {
	id := &vkauth.VKMeta{ID: "vk-1"}
	payload := &normalize.NormalizedPayload{Kind: normalize.KindAIChat}
	hdrs := http.Header{"X-Tag": []string{"v"}}
	raw := []byte(`{"hello":"world"}`)

	rc := NewBuilder().
		WithIdentity(id).
		WithNormalized(payload).
		WithEndpoint("chat/completions").
		WithHeaders(hdrs).
		WithRawBody(raw).
		Build()

	rr := Resolve(rc, nil, nil)

	if rr.Identity() != id {
		t.Errorf("Identity() = %p, want %p", rr.Identity(), id)
	}
	if rr.Normalized() != payload {
		t.Errorf("Normalized() = %p, want %p", rr.Normalized(), payload)
	}
	if got := rr.Endpoint(); got != "chat/completions" {
		t.Errorf("Endpoint() = %q, want chat/completions", got)
	}
	if got := rr.Headers().Get("X-Tag"); got != "v" {
		t.Errorf("Headers().Get(X-Tag) = %q, want v", got)
	}
	if got := string(rr.RawBody()); got != string(raw) {
		t.Errorf("RawBody() = %q, want %q", got, string(raw))
	}
}

func TestResolvedRequest_NilReceiver_AllSafe(t *testing.T) {
	var rr *ResolvedRequest

	if rr.Base() != nil || rr.Route() != nil || rr.Passthrough() != nil {
		t.Errorf("nil receiver getters should return nil; got base=%v route=%v ptc=%v",
			rr.Base(), rr.Route(), rr.Passthrough())
	}
	if rr.Identity() != nil || rr.Normalized() != nil || rr.Headers() != nil || rr.RawBody() != nil {
		t.Errorf("nil receiver delegates should return nil zero values")
	}
	if got := rr.Endpoint(); got != "" {
		t.Errorf("nil.Endpoint() = %q, want empty", got)
	}
}

func TestWithResolved_RoundTripsThroughContext(t *testing.T) {
	rr := Resolve(NewBuilder().WithEndpoint("chat/completions").Build(), nil, nil)
	ctx := WithResolved(context.Background(), rr)
	got := ResolvedFrom(ctx)
	if got != rr {
		t.Errorf("ResolvedFrom returned %p, want %p", got, rr)
	}
}

func TestResolvedFrom_NoValue_ReturnsNil(t *testing.T) {
	if got := ResolvedFrom(context.Background()); got != nil {
		t.Errorf("ResolvedFrom on plain ctx = %v, want nil", got)
	}
	if got := ResolvedFrom(nil); got != nil { //nolint:staticcheck // intentional nil
		t.Errorf("ResolvedFrom(nil) = %v, want nil", got)
	}
}
