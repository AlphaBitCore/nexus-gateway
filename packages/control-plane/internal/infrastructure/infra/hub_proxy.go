package infra

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterNodeRoutes registers BFF reverse-proxy routes to the Nexus Hub runtime
// API using product-facing terminology (node / config-sync / out-of-sync).
// Hub responses are passed through hubadapter so internal terms (thing / shadow
// / drift) never leak to admin clients.
//
// The POST /nodes/:id/resync route is owned by AdminResyncNode (registered in
// RegisterAdminNodeOverridesRoutes) — it supports both empty-body (whole-Thing
// replay) and {"configKey": "..."} (single-key) forms, plus thing-type RBAC.
func (h *Handler) RegisterNodeRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Nodes — gate on the `node` carve-out (audit #21 fix). Previously
	// these routes used the overloaded `settings` resource, which
	// granted node visibility to anyone with `settings.read` and made
	// the carved-out `node.read` / `node.force-resync` verbs in the
	// catalog inert.
	g.GET("/nodes", h.NodesList, iamMW(iam.ResourceNode.Action(iam.VerbRead)))
	g.GET("/nodes/:id", h.NodesGet, iamMW(iam.ResourceNode.Action(iam.VerbRead)))
	g.GET("/nodes/:id/device-assignments", h.GetNodeDeviceAssignments, iamMW(iam.ResourceNode.Action(iam.VerbRead)))

	// Config Sync
	g.GET("/config-sync/out-of-sync", h.ConfigSyncOutOfSync, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/config-sync/history", h.ConfigSyncHistory, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/config-sync/catalog", h.ConfigSyncCatalog, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.POST("/config-sync/update", h.ConfigSyncUpdate, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))

	// Scheduled Jobs (owned by Nexus Hub; CP proxies admin reads + writes).
	g.GET("/jobs", h.JobsList, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/jobs/:id", h.JobsGet, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/jobs/:id/runs", h.JobsListRuns, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.PUT("/jobs/:id", h.JobsUpdate, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
	g.POST("/jobs/:id/trigger", h.JobsTrigger, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))

	// Enrollment Tokens (naming already product-neutral)
	g.GET("/enrollment/tokens", h.EnrollmentListTokens, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.POST("/enrollment/token", h.EnrollmentCreateToken, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
}

// defaultHubHTTPClient is the fallback used when AdminHandler.HubProxyClient
// is nil (e.g. tests that build AdminHandler by hand). Production wiring in
// cmd/control-plane/main.go injects a client built from
// cfg.HTTPClients.HubProxy.TimeoutSec.
var defaultHubHTTPClient = nexushttp.New(nexushttp.Config{
	Timeout:        10 * time.Second,
	Caller:         "cp-admin-hub-proxy",
	PropagateReqID: true,
})

func (h *Handler) hubProxyClient() *http.Client {
	if h.hubProxyClientRef != nil {
		return h.hubProxyClientRef
	}
	return defaultHubHTTPClient
}

// hubForward proxies a Hub HTTP call, passing the response body through an
// optional JSON rename function before returning to the admin client.
func (h *Handler) hubForward(
	c echo.Context,
	method, hubPath string,
	rename func([]byte) ([]byte, error),
) error {
	if h.hub == nil || h.hub.BaseURL() == "" {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "server_error", "HUB_NOT_CONFIGURED"))
	}
	hubURL := h.hub.BaseURL() + hubPath

	var bodyReader io.Reader
	if method == http.MethodPost || method == http.MethodPut {
		bodyReader = c.Request().Body
	}

	req, err := http.NewRequestWithContext(c.Request().Context(), method, hubURL, bodyReader)
	if err != nil {
		h.logger.Error("hub proxy: failed to create request", "method", method, "path", hubPath, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create proxy request", "server_error", ""))
	}
	req.Header.Set("Authorization", "Bearer "+h.hub.Token())
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		req.Header.Set("X-Nexus-Actor-Id", aa.KeyID)
		req.Header.Set("X-Nexus-Actor-Name", aa.KeyName)
	}
	req.URL.RawQuery = c.QueryString()

	start := time.Now()
	resp, err := h.hubProxyClient().Do(req)
	if err != nil {
		h.logger.Warn("hub proxy: hub unreachable", "method", method, "path", hubPath, "duration", time.Since(start), "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Hub unreachable", "server_error", "HUB_UNREACHABLE"))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.JSON(http.StatusBadGateway, errJSON("Hub response read failed", "server_error", "HUB_READ_FAIL"))
	}

	if rename != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && len(body) > 0 {
		renamed, rerr := rename(body)
		if rerr != nil {
			h.logger.Error("hub proxy: rename failed", "path", hubPath, "error", rerr)
			return c.JSON(http.StatusBadGateway, errJSON("Hub adapter failed", "server_error", "HUB_ADAPT_FAIL"))
		}
		body = renamed
	}

	h.logger.Debug("hub proxy: forwarded", "method", method, "path", hubPath, "status", resp.StatusCode, "duration", time.Since(start))

	for k, vals := range resp.Header {
		if k == "Content-Length" { // recompute after rename
			continue
		}
		for _, v := range vals {
			c.Response().Header().Add(k, v)
		}
	}
	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(body)
	return nil
}

func (h *Handler) NodesList(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/things", hub.RenameThingsList)
}

func (h *Handler) NodesGet(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/things/"+url.PathEscape(c.Param("id")), hub.RenameNode)
}

func (h *Handler) ConfigSyncOutOfSync(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/drift", hub.RenameDriftResponse)
}

