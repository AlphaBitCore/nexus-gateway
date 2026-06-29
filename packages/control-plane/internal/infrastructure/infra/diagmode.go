// Package handler — diagmode.go: HTTP handlers for
// /api/admin/agents/:nodeId/diagnostic-mode + bulk + list endpoints.
//
// Diagnostic mode is delivered to the agent as a per-thing diag_mode
// thing_config_override (state {until}, expires_at=until), written through the
// Hub override API. Hub recomputes thing.desired, bumps desired_ver, writes the
// admin_audit_log row in-tx, and pushes the key — the agent raises its log level
// to debug for the window and self-restores on expiry. The generic OverrideExpiry
// job clears the override when the window ends.
//
//	On enable:  write the diag_mode override, then record an audit-history row
//	            in thing_diag_mode_window (who/when/reason/until) that the list
//	            endpoint reads.
//	On disable: clear the override, then close the active window row.
//
// The handler does NOT write admin_audit_log (Hub's override write already did —
// one row per mutation) and does NOT issue a separate config-change notification
// (the override write already recomputed desired + pushed).
package infra

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// maxDiagModeDuration caps the per-window length. Any `until` timestamp
// beyond now + this value is rejected with 400.
const maxDiagModeDuration = 24 * time.Hour

// minDiagModeDuration is the floor on a window. It matches the diag_mode
// thing_config_override's TTL lower bound ([5m, 30d]) — diag mode is
// delivered as an override with expires_at=until, and Hub rejects an
// expires_at less than 5m out. A sub-5m diagnostic window is also of no
// practical use: there is no time to reproduce an incident.
const minDiagModeDuration = 5 * time.Minute

// maxBulkDiagModeThings caps the bulk filter resolution. Filters that match
// more than this are rejected — operators should narrow the filter.
const maxBulkDiagModeThings = 500

// configKeyDiagMode is the per-thing override key carrying the diagnostic-mode
// window. The handler writes {until} to it via the Hub override API.
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

	// Delivery first: write the diag_mode override on Hub. Hub validates the
	// thing exists, writes the audit row, and pushes the key. Echo any non-2xx
	// (node not found, TTL-window reject) verbatim so the admin sees the reason.
	if status, body, err := h.putDiagModeOverride(c, thingID, until, req.Reason); err != nil {
		h.logger.Error("enable_diag_mode override", "error", err, "nodeId", thingID)
		return c.JSON(http.StatusBadGateway, errJSON("hub unreachable", "server_error", "HUB_UNREACHABLE"))
	} else if status < 200 || status >= 300 {
		return c.JSONBlob(status, body)
	}

	// Audit-history window row for the list endpoint. admin_audit_log was
	// already written by Hub's override path (one row per mutation).
	w, err := h.ops.EnableDiagMode(c.Request().Context(), opsstore.EnableDiagModeParams{
		ThingID: thingID,
		Until:   until,
		SetBy:   actorFromContext(c).UserID,
		Reason:  req.Reason,
	})
	if err != nil {
		if errors.Is(err, opsstore.ErrThingNotFound) {
			return c.JSON(http.StatusNotFound, errJSON("node not found", "not_found", "NODE_NOT_FOUND"))
		}
		h.logger.Error("enable_diag_mode window", "error", err, "nodeId", thingID)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to record diagnostic mode window", "server_error", "INTERNAL_ERROR"))
	}

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

	// Clear the override first (delivery). A Hub 404 means no override exists
	// — tolerate it and still close the window. Other non-2xx surface.
	if status, body, err := h.clearDiagModeOverride(c, thingID); err != nil {
		h.logger.Error("disable_diag_mode override", "error", err, "nodeId", thingID)
		return c.JSON(http.StatusBadGateway, errJSON("hub unreachable", "server_error", "HUB_UNREACHABLE"))
	} else if status >= 300 && status != http.StatusNotFound {
		return c.JSONBlob(status, body)
	}

	if err := h.ops.DisableDiagMode(c.Request().Context(), thingID); err != nil {
		if errors.Is(err, opsstore.ErrNoActiveDiagMode) {
			return c.JSON(http.StatusNotFound, errJSON("no active diagnostic mode window", "not_found", "WINDOW_NOT_FOUND"))
		}
		h.logger.Error("disable_diag_mode window", "error", err, "nodeId", thingID)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to disable diagnostic mode", "server_error", "INTERNAL_ERROR"))
	}

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
		ostatus, _, oerr := h.putDiagModeOverride(c, id, until, req.Reason)
		ok := oerr == nil && ostatus >= 200 && ostatus < 300
		var failErr error
		switch {
		case oerr != nil:
			failErr = oerr
		case !ok:
			failErr = fmt.Errorf("hub status %d", ostatus)
		default:
			if _, werr := h.ops.EnableDiagMode(c.Request().Context(), opsstore.EnableDiagModeParams{
				ThingID: id,
				Until:   until,
				SetBy:   actor.UserID,
				Reason:  req.Reason,
			}); werr != nil {
				ok = false
				failErr = werr
			}
		}
		item := bulkDiagModeItem{ThingID: id, OK: ok}
		if !ok {
			out.Failed++
			if failErr != nil {
				item.Error = failErr.Error()
			}
		}
		out.Items = append(out.Items, item)
	}
	if out.Failed > 0 {
		out.OK = false
	}

	status := http.StatusOK
	if out.Failed > 0 {
		status = http.StatusMultiStatus
	}
	return c.JSON(status, out)
}

