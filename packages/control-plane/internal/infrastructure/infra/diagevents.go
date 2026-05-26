// Package handler — diagevents.go: HTTP handlers for /api/admin/diag-events/*.
// All three endpoints are read-only and gated by admin:ReadObservability.
package infra

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterDiagEventsRoutes wires the three /api/admin/diag-events endpoints.
func (h *Handler) RegisterDiagEventsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/diag-events", h.DiagEventsList, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.GET("/diag-events/groups", h.DiagEventsGroups, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.GET("/diag-events/crash-cohorts", h.DiagEventsCrashCohorts, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
}

// DiagEventsList returns a newest-first paginated list of diagnostic events
// from thing_diag_event with optional nodeId / level / source / message
// search filters and a (occurred_at, id) cursor.
func (h *Handler) DiagEventsList(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	p := opsstore.DiagEventListParams{
		ThingID:   strings.TrimSpace(c.QueryParam("nodeId")),
		Level:     strings.TrimSpace(c.QueryParam("level")),
		EventType: strings.TrimSpace(c.QueryParam("eventType")),
		Source:    strings.TrimSpace(c.QueryParam("source")),
		Search:    strings.TrimSpace(c.QueryParam("q")),
		Cursor:    strings.TrimSpace(c.QueryParam("cursor")),
	}
	if v := strings.TrimSpace(c.QueryParam("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if v := strings.TrimSpace(c.QueryParam("from")); v != "" {
		t, ok := parseRFC3339Flexible(v)
		if !ok {
			return c.JSON(http.StatusBadRequest, errJSON("invalid from (RFC3339)", "validation_error", "VALIDATION_ERROR"))
		}
		p.From = &t
	}
	if v := strings.TrimSpace(c.QueryParam("to")); v != "" {
		t, ok := parseRFC3339Flexible(v)
		if !ok {
			return c.JSON(http.StatusBadRequest, errJSON("invalid to (RFC3339)", "validation_error", "VALIDATION_ERROR"))
		}
		p.To = &t
	}
	// Validate level when provided so we don't silently match nothing on a typo.
	if p.Level != "" {
		switch strings.ToLower(p.Level) {
		case "debug", "info", "warn", "error", "fatal":
			p.Level = strings.ToLower(p.Level)
		default:
			return c.JSON(http.StatusBadRequest, errJSON("invalid level (debug|info|warn|error|fatal)", "validation_error", "VALIDATION_ERROR"))
		}
	}

	res, err := h.ops.ListDiagEvents(c.Request().Context(), p)
	if err != nil {
		// Cursor decode errors surface as 400 rather than 500 because they
		// indicate a bad client cursor, not a server fault.
		if strings.Contains(err.Error(), "decode diag cursor") {
			return c.JSON(http.StatusBadRequest, errJSON("invalid cursor", "validation_error", "VALIDATION_ERROR"))
		}
		h.logger.Error("list_diag_events", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list diag events", "server_error", "INTERNAL_ERROR"))
	}
	if res.Items == nil {
		res.Items = []opsstore.DiagEvent{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"data":       res.Items,
		"nextCursor": res.NextCursor,
	})
}

// DiagEventsGroups returns the top-100 message_hash buckets in [from, to)
// with affected-nodes and total occurrence counts.
func (h *Handler) DiagEventsGroups(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	from, to, herr := parseFromTo(c)
	if herr != nil {
		return c.JSON(herr.status, herr.body)
	}
	thingType := strings.TrimSpace(c.QueryParam("nodeType"))
	eventType := strings.TrimSpace(c.QueryParam("eventType"))

	groups, err := h.ops.ListDiagGroups(c.Request().Context(), opsstore.DiagGroupsParams{
		From:      from,
		To:        to,
		ThingType: thingType,
		EventType: eventType,
	})
	if err != nil {
		h.logger.Error("list_diag_groups", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list diag groups", "server_error", "INTERNAL_ERROR"))
	}
	if groups == nil {
		groups = []opsstore.DiagGroup{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": groups})
}

// DiagEventsCrashCohorts returns FATAL/crash events grouped by
// (agent_version, os, os_version) for the time window. Used by the Crash
// Reports page.
func (h *Handler) DiagEventsCrashCohorts(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	from, to, herr := parseFromTo(c)
	if herr != nil {
		return c.JSON(herr.status, herr.body)
	}
	cohorts, err := h.ops.ListCrashCohorts(c.Request().Context(), from, to)
	if err != nil {
		h.logger.Error("list_crash_cohorts", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list crash cohorts", "server_error", "INTERNAL_ERROR"))
	}
	if cohorts == nil {
		cohorts = []opsstore.CrashCohort{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": cohorts})
}
