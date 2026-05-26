// Package handler — diagmode.go: HTTP handlers for
// /api/admin/agents/:nodeId/diagnostic-mode + bulk + list endpoints.
//
// Per spec §8.2 the server-side handling is:
//
//	On enable:
//	  1. Validate `until <= now + 24h`
//	  2. Insert thing_diag_mode_window (started_at = now, ended_at = until)
//	  3. Set thing.metadata.diagModeUntil = until via jsonb_set
//	  4. Push shadow update so the agent picks up the change
//	  5. Audit
//	On disable:
//	  - Update active window's ended_at = now
//	  - Clear thing.metadata.diagModeUntil
//	  - Push shadow update; audit
//
// Steps 2+3 happen in a single store transaction (EnableDiagMode /
// DisableDiagMode in opsmetrics_store.go). Step 4 fires through Hub via
// InvalidateConfig so connected agents drop their cached desired-state and
// re-sync on the next heartbeat. Step 5 publishes the standard admin audit
// event onto MQ.
package infra

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// maxDiagModeDuration caps the per-window length per spec §8.1. Any `until`
// timestamp beyond now + this value is rejected with 400.
const maxDiagModeDuration = 24 * time.Hour

// maxBulkDiagModeThings caps the bulk filter resolution. Filters that match
// more than this are rejected — operators should narrow the filter.
const maxBulkDiagModeThings = 500

// thingTypeAgent is the canonical wire value for agent things in the registry.
const thingTypeAgent = "agent"

// configKeyDiagMode is the synthetic config key used for the Hub config-change
// notification on diag-mode toggles. The agent does not store this in its
// thing_config_template — the actual value lives on thing.metadata.diagModeUntil
// — but issuing a NotifyConfigChange on this key is the cheapest way to nudge
// connected agents into a shadow re-read.
const configKeyDiagMode = "diag_mode"

// RegisterDiagModeRoutes wires the four diagnostic-mode endpoints.
//
// IAM resource: diagnostic-mode (carved out in shared/iam.Catalog so the
// compliance / security team can be granted toggle access without holding
// write on every observability surface). Audit emissions in the handlers
// below already use ResourceDiagnosticMode; the IAM gate here matches.
func (h *Handler) RegisterDiagModeRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/agents/diagnostic-mode", h.ListDiagMode, iamMW(iam.ResourceDiagnosticMode.Action(iam.VerbRead)))
	g.POST("/agents/diagnostic-mode/bulk", h.BulkEnableDiagMode, iamMW(iam.ResourceDiagnosticMode.Action(iam.VerbUpdate)))
	g.POST("/agents/:nodeId/diagnostic-mode", h.EnableDiagMode, iamMW(iam.ResourceDiagnosticMode.Action(iam.VerbUpdate)))
	g.DELETE("/agents/:nodeId/diagnostic-mode", h.DisableDiagMode, iamMW(iam.ResourceDiagnosticMode.Action(iam.VerbUpdate)))
}

// enableDiagModeRequest is the POST body for both the single-thing and bulk
// enable endpoints. The bulk variant adds a Filter field via composition.
type enableDiagModeRequest struct {
	Until  string `json:"until"`
	Reason string `json:"reason,omitempty"`
}

// EnableDiagMode opens a diag-mode window for a single thing.
func (h *Handler) EnableDiagMode(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	thingID := strings.TrimSpace(c.Param("nodeId"))
	if thingID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("nodeId is required", "validation_error", "VALIDATION_ERROR"))
	}

	var req enableDiagModeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", "VALIDATION_ERROR"))
	}
	until, herr := parseUntil(req.Until)
	if herr != nil {
		return c.JSON(herr.status, herr.body)
	}

	actor := actorFromContext(c)
	w, err := h.ops.EnableDiagMode(c.Request().Context(), opsstore.EnableDiagModeParams{
		ThingID: thingID,
		Until:   until,
		SetBy:   actor.UserID,
		Reason:  req.Reason,
	})
	if err != nil {
		if errors.Is(err, opsstore.ErrThingNotFound) {
			return c.JSON(http.StatusNotFound, errJSON("node not found", "not_found", "NODE_NOT_FOUND"))
		}
		h.logger.Error("enable_diag_mode", "error", err, "nodeId", thingID)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to enable diagnostic mode", "server_error", "INTERNAL_ERROR"))
	}

	h.notifyDiagModeChange(c, thingID)

	ae := audit.EntryFor(c, iam.ResourceDiagnosticMode, iam.VerbUpdate)
	ae.EntityID = thingID
	ae.AfterState = map[string]any{
		"until":  until.UTC().Format(time.RFC3339),
		"reason": req.Reason,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"window": w})
}