// putDiagModeOverride writes (replaces) the per-thing diag_mode override on
// Hub: state {"until": <RFC3339>}, expires_at=until. Returns the Hub HTTP
// status + body so the caller can echo a Hub-side reject (e.g. TTL window)
// verbatim; a transport/build failure is returned as err.
func (h *Handler) putDiagModeOverride(c echo.Context, thingID string, until time.Time, reason string) (int, []byte, error) {
	stateJSON, _ := json.Marshal(map[string]string{"until": until.UTC().Format(time.RFC3339)})
	body, _ := json.Marshal(map[string]any{
		"state":     json.RawMessage(stateJSON),
		"expiresAt": until.UTC().Format(time.RFC3339),
		"reason":    reason,
	})
	return h.doDiagModeOverride(c, http.MethodPut, thingID, body)
}

// clearDiagModeOverride deletes the per-thing diag_mode override on Hub.
func (h *Handler) clearDiagModeOverride(c echo.Context, thingID string) (int, []byte, error) {
	return h.doDiagModeOverride(c, http.MethodDelete, thingID, nil)
}

// doDiagModeOverride issues the diag_mode override request to Hub, attributing
// the live admin via X-Nexus-Actor-Id so the Hub-side audit row carries the
// right identity. err is non-nil only on a not-configured / build / transport
// failure; a Hub 4xx/5xx is returned as (status, body, nil) for the caller to
// surface verbatim.
func (h *Handler) doDiagModeOverride(c echo.Context, method, thingID string, body []byte) (int, []byte, error) {
	if h.hub == nil || h.hub.BaseURL() == "" {
		return 0, nil, errors.New("hub not configured")
	}
	u := h.hub.BaseURL() + "/api/hub/things/" + url.PathEscape(thingID) + "/overrides/" + configKeyDiagMode
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.Request().Context(), method, u, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.hub.Token())
	if actor := actorFromContext(c); actor.UserID != "" {
		req.Header.Set("X-Nexus-Actor-Id", actor.UserID)
	}
	resp, err := h.hubProxyClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb, nil
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
	if t.Sub(now) < minDiagModeDuration {
		return time.Time{}, badReq("until must be at least 5m in the future")
	}
	if t.Sub(now) > maxDiagModeDuration {
		return time.Time{}, badReq("until is more than 24h in the future")
	}
	return t.UTC(), nil
}
