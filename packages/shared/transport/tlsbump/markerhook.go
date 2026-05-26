package tlsbump

import (
	"context"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/responseio"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// defaultIdentity is the via-name stamped when the caller did not supply
// one. compliance-proxy was the original caller so its name is the
// safe fallback; new call sites (agent bridge) MUST pass WithIdentity.
const defaultIdentity = "compliance-proxy"

// Per-spec marker header names. There is no per-service prefix anymore —
// every hop writes to the canonical X-Nexus-* headers and uses traffic
// chain helpers (PrependVia / PrependChain) to compose multi-hop values
// 1:1 with the via chain. See nexus-response-markers.md.
const (
	requestIDHeader  = "X-Nexus-Request-Id"
	hookHeader       = "X-Nexus-Hook"
	modeHeader       = "X-Nexus-Mode"
	domainRuleHeader = "X-Nexus-Domain-Rule"
)

// stampMarkers writes X-Nexus-Via + the canonical chain markers (hook +
// mode) + the CP-only X-Nexus-Domain-Rule directly onto h. Shared by the
// non-streaming path (via markerHook, which wraps a *http.Response) and
// the SSE path (which writes directly to an http.ResponseWriter before
// the first flush).
//
// `identity` controls the value stamped on X-Nexus-Via (e.g.
// "compliance-proxy", "agent"). Empty string falls back to the historical
// "compliance-proxy" default.
//
// Reads the per-request CPMarker from ctx; falls back to a minimal mode-only
// marker when absent (e.g. compliance-disabled fast path or CONNECT tunnel).
//
// The hook + mode + via fields prepend into the chain so a downstream
// hop (e.g. ai-gateway) can have stamped its own values first; this hop
// then prepends its values at position 0. Strict 1:1 alignment with
// X-Nexus-Via is preserved.
func stampMarkers(ctx context.Context, h http.Header, identity string) {
	if identity == "" {
		identity = defaultIdentity
	}
	m := CPMarkerFromContext(ctx)
	traffic.PrependVia(h, identity)
	if m != nil {
		traffic.PrependChain(h, hookHeader, traffic.FormatHookOutcome(m.HookOutcome))
		if m.DomainRuleID != "" {
			// Domain-Rule is CP-only and has no chain semantics — Set
			// (overwrite) is correct because no other hop ever stamps
			// this header.
			h.Set(domainRuleHeader, m.DomainRuleID)
		}
		if m.RequestID != "" {
			h.Set(requestIDHeader, m.RequestID)
		}
		// mode = "inspect" 表示 hook pipeline 跑过 (CPMarker 存在);
		// reject 路径走 stampRejectMarkers stamp "deny"。
		traffic.PrependChain(h, modeHeader, "inspect")
	}
	// When m == nil (compliance disabled path) only the via header is emitted;
	// downstream consumers treat the absence of marker chain entries as
	// "no inspection".
	traffic.MergeExposeHeaders(h,
		traffic.HeaderVia,
		requestIDHeader, modeHeader, hookHeader, domainRuleHeader)
}

// markerHook returns a HeaderHook that injects X-Nexus-Via + the canonical
// marker chain headers onto the upstream response before it is forwarded
// to the client. Delegates to stampMarkers so that both the non-streaming
// (responseio.Copy) and SSE paths use identical stamping logic.
//
// The hook fires after static + dynamic hop-by-hop stripping (per Task 0.3
// hook ordering), so marker headers will not be accidentally removed.
func markerHook(ctx context.Context, identity string) responseio.HeaderHook {
	return func(resp *http.Response) {
		stampMarkers(ctx, resp.Header, identity)
	}
}

// stampRejectMarkers writes X-Nexus-Via + the canonical marker chain
// headers onto h for a synthetic reject response (403 / 451). Because the
// response is synthesized locally (no upstream CORS headers to preserve),
// it calls SetExposeHeaders rather than MergeExposeHeaders so the list is
// authoritative.
//
// Reject responses are always the result of this hop's own decision (no
// downstream chain to prepend into), so the marker chain values are set
// verbatim — no inner-hop position to preserve.
//
// Must be called BEFORE WriteRejectResponse (which calls w.WriteHeader,
// after which Go's net/http runtime locks the header map).
func stampRejectMarkers(h http.Header, identity, requestID, domainRuleID string, outcome traffic.HookOutcomeInput) {
	if identity == "" {
		identity = defaultIdentity
	}
	traffic.PrependVia(h, identity)
	h.Set(hookHeader, traffic.FormatHookOutcome(outcome))
	if domainRuleID != "" {
		h.Set(domainRuleHeader, domainRuleID)
	}
	if requestID != "" {
		h.Set(requestIDHeader, requestID)
	}
	// 拒绝(deny)路径: stamp mode=deny 区别于 inspect 路径,让客户端能区分
	// "通过 inspect 后被 hook block" 和 "fast-path 直通"。
	h.Set(modeHeader, "deny")
	traffic.SetExposeHeaders(h)
}
