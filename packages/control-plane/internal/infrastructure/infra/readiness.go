package infra

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterReadinessRoutes mounts the readiness check + instance list routes.
// ReadinessCheck is also registered at the public /ready path in
// cmd/control-plane/main.go (without auth) so k8s probes / load-balancer
// health checks reach it without an admin token.
func (h *Handler) RegisterReadinessRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/instances", h.ListInstances, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

// ReadinessCheck returns health status of the database and Hub.
// Exposed on the public /ready route (no auth) via RegisterPublicReadinessRoute.
func (h *Handler) ReadinessCheck(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	allOk := true

	// Database check — use the test seam (dbPingFn) when set, otherwise
	// fall back to the concrete *pgxpool.Pool. In production Pool is always
	// non-nil; in tests dbPingFn is injected so the pool is not needed.
	pingFn := h.dbPingFn
	if pingFn == nil && h.db != nil && h.db.Pool != nil {
		pingFn = h.db.Pool.Ping
	}
	if pingFn != nil {
		if err := pingFn(ctx); err != nil {
			checks["database"] = "unhealthy"
			allOk = false
		} else {
			checks["database"] = "ok"
		}
	}

	// Hub check
	if h.hub == nil || h.hub.BaseURL() == "" {
		checks["hub"] = "not_configured"
	} else {
		hubReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.hub.BaseURL()+"/healthz", nil)
		if hubReq != nil {
			hubResp, hubErr := nexushttp.New(nexushttp.Config{
				Timeout:        2 * time.Second,
				Caller:         "cp-admin-infra-hub-healthz",
				PropagateReqID: true,
			}).Do(hubReq)
			if hubErr != nil {
				checks["hub"] = "unreachable"
				allOk = false
			} else {
				_ = hubResp.Body.Close()
				if hubResp.StatusCode == http.StatusOK {
					checks["hub"] = "ok"
				} else {
					checks["hub"] = "unhealthy"
					allOk = false
				}
			}
		}
	}

	status := "ready"
	httpStatus := http.StatusOK
	if !allOk {
		status = "not_ready"
		httpStatus = http.StatusServiceUnavailable
	}

	return c.JSON(httpStatus, map[string]any{
		"status": status,
		"checks": checks,
	})
}

// ListInstances returns all registered service instances with per-service summaries.
func (h *Handler) ListInstances(c echo.Context) error {
	instances, err := h.db.ListThingServices(c.Request().Context())
	if err != nil {
		h.logger.Error("list instances", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list instances", "server_error", ""))
	}

	summaries, err := h.db.GetServiceSummaries(c.Request().Context())
	if err != nil {
		h.logger.Error("service summaries", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get summaries", "server_error", ""))
	}

	servicesMap := map[string]any{}
	for _, s := range summaries {
		servicesMap[s.Service] = s
	}

	totalCount := 0
	for _, s := range summaries {
		totalCount += s.Total
	}

	return c.JSON(http.StatusOK, map[string]any{
		"instances": instances,
		"count":     totalCount,
		"services":  servicesMap,
	})
}