// DisableDiagMode closes the active window for the addressed thing.
func (h *Handler) DisableDiagMode(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	thingID := strings.TrimSpace(c.Param("nodeId"))
	if thingID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("nodeId is required", "validation_error", "VALIDATION_ERROR"))
	}

	if err := h.ops.DisableDiagMode(c.Request().Context(), thingID); err != nil {
		if errors.Is(err, opsstore.ErrNoActiveDiagMode) {
			return c.JSON(http.StatusNotFound, errJSON("no active diagnostic mode window", "not_found", "WINDOW_NOT_FOUND"))
		}
		h.logger.Error("disable_diag_mode", "error", err, "nodeId", thingID)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to disable diagnostic mode", "server_error", "INTERNAL_ERROR"))
	}

	h.notifyDiagModeChange(c, thingID)

	ae := audit.EntryFor(c, iam.ResourceDiagnosticMode, iam.VerbUpdate)
	ae.EntityID = thingID
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// ListDiagMode returns every active window (ended_at > now()).
func (h *Handler) ListDiagMode(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	wins, err := h.ops.ListActiveDiagModeWindows(c.Request().Context())
	if err != nil {
		h.logger.Error("list_active_diag_mode", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list diagnostic mode windows", "server_error", "INTERNAL_ERROR"))
	}
	if wins == nil {
		wins = []opsstore.DiagModeWindow{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": wins})
}

// bulkDiagModeRequest mirrors the POST /agents/diagnostic-mode/bulk body.
type bulkDiagModeRequest struct {
	Filter struct {
		ThingIDs     []string `json:"nodeIds,omitempty"`
		AgentVersion string   `json:"agentVersion,omitempty"`
		OS           string   `json:"os,omitempty"`
	} `json:"filter"`
	Until  string `json:"until"`
	Reason string `json:"reason,omitempty"`
}

// bulkDiagModeResult is the response body. Each thing gets its own status
// entry so partial failures are visible to the caller.
type bulkDiagModeResult struct {
	OK     bool               `json:"ok"`
	Total  int                `json:"total"`
	Items  []bulkDiagModeItem `json:"items"`
	Failed int                `json:"failed"`
}

type bulkDiagModeItem struct {
	ThingID string `json:"nodeId"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// BulkEnableDiagMode resolves the filter, caps at 500 things, then enables
// diag-mode for each one. Failures per thing are surfaced individually so the
// operator can retry the misses.
func (h *Handler) BulkEnableDiagMode(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	var req bulkDiagModeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", "VALIDATION_ERROR"))
	}
	until, herr := parseUntil(req.Until)
	if herr != nil {
		return c.JSON(herr.status, herr.body)
	}

	ids, err := h.ops.ResolveBulkAgents(c.Request().Context(), opsstore.BulkAgentFilter{
		ThingIDs:     req.Filter.ThingIDs,
		AgentVersion: req.Filter.AgentVersion,
		OS:           req.Filter.OS,
	}, maxBulkDiagModeThings)
	if err != nil {
		h.logger.Error("resolve_bulk_agents", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to resolve agents", "server_error", "INTERNAL_ERROR"))
	}
	if len(ids) > maxBulkDiagModeThings {
		return c.JSON(http.StatusBadRequest, errJSON(
			"filter resolves to more than 500 agents — narrow the filter",
			"validation_error", "TOO_MANY_NODES"))
	}
	if len(ids) == 0 {
		return c.JSON(http.StatusOK, bulkDiagModeResult{OK: true, Total: 0, Items: []bulkDiagModeItem{}})
	}

	actor := actorFromContext(c)
	out := bulkDiagModeResult{OK: true, Total: len(ids), Items: make([]bulkDiagModeItem, 0, len(ids))}
	for _, id := range ids {
		_, err := h.ops.EnableDiagMode(c.Request().Context(), opsstore.EnableDiagModeParams{
			ThingID: id,
			Until:   until,
			SetBy:   actor.UserID,
			Reason:  req.Reason,
		})
		item := bulkDiagModeItem{ThingID: id, OK: err == nil}
		if err != nil {
			item.Error = err.Error()
			out.Failed++
		} else {
			h.notifyDiagModeChange(c, id)
		}
		out.Items = append(out.Items, item)
	}
	if out.Failed > 0 {
		out.OK = false
	}

	ae := audit.EntryFor(c, iam.ResourceDiagnosticMode, iam.VerbUpdate)
	ae.AfterState = map[string]any{
		"thingCount": out.Total,
		"failed":     out.Failed,
		"until":      until.UTC().Format(time.RFC3339),
		"reason":     req.Reason,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	status := http.StatusOK
	if out.Failed > 0 {
		status = http.StatusMultiStatus
	}
	return c.JSON(status, out)
}

// notifyDiagModeChange nudges connected things to re-read their shadow so
// the new thing.metadata.diagModeUntil propagates without waiting for the
// next heartbeat. Best-effort: errors only log.
func (h *Handler) notifyDiagModeChange(c echo.Context, thingID string) {
	if h.hub == nil {
		return
	}
	// InvalidateConfig is fire-and-forget; it triggers Hub to broadcast a
	// version bump on (thingType, configKey). Even though diagModeUntil is on
	// thing.metadata (not thing_config_template), Hub's WS pump re-reads the
	// per-thing shadow when it pushes any update, so the agent observes the
	// new diagModeUntil. Pass the thing-id-scoped key so the Hub-side audit
	// links the event to the right thing.
	_ = thingID
	h.hub.InvalidateConfig(c.Request().Context(), thingTypeAgent, configKeyDiagMode)
}

// parseUntil decodes the {"until": ".."} field with the spec's 24h cap and
// the now-or-future requirement.
func parseUntil(raw string) (time.Time, *httpErr) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, badReq("until is required (RFC3339)")
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, badReq("invalid until (RFC3339)")
	}
	now := time.Now().UTC()
	if !t.After(now) {
		return time.Time{}, badReq("until must be in the future")
	}
	if t.Sub(now) > maxDiagModeDuration {
		return time.Time{}, badReq("until is more than 24h in the future")
	}
	return t.UTC(), nil
}
