package traffic

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func parseAdminAuditParams(c echo.Context) trafficstore.AdminAuditLogListParams {
	pg := parsePagination(c)
	params := trafficstore.AdminAuditLogListParams{
		ActorID:        c.QueryParam("actorId"),
		ActorLabel:     c.QueryParam("actorLabel"),
		ActorRole:      c.QueryParam("actorRole"),
		Action:         c.QueryParam("action"),
		EntityType:     c.QueryParam("entityType"),
		NexusRequestID: c.QueryParam("nexusRequestId"),
		Limit:          pg.Limit,
		Offset:         pg.Offset,
	}
	if v := c.QueryParam("startTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.StartTime = &t
		}
	}
	if v := c.QueryParam("endTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.EndTime = &t
		}
	}
	return params
}

func (h *Handler) ListAdminAuditLogs(c echo.Context) error {
	params := parseAdminAuditParams(c)
	data, total, err := h.traffic.ListAdminAuditLogs(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list admin audit logs", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total, "limit": params.Limit, "offset": params.Offset})
}

// ListMyAdminAuditLogs returns admin audit logs for the current user only.
func (h *Handler) ListMyAdminAuditLogs(c echo.Context) error {
	params := parseAdminAuditParams(c)
	aa := middleware.AdminAuthFromContext(c)
	if aa != nil {
		params.ActorID = aa.KeyID
	}
	data, total, err := h.traffic.ListAdminAuditLogs(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list my admin audit logs", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total, "limit": params.Limit, "offset": params.Offset})
}

func (h *Handler) ExportAdminAuditLogs(c echo.Context) error {
	params := parseAdminAuditParams(c)
	const maxExport = 10_000

	entries, err := h.traffic.ExportAdminAuditLogs(c.Request().Context(), params, maxExport)
	if err != nil {
		h.logger.Error("export admin audit logs", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceAuditLog, iam.VerbExport)
	ae.AfterState = map[string]any{"recordCount": len(entries)}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{
		"exportedAt": time.Now().Format(time.RFC3339),
		"truncated":  len(entries) >= maxExport,
		"entries":    entries,
	})
}
