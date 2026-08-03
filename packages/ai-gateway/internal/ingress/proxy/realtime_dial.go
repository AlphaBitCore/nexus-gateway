package proxy

// realtime_dial.go — the upstream WebSocket dial for the realtime relay.
//
// Credential hygiene rules this file enforces:
//   - The handshake header set is built FROM SCRATCH and carries only the
//     adapter-applied provider auth. Nothing is copied from the inbound
//     request — the client's credential, subprotocols, and headers never
//     reach the provider (forwardheader.Apply is deliberately unused).
//   - The scratch request, dial URL, and dial headers are NEVER logged, and
//     dial errors are not logged or echoed either: coder/websocket embeds
//     the dial URL in its error strings, and upstream host/TLS internals
//     must not leak to the caller. The client sees one STATIC 502 body.

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// dialRealtimeUpstream resolves and dials routed targets in order until one
// WebSocket handshake succeeds, within the pre-accept budget (≤3 targets,
// 8 s per attempt, 24 s overall — the whole thing must finish, and any error
// must be written, before the server's write timeout fires; only a
// successful hijack clears it). After the first success the target is PINNED
// for the session's life — failover is connection-establishment only; there
// is no mid-session re-dial.
//
// On success it returns the conn, the PINNED routing target, and its resolved
// CallTarget — the pinned target may be targets[1..2] after a
// connection-time failover, so every caller MUST attribute rows and evaluate
// pricing against the returned target, never targets[0]. On total failure it
// writes the static 502 and returns ok=false.
func (h *Handler) dialRealtimeUpstream(
	w http.ResponseWriter,
	r *http.Request,
	rec *audit.Record,
	targets []routingcore.RoutingTarget,
	vkID string,
) (*websocket.Conn, routingcore.RoutingTarget, provcore.CallTarget, bool) {
	if h.deps.Resolver == nil {
		h.writeRealtimeUpstreamUnavailable(w, rec)
		return nil, routingcore.RoutingTarget{}, provcore.CallTarget{}, false
	}

	overallCtx, cancel := context.WithTimeout(r.Context(), realtimeDialOverallTimeout)
	defer cancel()

	tried := 0
	for _, target := range targets {
		if tried >= realtimeMaxDialTargets || overallCtx.Err() != nil {
			break
		}
		tried++

		callTarget, resolveErr := h.deps.Resolver.Resolve(overallCtx, target.ProviderID, target.ModelID,
			provtarget.ResolveHints{StickyKey: vkID})
		if resolveErr != nil {
			continue // credential/target resolution failure — try the next target
		}
		conn, ok := h.dialRealtimeTarget(overallCtx, callTarget)
		if ok {
			return conn, target, callTarget, true
		}
	}

	h.writeRealtimeUpstreamUnavailable(w, rec)
	return nil, routingcore.RoutingTarget{}, provcore.CallTarget{}, false
}

// dialRealtimeTarget performs one bounded dial attempt against a resolved
// target. Failures are counted per provider but the error itself is neither
// logged nor surfaced (see the file header).
func (h *Handler) dialRealtimeTarget(ctx context.Context, callTarget provcore.CallTarget) (*websocket.Conn, bool) {
	dialURL, urlErr := realtimeDialURL(callTarget.BaseURL, callTarget.ProviderModelID)
	if urlErr != nil {
		return nil, false
	}
	header, authErr := h.realtimeDialHeader(callTarget)
	if authErr != nil {
		return nil, false
	}

	attemptCtx, cancel := context.WithTimeout(ctx, realtimeDialAttemptTimeout)
	defer cancel()

	// The upstream-shaped client supplies proxy/dial-control-aware transport;
	// the library converts a client Timeout into a handshake timeout and
	// zeroes it for the live session, so it cannot sever the relay later.
	conn, resp, dialErr := websocket.Dial(attemptCtx, dialURL, &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: h.deps.UpstreamClient,
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if dialErr != nil {
		if h.deps.Logger != nil {
			// Provider identity only — the dial error text embeds the dial
			// URL and TLS internals and must not reach the log.
			h.deps.Logger.Warn("realtime upstream dial attempt failed",
				"provider", callTarget.ProviderID)
		}
		return nil, false
	}
	return conn, true
}

// realtimeDialURL derives the provider WebSocket URL from the target's
// BaseURL: scheme-swap https→wss (http→ws for local test providers), path
// /v1/realtime appended to any base path prefix (BaseURL excludes /v1 by the
// OpenAI convention), query built via url.Values — NEVER string
// concatenation, so a provider model id containing &/#/space cannot inject
// into the dial URL.
func realtimeDialURL(baseURL, providerModelID string) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// already a WebSocket base — keep as-is
	default:
		return "", &url.Error{Op: "dial", URL: baseURL, Err: errUnsupportedScheme}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/realtime"
	u.RawQuery = url.Values{"model": {providerModelID}}.Encode()
	return u.String(), nil
}

// errUnsupportedScheme flags a provider BaseURL whose scheme cannot carry a
// WebSocket upgrade.
var errUnsupportedScheme = &unsupportedSchemeError{}

type unsupportedSchemeError struct{}

func (*unsupportedSchemeError) Error() string { return "unsupported base URL scheme" }

// realtimeDialHeader builds the handshake headers from scratch: a throwaway
// request carrying the equivalent HTTPS URL is handed to the resolved
// adapter's ApplyAuth (the same per-provider auth scheme the chat path uses
// — Bearer for OpenAI), and only those adapter-stamped headers go on the
// wire.
func (h *Handler) realtimeDialHeader(callTarget provcore.CallTarget) (http.Header, error) {
	adapter, ok := h.deps.ProviderReg.Get(callTarget.Format)
	if !ok {
		return nil, errUnsupportedScheme
	}
	auth, ok := adapter.(authApplier)
	if !ok {
		return nil, errUnsupportedScheme
	}
	// Scratch request: never sent — it exists only so the adapter's ApplyAuth
	// can stamp provider auth headers, which are then lifted onto the
	// WebSocket dial. context.Background() is therefore correct: there is no
	// round trip for a caller context to cancel.
	scratch, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimRight(callTarget.BaseURL, "/")+"/v1/realtime", nil)
	if err != nil {
		return nil, err
	}
	if err := auth.ApplyAuth(scratch, callTarget); err != nil {
		return nil, err
	}
	return scratch.Header, nil
}

// writeRealtimeUpstreamUnavailable writes the STATIC dial-failure 502. The
// body carries no attempt detail by design — upstream hosts, TLS errors, and
// dial URLs are infrastructure internals.
func (h *Handler) writeRealtimeUpstreamUnavailable(w http.ResponseWriter, rec *audit.Record) {
	h.writeDetailedErr(w, rec, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE",
		"the realtime provider is unavailable",
		"Retry shortly; contact an operator if it persists")
}
