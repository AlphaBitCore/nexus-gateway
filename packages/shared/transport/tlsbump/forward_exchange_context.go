package tlsbump

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Context stamping for a bumped exchange.
//
// Split out of forward_exchange.go along the seam finding C-3 created: every value the
// request phases hand to the post-upstream write sites travels the request context, and
// each stamp costs an http.Request clone, so how many stamps there are and when they run
// is a cost decision as much as a plumbing one. Keeping them in one file makes the count
// visible.
//
// The remaining stamps on a bumped request, in execution order:
//
//	newExchange  clientHelloKey   (conditional; read by UpstreamTransport.DialTLSContext)
//	this file    PhaseSink + CPMarker + requestAuditCtx, in ONE clone
//
// Two clones per bumped request, down from four. The PhaseSink stamp used to be its own
// clone in prepare(); it moved here once the reader set was checked. Nothing between
// prepare() and this function reads the sink OFF A CONTEXT — the in-package phases hold
// x.phaseSink directly, and the only context reader in the repo that matters here is the
// tracing RoundTripper (shared/traffic/tracing.go), which runs inside forwardUpstream. Both
// of the repo's ForwardRequest call sites are downstream of this stamp or bypass prepare()
// entirely, so the sink is always on the context by the time anything looks for it.
//
// The ClientHello stamp is NOT mergeable into this one, and the reason is load-bearing:
// attestationPeek runs before prepare() and short-circuits to attestationPassthrough, which
// calls upstream.ForwardRequest itself — and the h2 dialer reads clientHelloKey off the
// request context (upstream_h2.go). Moving that stamp later would silently drop TLS
// fingerprint replay for attested traffic, which is the one class of traffic where the agent
// has already signed the request and CP is meant to be transparent.

// stampCPMarker stashes the per-request marker state on the context so
// that downstream response write sites (the buffered relay in upstream.go
// and the SSE handler in sse.go) can inject the X-Nexus-* chain markers without
// re-deriving these values. The marker is always set — even on the
// compliance-disabled fast path — so callers never need to handle a nil
// check for the basic request-id field.
func (x *bumpedExchange) stampCPMarker() {
	ctx := contextWithCPMarker(x.r.Context(), &CPMarker{
		RequestID:    x.txID,
		DomainRuleID: x.domainRuleID,
		HookOutcome:  cpHookOutcomeFromResult(x.reqHookResult),
	})
	// The request phase's audit context rides along in the same clone. It is nil
	// only when the phase refused the request, and in that case this function is
	// not reached at all — so the nil check is for the compliance-disabled fast
	// path, which stamps a marker without ever running a request phase.
	if x.auditCtx != nil {
		ctx = context.WithValue(ctx, requestAuditKey{}, x.auditCtx)
	}
	// The latency sink rides along too — see the file header for why this is the right clone
	// for it. prepare() always runs before this, so the sink is never nil here.
	ctx = traffic.WithPhaseSink(ctx, x.phaseSink)
	x.r = x.r.WithContext(ctx)
}

// runDeferredAudit emits the audit row the stream-through arm parked for after the relay
// (finding C-34), and is a no-op on every other arm. It runs exactly once: the closure is
// cleared before it is called, so a caller that both defers this and calls it directly
// cannot double-write a row — a duplicate audit row is worse than a late one, because
// nothing downstream deduplicates them.
func (x *bumpedExchange) runDeferredAudit() {
	emit := x.deferredAudit
	if emit == nil {
		return
	}
	x.deferredAudit = nil
	emit()
}
