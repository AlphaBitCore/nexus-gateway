package handler

import (
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	cphttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// RuntimeDeps is a type alias to config.RuntimeDeps so call sites that
// already reference handler.RuntimeDeps continue to compile.
type RuntimeDeps = config.RuntimeDeps

// HealthzResponse is a type alias to config.HealthzResponse.
type HealthzResponse = config.HealthzResponse

// ConnectionsResponse is a type alias to config.ConnectionsResponse.
type ConnectionsResponse = config.ConnectionsResponse

// HandleHealthz returns proxy health information.
// Returns 200 when ready, 503 during shutdown.
func HandleHealthz(deps RuntimeDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			return
		}

		redisOK := false
		if deps.RedisChecker != nil {
			redisOK = deps.RedisChecker()
		}

		resp := config.HealthzResponse{
			UptimeSeconds:     time.Since(deps.StartTime).Seconds(),
			ConnectionsActive: deps.ConnManager.ActiveCount(),
			BumpEnabled:       !deps.KillSwitch.IsEngaged(),
			RedisConnected:    redisOK,
		}

		ready := deps.Readiness == nil || deps.Readiness.Load()
		if ready {
			resp.Status = "ok"
			config.WriteJSON(w, http.StatusOK, resp)
		} else {
			resp.Status = "shutting_down"
			config.WriteJSON(w, http.StatusServiceUnavailable, resp)
		}
	}
}

// HandleConnections returns active connection information.
// Supports optional ?targetHost= query parameter for host-based filtering.
// Returns a snapshot of per-connection metadata including source IP, target host,
// and connection start time.
func HandleConnections(deps RuntimeDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			cphttperr.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed", "METHOD_NOT_ALLOWED")
			return
		}

		conns := deps.ConnManager.ActiveConnections()

		// Optional ?targetHost= filter.
		if filter := r.URL.Query().Get("targetHost"); filter != "" {
			filtered := make([]conn.ConnInfo, 0, len(conns))
			for _, c := range conns {
				if c.TargetHost == filter {
					filtered = append(filtered, c)
				}
			}
			conns = filtered
		}

		if conns == nil {
			conns = []conn.ConnInfo{}
		}

		resp := config.ConnectionsResponse{
			Connections: conns,
			Total:       len(conns),
		}
		config.WriteJSON(w, http.StatusOK, resp)
	}
}
