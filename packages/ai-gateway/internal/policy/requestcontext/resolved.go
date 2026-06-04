package requestcontext

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// resolvedKey is the unexported context key used for stashing
// *ResolvedRequest on a request's context.Context. Using an
// unexported type prevents stringly-typed key collisions with
// downstream code that might use plain strings.
type resolvedKey struct{}

// WithResolved returns a context derived from parent that carries the
// supplied *ResolvedRequest. Downstream consumers retrieve via
// ResolvedFrom(ctx).
func WithResolved(parent context.Context, r *ResolvedRequest) context.Context {
	return context.WithValue(parent, resolvedKey{}, r)
}

// ResolvedFrom extracts the *ResolvedRequest from ctx. Returns nil when
// no ResolvedRequest is on ctx — downstream consumers must nil-check
// (they call Passthrough() etc. on the result, both of which are
// nil-safe).
func ResolvedFrom(ctx context.Context) *ResolvedRequest {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(resolvedKey{}).(*ResolvedRequest); ok {
		return v
	}
	return nil
}

// ResolvedRequest is the L4 view that bundles the immutable L3
// RequestContext with the post-routing decisions the handler needs:
// the resolved routing targets and the effective passthrough
// configuration for the picked primary target's provider.
//
// Built once via Resolve(); downstream consumers (hooks pipeline,
// audit Writer, executor, response normalize) receive a pointer and
// treat it as read-only.
//
// Why a separate type and not fields on RequestContext: RequestContext
// is immutable after Builder.Build(). Post-routing fields are not
// known at build time, so adding them as nullable mutators would
// reintroduce after-Build coupling. The wrapper type pins the
// "pre-routing vs post-routing" boundary at the type level:
// pre-routing layers take *RequestContext; post-routing consumers
// take *ResolvedRequest and explicitly opt in to the resolved data.
type ResolvedRequest struct {
	base        *RequestContext
	route       *routingcore.RouteResult
	passthrough *passthrough.Config
}

// Resolve constructs a ResolvedRequest wrapping the supplied L3
// RequestContext, post-routing RouteResult, and effective
// PassthroughConfig. All three pointers are retained as-is; the
// constructor does not copy or clone any field.
//
// Any of the three inputs may be nil — Resolve preserves the nil;
// downstream getters return nil accordingly. This matches the
// cold-start fail-closed invariant: when the passthrough cache hasn't
// been populated yet, Resolve(rc, route, nil) is the correct call —
// Passthrough() returns nil and downstream consumers treat that as
// "no bypass" via *Config's nil-safe AnyBypassActive / Flags methods.
func Resolve(rc *RequestContext, route *routingcore.RouteResult, ptc *passthrough.Config) *ResolvedRequest {
	return &ResolvedRequest{base: rc, route: route, passthrough: ptc}
}

// Base returns the wrapped *RequestContext (the L3 immutable artefact).
// Nil-safe.
func (r *ResolvedRequest) Base() *RequestContext {
	if r == nil {
		return nil
	}
	return r.base
}

// Route returns the wrapped *routingcore.RouteResult (the post-routing
// decision). Nil-safe.
func (r *ResolvedRequest) Route() *routingcore.RouteResult {
	if r == nil {
		return nil
	}
	return r.route
}

// Passthrough returns the wrapped *passthrough.Config (the effective
// merged config for the primary target's provider). Nil-safe.
// Downstream consumers should always use this against the nil-safe
// methods on *passthrough.Config (AnyBypassActive, Flags) rather than
// dereferencing.
func (r *ResolvedRequest) Passthrough() *passthrough.Config {
	if r == nil {
		return nil
	}
	return r.passthrough
}

// Identity delegates to Base().Identity(). Nil-safe.
func (r *ResolvedRequest) Identity() *vkauth.VKMeta {
	return r.Base().Identity()
}

// Normalized delegates to Base().Normalized(). Nil-safe.
func (r *ResolvedRequest) Normalized() *normcore.NormalizedPayload {
	return r.Base().Normalized()
}

// Endpoint delegates to Base().Endpoint(). Nil-safe.
func (r *ResolvedRequest) Endpoint() string {
	return r.Base().Endpoint()
}

// Headers delegates to Base().Headers(). Nil-safe.
func (r *ResolvedRequest) Headers() http.Header {
	return r.Base().Headers()
}

// RawBody delegates to Base().RawBody(). Nil-safe.
func (r *ResolvedRequest) RawBody() []byte {
	return r.Base().RawBody()
}
