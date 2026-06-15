package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/breakglass"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
	cphttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// Server is the Go runtime API HTTP server.
//
// This server does not expose audit log or traffic_event query APIs — those
// are served by the Control Plane admin API against PostgreSQL. Only process-local
// probes (/healthz, /connections, /metrics) and the /runtime/* shadow surface
// live here.
//
// The shape is now shadow-driven: every mutating surface the proxy exposes
// lives under PUT /runtime/config/{key} (the break-glass path). Legacy
// routes (/killswitch*, /exemptions*, /alerts/*) have been deleted as part
// of the runtimeapi-slimming refactor; the TestServer_Deleted404 suite in
// deleted_routes_test.go is the compliance gate.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
	bg         *breakglass.State
}

// NewServer creates the runtime API server with all routes registered.
// Route authentication summary:
//   - /healthz                               — no auth
//   - /metrics                               — Require
//   - /connections                           — Require
//   - GET  /runtime/config                   — Require
//   - GET  /runtime/config/{key}             — Require
//   - PUT  /runtime/config/{key}             — Require (break-glass)
//   - GET  /runtime/sync-status              — Require
//   - GET  /runtime/health                   — Require
func NewServer(addr string, deps config.RuntimeDeps, tokenAuth *auth.TokenAuth) *Server {
	mux := http.NewServeMux()

	// The runtime API is guarded by TokenAuth (static env bearer token
	// COMPLIANCE_PROXY_API_TOKEN). These are operator-facing surfaces; the
	// platform's primary config path flows through Hub shadow sync, so the
	// break-glass PUT exists only as a fallback when Hub is unreachable.
	// If this surface is ever migrated to auth-server JWT, swap Require
	// for jwtverifier.Middleware.

	// /healthz — no auth required (used by probes).
	mux.Handle("/healthz", handler.HandleHealthz(deps))

	// /metrics — standard auth, delegate to Prometheus handler.
	mux.Handle("/metrics", tokenAuth.Require(promhttp.Handler()))

	// /connections — standard auth.
	mux.Handle("/connections", tokenAuth.Require(handler.HandleConnections(deps)))

	// --- /runtime/* shadow-aligned surface ---
	//
	// /runtime/config          GET composite snapshot
	// /runtime/config/{key}    GET per-key snapshot
	//                          PUT break-glass write for killswitch and
	//                              exemptions only. Other keys return
	//                              400 via the handler's applyBreakGlassLocal
	//                              switch — the route still dispatches to PUT
	//                              so operators get a clear error instead of 404.
	// /runtime/sync-status     GET shadow cursor
	// /runtime/health          GET per-subsystem liveness
	mux.Handle("/runtime/config", tokenAuth.Require(config.HandleRuntimeConfig(deps)))

	// deps.Thingclient satisfies BreakGlassVersionSource via its DesiredVer /
	// ReportedVer accessors; passing it here is what makes the break-glass
	// newVer Hub-aware per spec §5.2 step 3.
	bg := breakglass.NewBreakGlassState(deps.DataDir, deps.BreakGlassReporter, deps.Thingclient)
	for _, key := range config.KnownRuntimeConfigKeys {
		k := key // capture for closure
		getH := tokenAuth.Require(config.HandleRuntimeConfigKey(deps, k))
		putH := tokenAuth.Require(breakglass.HandleBreakGlassPut(deps, k, bg))
		mux.Handle("/runtime/config/"+k, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				getH.ServeHTTP(w, r)
			case http.MethodPut:
				putH.ServeHTTP(w, r)
			default:
				cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			}
		}))
	}

	mux.Handle("/runtime/sync-status", tokenAuth.Require(config.HandleRuntimeSyncStatus(deps)))
	mux.Handle("/runtime/health", tokenAuth.Require(config.HandleRuntimeHealth(deps, deps.Health)))

	// Wrap the mux with HTTP access log middleware.
	h := logging.HTTPRequestLogger(deps.Logger)(mux)

	return &Server{
		httpServer: &http.Server{
			Addr:    addr,
			Handler: h,
		},
		logger: deps.Logger,
		bg:     bg,
	}
}

// ReplayPending attempts to redeliver any buffered break-glass report. Safe
// to call at any time (the internal bgState handles the "nothing pending"
// case). Callers typically wire it to thingclient.OnReconnect and to a
// periodic ticker so a spool that lingers through a flap eventually drains.
//
// Returns (true, nil) on a successful drain, (false, nil) when nothing was
// pending, and (false, err) when the delivery failed — the caller should
// log the error and retry later.
func (s *Server) ReplayPending(ctx context.Context) (bool, error) {
	if s == nil || s.bg == nil {
		return false, nil
	}
	return s.bg.ReplayPending(ctx)
}

// Start begins listening. It blocks until the context is cancelled or the
// server encounters a fatal error.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("runtime API server starting", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("runtime API server stopping (context cancelled)")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("runtime API server shutting down")
	return s.httpServer.Shutdown(ctx)
}
