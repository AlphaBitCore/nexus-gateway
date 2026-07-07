package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// ConnectionStage returns an HTTP middleware that evaluates connection-stage
// hooks before the wrapped handler runs. Reject decisions short-circuit with
// 403 Forbidden carrying the hook's Reason in the body. On resolver errors
// or pipeline build errors the middleware logs a warning and falls through
// (fail-open) — the connection-stage pipeline is always optional and an
// infrastructure glitch must not take down every request.
//
// Passing a nil resolverSupplier disables the middleware entirely (no-op
// wrapper). This is the default in tests / local runs that don't have a
// HookConfigCache configured.
//
// The supplier callback is invoked per-request so the middleware keeps
// working if a future cache implementation ever hands out per-generation
// resolver instances. With pipeline.HookConfigCache the call is a pure
// pointer read (the resolver is swapped in place by push/backstop reloads),
// so the indirection costs nothing on the hot path.
func ConnectionStage(
	resolverSupplier func(ctx context.Context) (*pipeline.PolicyResolver, error),
	perHookTimeout, totalTimeout time.Duration,
	ingress string,
	logger *slog.Logger,
) func(http.Handler) http.Handler {
	if resolverSupplier == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resolver, err := resolverSupplier(r.Context())
			if err != nil {
				logger.Warn("connection-stage resolver error; failing open", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			if resolver == nil {
				next.ServeHTTP(w, r)
				return
			}

			// Connection-stage: endpoint type is not yet known (CONNECT predates
			// the TLS handshake and request parsing). Pass "" and nil modalities
			// to preserve existing behavior — all hooks that declare
			// SupportsEndpoint("") == true will be included.
			// strictFailClosed=false here: the connection-stage error handler below
			// deliberately fails open (CONNECT predates request parsing; refusing at
			// connect time is the wrong layer), so passing true would be a misleading
			// no-op. The request/response stages are the real fail-closed enforcement
			// surface. Build errors here skip the connection-stage pipeline.
			pipe, err := resolver.BuildPipeline(
				"connection", ingress,
				"", nil,
				perHookTimeout, totalTimeout, false, false, logger,
			)
			if err != nil {
				logger.Warn("connection-stage pipeline build error; failing open", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			if pipe == nil {
				// No connection-stage hooks configured for this ingress — fast path.
				next.ServeHTTP(w, r)
				return
			}

			input := &hookcore.HookInput{
				RequestID:   r.Header.Get("X-Nexus-Request-Id"),
				Stage:       "connection",
				SourceIP:    ClientIP(r),
				TargetHost:  r.Host,
				Path:        r.URL.Path,
				Method:      r.Method,
				IngressType: ingress,
				TLS:         tlsInfoFromRequest(r),
			}

			res := pipe.Execute(r.Context(), input)
			if res != nil && res.Decision == hookcore.RejectHard {
				reason := res.Reason
				if reason == "" {
					reason = "connection blocked by compliance policy"
				}
				http.Error(w, reason, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP extracts the originating client IP from the request. The order
// follows the conventions used by proxy-fronted HTTP services: prefer the
// first entry of X-Forwarded-For, then X-Real-IP, and finally fall back to
// RemoteAddr with the port stripped.
//
// Exported because the handler package injects the same value into
// request/response-stage HookInput so ip-access-filter and friends see a
// consistent source IP across stages.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.IndexByte(xff, ','); comma > 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return rip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tlsInfoFromRequest returns a *hookcore.TLSInfo populated from r.TLS when
// available. Returns nil for plain HTTP or when the ingress terminated TLS
// upstream (no r.TLS on the handler's side).
func tlsInfoFromRequest(r *http.Request) *hookcore.TLSInfo {
	if r.TLS == nil {
		return nil
	}
	// ClientCertFingerprint is left empty here; Go's net/http in its default
	// configuration does not request a client certificate. A future mTLS
	// terminator can enrich this by wrapping the middleware or by setting a
	// request header / context value consumed here.
	return &hookcore.TLSInfo{SNI: r.TLS.ServerName}
}