func (h *Handler) ConfigSyncHistory(c echo.Context) error {
	// Rewrite the product-facing `nodeType` query param into Hub's
	// internal `thingType` before forwarding. hubForward passes the
	// query string through verbatim, so without this step the filter
	// silently drops on the Hub side and the admin UI returns the
	// unfiltered history despite the Select selection. Other params
	// (configKey, actorId, from, to, page, pageSize) match on both
	// sides and pass through unchanged.
	q := c.Request().URL.Query()
	if v := q.Get("nodeType"); v != "" && q.Get("thingType") == "" {
		q.Set("thingType", v)
		q.Del("nodeType")
		c.Request().URL.RawQuery = q.Encode()
	}
	return h.hubForward(c, http.MethodGet, "/api/hub/config/history", hub.RenameConfigHistoryResponse)
}

// ConfigSyncCatalog proxies Hub's (thingType, configKey) catalog so the
// admin Config Sync history filter can populate its Type / Config Key
// selects from live template data. Response uses product-facing `nodeType`.
func (h *Handler) ConfigSyncCatalog(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/config/catalog", hub.RenameConfigCatalogResponse)
}

// ConfigSyncUpdate proxies the admin "push a config update" action (used by
// the Kill Switch page and any future direct config editors) to Hub's
// POST /api/hub/config/update. CP's surface uses product-neutral `nodeType`
// while Hub's contract requires internal `thingType`; forwarding the admin
// body unchanged would trip Hub's 400 "thingType and configKey are required"
// validator. Validate here, re-serialize a clean Hub-contract body, then
// rename the response counters on the way out.
func (h *Handler) ConfigSyncUpdate(c echo.Context) error {
	var req struct {
		NodeType  string          `json:"nodeType"`
		ConfigKey string          `json:"configKey"`
		State     json.RawMessage `json:"state"`
		Action    string          `json:"action"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid body", "validation_error", ""))
	}
	if req.NodeType == "" || req.ConfigKey == "" {
		return c.JSON(http.StatusBadRequest, errJSON("nodeType and configKey are required", "validation_error", ""))
	}

	hubPayload := map[string]any{
		"thingType": req.NodeType,
		"configKey": req.ConfigKey,
	}
	if len(req.State) > 0 {
		hubPayload["state"] = req.State
	}
	if req.Action != "" {
		hubPayload["action"] = req.Action
	}
	hubBody, err := json.Marshal(hubPayload)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("failed to encode hub request", "server_error", ""))
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(hubBody))
	c.Request().ContentLength = int64(len(hubBody))
	c.Request().Header.Set("Content-Type", "application/json")

	if err := h.hubForward(c, http.MethodPost, "/api/hub/config/update", hub.RenameConfigUpdateResponse); err != nil {
		return err
	}
	if c.Response().Status >= 200 && c.Response().Status < 300 {
		ae := audit.EntryFor(c, iam.ResourceNode, iam.VerbUpdate)
		if req.Action != "" {
			ae.Action = req.Action
		}
		ae.EntityID = req.ConfigKey
		ae.AfterState = map[string]any{"nodeType": req.NodeType, "configKey": req.ConfigKey}
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return nil
}

func (h *Handler) JobsList(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/jobs", nil)
}

func (h *Handler) JobsGet(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/jobs/"+url.PathEscape(c.Param("id")), nil)
}

func (h *Handler) JobsListRuns(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/jobs/"+url.PathEscape(c.Param("id"))+"/runs", nil)
}

// JobsUpdate proxies PUT /api/hub/jobs/:id. Hub currently accepts only the
// `enabled` toggle; CP buffers the request body so we can snapshot the
// intended state into the audit log without starving the proxy read.
func (h *Handler) JobsUpdate(c echo.Context) error {
	id := c.Param("id")
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid body", "validation_error", ""))
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(body))

	if err := h.hubForward(c, http.MethodPut, "/api/hub/jobs/"+url.PathEscape(id), nil); err != nil {
		return err
	}
	if c.Response().Status >= 200 && c.Response().Status < 300 {
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		ae := audit.EntryFor(c, iam.ResourceNode, iam.VerbUpdate)
		ae.EntityID = id
		ae.AfterState = parsed
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return nil
}

// JobsTrigger proxies POST /api/hub/jobs/:id/trigger and, on 2xx, writes an
// audit entry so the admin ledger records who forced the run.
func (h *Handler) JobsTrigger(c echo.Context) error {
	id := c.Param("id")
	if err := h.hubForward(c, http.MethodPost, "/api/hub/jobs/"+url.PathEscape(id)+"/trigger", nil); err != nil {
		return err
	}
	if c.Response().Status >= 200 && c.Response().Status < 300 {
		ae := audit.EntryFor(c, iam.ResourceNode, iam.VerbUpdate)
		ae.EntityID = id
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return nil
}

func (h *Handler) EnrollmentListTokens(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/enrollment/tokens", nil)
}

func (h *Handler) EnrollmentCreateToken(c echo.Context) error {
	if err := h.hubForward(c, http.MethodPost, "/api/hub/enrollment/token", nil); err != nil {
		return err
	}
	if c.Response().Status >= 200 && c.Response().Status < 300 {
		ae := audit.EntryFor(c, iam.ResourceNode, iam.VerbCreate)
		// Do not record token material from Hub; summary only.
		ae.AfterState = map[string]any{"issued": true}
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return nil
}

// GetNodeDeviceAssignments returns the login / device-assignment history for
// a node (thing). The node ID is a thing ID, which is also the device ID in
// DeviceAssignment."deviceId". Returns active + historical assignments ordered
// by most recent first.
func (h *Handler) GetNodeDeviceAssignments(c echo.Context) error {
	id := c.Param("id")
	assignments, err := h.fleet.ListDeviceAssignments(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get node device assignments", "nodeId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch device assignments", "server_error", ""))
	}
	if assignments == nil {
		assignments = []fleetstore.DeviceAssignmentDetail{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": assignments, "total": len(assignments)})
}
