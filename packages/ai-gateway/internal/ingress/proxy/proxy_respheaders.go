package proxy

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// proxy_respheaders.go holds the response-header writers (forwarded upstream headers +
// Nexus's own x-nexus-* / Server-Timing stamps) split out of proxy.go (behavior
// unchanged).

func allowlistVersionFromDeps(d *Deps) string {
	if d == nil || d.Allowlist == nil {
		return ""
	}
	return d.Allowlist.Hash()
}

// writeForwardedResponseHeaders applies the resolved response-side
// forward-header allowlist to upstream headers and writes the
// permitted set onto w. Per-request headers (e.g. `x-request-id`,
// `x-ratelimit-*-tokens`, `openai-processing-ms`) are stripped on
// cache HIT (`isCacheHit == true`); replaying a stale per-request
// value is worse than not surfacing it.
//
// MUST be called BEFORE [Handler.setResponseHeaders] /
// [Handler.setResponseHeadersStream] so Nexus's own
// `x-nexus-aigw-*` stamps overwrite any conflicting upstream value
// ("Nexus wins on conflict").
//
// Safe with a nil allowlist (falls back to embedded defaults via
// provcore.FilterResponseHeaders) and with an empty / nil src.
//
// Prefers the live atomic snapshot from forwardheader.Active (set once
// from yaml at boot) so the response writer reads the same allowlist
// every request without re-resolving. The supplied parameter stays as
// the test/early-startup fallback.
func writeForwardedResponseHeaders(w http.ResponseWriter, allowlist *forwardheader.Resolved, format provcore.Format, src http.Header, isCacheHit bool) {
	if len(src) == 0 {
		return
	}
	if live := forwardheader.Active(); live != nil {
		allowlist = live
	}
	filtered := provdispatch.FilterResponseHeaders(allowlist, format, src, isCacheHit)
	for k, vs := range filtered {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// setResponseHeaders writes standard Nexus response headers for non-streaming
// responses. It initialises the x-nexus-via chain and stamps latency.
//
// `attempts` is the total upstream attempt count for this request — 1 means
// first-try success, 2+ means at least one L2 retry or L3 failover happened.
// The header is emitted on every response (no `> 0` gate) so observers can
// always tell whether failover engaged.
func (h *Handler) setResponseHeaders(w http.ResponseWriter, rec *audit.Record, target routingcore.RoutingTarget, result *routingcore.RouteResult, start time.Time, attempts int) {
	traffic.PrependVia(w.Header(), "ai-gateway")
	// X-Nexus-Mode is reserved as an empty position so an outer hop
	// (agent, compliance-proxy) preserves 1:1 alignment with X-Nexus-Via
	// when it prepends its own mode value. AI Gateway has no mode concept.
	w.Header().Set("X-Nexus-Mode", "")
	// Customer-facing model identifier — the same string the caller
	// sent in `{"model": "..."}`. Internal UUIDs stay out of the
	// headers; correlation against the catalog uses the code.
	if attempts < 1 {
		attempts = 1 // defensive — should never be 0 if we reached this code path
	}
	w.Header().Set("X-Nexus-Attempts", strconv.Itoa(attempts))
	if result.Substituted {
		w.Header().Set("X-Nexus-Routed-Model", target.ModelCode)
		w.Header().Set("X-Nexus-Routed-Provider", target.ProviderName)
	}
	// Server-Timing (RFC 8674) exposes gateway/upstream latency
	// breakdowns. Native browser DevTools support; comma-separated tokens.
	parts := make([]string, 0, 3)
	gwTotalMs := time.Since(start).Milliseconds()
	if rec.UpstreamTotalMs != nil {
		gwOverhead := gwTotalMs - int64(*rec.UpstreamTotalMs)
		if gwOverhead < 0 {
			gwOverhead = 0
		}
		parts = append(parts, fmt.Sprintf("gw;dur=%d", gwOverhead))
		if rec.UpstreamTtfbMs != nil {
			parts = append(parts, fmt.Sprintf("upstream-ttfb;dur=%d", *rec.UpstreamTtfbMs))
		}
		parts = append(parts, fmt.Sprintf("upstream-total;dur=%d", *rec.UpstreamTotalMs))
	} else {
		parts = append(parts, fmt.Sprintf("gw;dur=%d", gwTotalMs))
	}
	w.Header().Set("Server-Timing", strings.Join(parts, ", "))
}

// setResponseHeadersStream writes standard Nexus response headers for
// streaming (SSE) responses. It initialises the x-nexus-via chain but
// omits latency (per spec §5 — latency is meaningless on a streaming
// response where the last byte arrives long after headers are sent).
//
// `attempts` is the total upstream attempt count for this request — 1 means
// first-try success, 2+ means at least one L2 retry or L3 failover happened.
// The header is emitted on every response (no `> 0` gate) so observers can
// always tell whether failover engaged.
func (h *Handler) setResponseHeadersStream(w http.ResponseWriter, rec *audit.Record, target routingcore.RoutingTarget, result *routingcore.RouteResult, attempts int) {
	traffic.PrependVia(w.Header(), "ai-gateway")
	// Reserve X-Nexus-Mode position for outer-hop 1:1 alignment with the
	// via chain — same rationale as setResponseHeaders above.
	w.Header().Set("X-Nexus-Mode", "")
	if attempts < 1 {
		attempts = 1 // defensive — should never be 0 if we reached this code path
	}
	w.Header().Set("X-Nexus-Attempts", strconv.Itoa(attempts))
	if result.Substituted {
		w.Header().Set("X-Nexus-Routed-Model", target.ModelCode)
		w.Header().Set("X-Nexus-Routed-Provider", target.ProviderName)
	}
}
