// Package alerts owns the unified alerting admin surface — alerts /
// alert rules / alert channels — proxied through to Nexus Hub's
// /api/v1/admin/alerts/* endpoints. R6 second domain extracted from
// the flat handler/ package; recipe documented in
// docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
//
// Every CP route here is a thin forwarder: CP records the actor for
// audit + applies IAM gating, Hub owns the durable alert/rule/channel
// state. Hub-bound mutations (POST/PUT/DELETE non-2xx-clean) record
// an admin_audit entry on success.
package alerts

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// HubBaseURLToken is the narrow surface alerts needs from Hub:
// just the base URL and bearer token for proxying. The parent
// handler.HubNotifier interface is wider; this local interface keeps
// alerts/ from depending on the full god-object surface.
type HubBaseURLToken interface {
	BaseURL() string
	Token() string
}

// Deps is the construction-time arg shape. main.go (or admin_routes.go)
// assembles the parent AdminHandler-equivalent value and passes the
// smaller Deps subset the alerts methods need.
type Deps struct {
	Hub    HubBaseURLToken
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler is the per-domain admin handler for /api/admin/alerts*
// endpoints.
type Handler struct {
	hub    HubBaseURLToken
	audit  *audit.Writer
	logger *slog.Logger
}

// New constructs an alerts Handler from its narrow Deps.
func New(d Deps) *Handler {
	return &Handler{hub: d.Hub, audit: d.Audit, logger: d.Logger}
}

// RegisterRoutes registers the unified alerting admin routes. All
// requests are thin-forwarded to Hub's /api/v1/admin/alerts/* endpoints.
// IAM actions follow the catalog taxonomy: alert.read for reads;
// writes split per HTTP semantic — POST/channels = create,
// PUT/DELETE/test/reset = update/delete, /ack and /resolve = acknowledge.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Alert routes
	g.GET("/alerts", h.ListAlerts, iamMW(iam.ResourceAlert.Action(iam.VerbRead)))
	g.GET("/alerts/rules", h.ListAlertRules, iamMW(iam.ResourceAlert.Action(iam.VerbRead)))
	g.GET("/alerts/rules/:id", h.GetAlertRule, iamMW(iam.ResourceAlert.Action(iam.VerbRead)))
	g.PUT("/alerts/rules/:id", h.UpdateAlertRule, iamMW(iam.ResourceAlert.Action(iam.VerbUpdate)))
	g.POST("/alerts/rules/:id/reset", h.ResetAlertRule, iamMW(iam.ResourceAlert.Action(iam.VerbUpdate)))
	g.GET("/alerts/channels", h.ListAlertChannels, iamMW(iam.ResourceAlert.Action(iam.VerbRead)))
	g.POST("/alerts/channels", h.CreateAlertChannel, iamMW(iam.ResourceAlert.Action(iam.VerbCreate)))
	g.GET("/alerts/channels/:id", h.GetAlertChannel, iamMW(iam.ResourceAlert.Action(iam.VerbRead)))
	g.PUT("/alerts/channels/:id", h.UpdateAlertChannel, iamMW(iam.ResourceAlert.Action(iam.VerbUpdate)))
	g.DELETE("/alerts/channels/:id", h.DeleteAlertChannel, iamMW(iam.ResourceAlert.Action(iam.VerbDelete)))
	g.POST("/alerts/channels/:id/test", h.TestAlertChannel, iamMW(iam.ResourceAlert.Action(iam.VerbUpdate)))
	// Parametric alert routes last — static siblings above must register first
	// to prevent Echo matching /rules and /channels against /:id.
	g.GET("/alerts/:id", h.GetAlert, iamMW(iam.ResourceAlert.Action(iam.VerbRead)))
	g.POST("/alerts/:id/ack", h.AckAlert, iamMW(iam.ResourceAlert.Action(iam.VerbAcknowledge)))
	g.POST("/alerts/:id/resolve", h.ResolveAlert, iamMW(iam.ResourceAlert.Action(iam.VerbAcknowledge)))
}

// errJSON builds a canonical JSON error envelope used across admin
// handlers. Local copy of the helper in handler/helpers.go (R6
// runbook §4.2 option 1: per-subpackage copy of small private
// helpers).
func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

var alertsHTTPClient = nexushttp.New(nexushttp.Config{
	Timeout:        10 * time.Second,
	Caller:         "cp-admin-alerts",
	PropagateReqID: true,
})

// hubAlertForward proxies a Hub /api/v1/admin/alerts/* call, streaming the
// response status, headers, and body back to the Echo client unchanged.
// The actor identity (X-Nexus-Actor-User-Id) is injected so Hub can record
// who performed the action.
func (h *Handler) hubAlertForward(c echo.Context, method, hubPath string) error {
	if h.hub == nil || h.hub.BaseURL() == "" {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "server_error", "HUB_NOT_CONFIGURED"))
	}

	var actor *hub.ActorIdentity
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		actor = &hub.ActorIdentity{ID: aa.KeyID}
	}

	var bodyReader io.Reader
	if method == http.MethodPost || method == http.MethodPut {
		bodyReader = c.Request().Body
	}

	// Forward query string verbatim — Hub's ListAlerts accepts the same params
	// the admin UI sends (state, severity, sourceType, ruleId, since, until,
	// offset, limit).
	fullPath := hubPath
	if qs := c.QueryString(); qs != "" {
		fullPath += "?" + qs
	}

	resp, err := alertsHTTPClient.Do(buildAlertRequest(c, method, h.hub.BaseURL()+fullPath, bodyReader, h.hub.Token(), actor))
	if err != nil {
		h.logger.Warn("hub alert proxy: hub unreachable", "method", method, "path", hubPath, "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Hub unreachable", "server_error", "HUB_UNREACHABLE"))
	}
	defer resp.Body.Close() //nolint:errcheck

	h.logger.Debug("hub alert proxy: forwarded", "method", method, "path", hubPath, "status", resp.StatusCode)

	for k, vals := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vals {
			c.Response().Header().Add(k, v)
		}
	}
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = io.Copy(c.Response(), resp.Body)
	return nil
}

// hubAlertForwardMutating forwards to Hub, buffers the response for replay to
// the client, and on 2xx records an admin audit entry (MQ → admin_audit_log).
// Request bodies are not copied into the audit payload (secrets / PII).
//
// All three alert sub-entities (alert, alertRule, alertChannel) are
// represented in the canonical catalog by ResourceAlert; the sub-entity
// (rule vs channel vs alert itself) plus the high-level HTTP method are
// captured in AfterState.{subEntity,hubPath,method} for traceability.
func (h *Handler) hubAlertForwardMutating(
	c echo.Context,
	method, hubPath string,
	verb iam.Verb, subEntity, entityID string,
) error {
	if h.hub == nil || h.hub.BaseURL() == "" {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "server_error", "HUB_NOT_CONFIGURED"))
	}

	var actor *hub.ActorIdentity
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		actor = &hub.ActorIdentity{ID: aa.KeyID}
	}

	var bodyReader io.Reader
	switch method {
	case http.MethodPost, http.MethodPut:
		bodyReader = c.Request().Body
	}

	fullPath := hubPath
	if qs := c.QueryString(); qs != "" {
		fullPath += "?" + qs
	}

	resp, err := alertsHTTPClient.Do(buildAlertRequest(c, method, h.hub.BaseURL()+fullPath, bodyReader, h.hub.Token(), actor))
	if err != nil {
		h.logger.Warn("hub alert proxy: hub unreachable", "method", method, "path", hubPath, "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Hub unreachable", "server_error", "HUB_UNREACHABLE"))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Warn("hub alert proxy: hub body read failed", "method", method, "path", hubPath, "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Hub response read failed", "server_error", "HUB_READ_FAIL"))
	}

	h.logger.Debug("hub alert proxy: forwarded", "method", method, "path", hubPath, "status", resp.StatusCode)

	for k, vals := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vals {
			c.Response().Header().Add(k, v)
		}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Response().Header().Set("Content-Type", ct)
	} else {
		c.Response().Header().Set("Content-Type", "application/json")
	}
	c.Response().WriteHeader(resp.StatusCode)
	// 204 No Content must not carry a body (net/http rejects writes after 204).
	if resp.StatusCode != http.StatusNoContent && len(body) > 0 {
		if _, err := c.Response().Write(body); err != nil {
			return err
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		ae := audit.EntryFor(c, iam.ResourceAlert, verb)
		ae.EntityID = entityID
		ae.AfterState = map[string]any{"hubPath": hubPath, "method": method, "subEntity": subEntity}
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return nil
}

// buildAlertRequest constructs an *http.Request for hubAlertForward.
// Extracted to keep the forward function readable and to allow tests
// to call it directly without an echo.Context.
func buildAlertRequest(c echo.Context, method, fullURL string, body io.Reader, token string, actor *hub.ActorIdentity) *http.Request {
	req, err := http.NewRequestWithContext(c.Request().Context(), method, fullURL, body)
	if err != nil {
		// NewRequestWithContext fails only on programmer error (invalid method or
		// nil context). Panic here surfaces the bug immediately in tests.
		panic("alerts: build request: " + err.Error())
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != nil && actor.ID != "" {
		req.Header.Set("X-Nexus-Actor-User-Id", actor.ID)
	}
	return req
}
