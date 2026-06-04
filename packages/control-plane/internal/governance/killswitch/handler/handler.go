// Package killswitch owns the Control Plane admin API for the data-plane
// killswitch. Reads CP's thing_config_template + config_change_event tables
// directly; writes route through Hub which owns shadow UPSERT + audit +
// WebSocket broadcast.
package killswitch

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	cfginterception "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// Thing types that receive the kill-switch fan-out. The kill switch is
// the platform's emergency-grade brake: engaging it MUST stop TLS
// bumping on every Thing that performs interception (compliance-proxy
// AND agent), otherwise the operator's "stop the bleed" intent is
// silently honored on only one half of the fleet.
const (
	thingTypeComplianceProxy = "compliance-proxy"
	thingTypeAgent           = "agent"
)

// killswitchFanoutTypes is the canonical fan-out order. Compliance-proxy
// is listed first because it is the primary kill-switch consumer (every
// browser-side AI call routes through it); agent follows so a partial
// Hub failure on the agent leg still gets the compliance-proxy fleet
// into a safe state.
var killswitchFanoutTypes = []string{thingTypeComplianceProxy, thingTypeAgent}

// configKeyKillswitch is the shadow config key owning the global
// enable/disable flag for the data planes.
const configKeyKillswitch = "killswitch"

// HubConfigChanger is the narrow Hub surface killswitch/ needs:
// NotifyConfigChange routes through Hub which UPSERTs the template,
// inserts the audit-log row, and broadcasts via WebSocket. Same shape
// as cache.HubConfigChanger.
type HubConfigChanger interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Hub    HubConfigChanger // may be nil — POST returns 503 in that case
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler is the per-domain admin handler for
// /api/admin/compliance/killswitch.
type Handler struct {
	hub    HubConfigChanger
	audit  *audit.Writer
	logger *slog.Logger
}

// New constructs a killswitch Handler from its narrow Deps.
func New(d Deps) *Handler {
	return &Handler{
		hub:    d.Hub,
		audit:  d.Audit,
		logger: d.Logger,
	}
}

// RegisterRoutes mounts the single canonical kill-switch toggle
// endpoint under the caller-supplied admin group. The dedicated route
// (vs the generic /api/admin/config-sync/update) owns three things the
// generic surface cannot: (a) fan-out across both compliance-proxy and
// agent template rows in one call, (b) a dedicated `kill-switch.toggle`
// admin-audit action label the SIEM bridge keys off, and (c) the narrow
// `admin:kill-switch.toggle` IAM verb. Read-side data (current desired
// state per node, history events) lives on the generic config-sync
// surface so this handler is write-only.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/compliance/killswitch", h.Post, iamMW(iam.ResourceKillSwitch.Action(iam.VerbToggle)))
}

// Post toggles the killswitch by notifying Hub. Hub owns the UPSERT
// of thing_config_template, the audit-log insert, and the WebSocket
// broadcast — CP never writes the template directly.
func (h *Handler) Post(c echo.Context) error {
	var req struct {
		Engaged *bool `json:"engaged"`
	}
	if err := c.Bind(&req); err != nil || req.Engaged == nil {
		return c.JSON(http.StatusBadRequest, errJSON("engaged is required", "validation_error", "VALIDATION_ERROR"))
	}
	if h.hub == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "hub_error", "HUB_UNAVAILABLE"))
	}

	actor := actorFromContext(c)
	ks := cfginterception.Killswitch{Engaged: *req.Engaged}
	action := "disengage"
	if ks.Engaged {
		action = "engage"
	}

	// Fan out across every Thing type that has a kill-switch receiver.
	// The first leg (compliance-proxy) is the canonical one: its Hub
	// response carries the version + notified/online counts that flow
	// back into the admin audit entry and the HTTP response. The agent
	// leg is best-effort — a transient Hub error on agent must NOT abort
	// the compliance-proxy update (the operator's intent is "stop bumping
	// everywhere"; rolling back the compliance-proxy state because the
	// agent leg failed would leave the fleet in a worse state than a
	// partial fan-out). The drift reconciler (reconcile.go) will
	// re-push to agent on its next tick.
	ctx := c.Request().Context()
	var primaryResp *hub.ConfigChangeResponse
	thingsNotified := 0
	thingsOnline := 0
	for _, thingType := range killswitchFanoutTypes {
		resp, notifyErr := h.hub.NotifyConfigChange(ctx, hub.ConfigChangeRequest{
			ThingType: thingType,
			ConfigKey: configKeyKillswitch,
			State:     ks,
			Action:    action,
			ActorID:   actor.UserID,
			ActorName: actor.Name,
			SourceIP:  c.RealIP(),
		})
		if notifyErr != nil {
			if thingType == thingTypeComplianceProxy {
				// Primary leg failure → 502 so the UI surfaces a real
				// error and the admin re-tries. The agent leg has not
				// been attempted yet at this point.
				h.logger.Error("notify hub killswitch", "thingType", thingType, "error", notifyErr)
				return c.JSON(http.StatusBadGateway, errJSON("Hub unavailable", "hub_error", "HUB_UNAVAILABLE"))
			}
			// Secondary leg failure → log + continue. The drift
			// reconciler will re-push on its next tick.
			h.logger.Error("killswitch fanout failed (continuing)", "thingType", thingType, "error", notifyErr)
			continue
		}
		if thingType == thingTypeComplianceProxy {
			primaryResp = resp
		}
		if resp != nil {
			thingsNotified += resp.ThingsNotified
			thingsOnline += resp.ThingsOnline
		}
	}
	if primaryResp == nil {
		// Defensive: killswitchFanoutTypes is package-private and
		// always lists compliance-proxy first, so this branch is
		// unreachable unless someone reorders the constants. Surface
		// a 500 rather than panic on the resp.Version dereference.
		h.logger.Error("killswitch fanout produced no primary response")
		return c.JSON(http.StatusInternalServerError, errJSON("Hub did not respond", "server_error", "INTERNAL_ERROR"))
	}

	// Admin audit ledger (MQ → admin_audit_log). Hub also persists
	// config_change_event per leg. engage/disengage are both a VerbToggle
	// on the kill-switch resource; the specific direction is captured
	// in AfterState.engaged. Keeps SIEM eventType stable as
	// "kill-switch.toggle".
	ae := audit.EntryFor(c, iam.ResourceKillSwitch, iam.VerbToggle)
	ae.AfterState = map[string]any{"engaged": ks.Engaged, "version": primaryResp.Version, "intent": action}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{
		"engaged":        ks.Engaged,
		"version":        primaryResp.Version,
		"thingsNotified": thingsNotified,
		"thingsOnline":   thingsOnline,
	})
}

// --- Helper-copies (R6 runbook §4.2 option 1) ---

// Actor captures the authenticated principal identity propagated into
// hub.ConfigChangeRequest. Local copy of handler/helpers.go
// Actor — keeps killswitch/ free of cross-package back-imports.
type Actor struct {
	UserID string
	Name   string
}

// actorFromContext extracts the caller identity attached by the admin
// auth middleware. Returns zero-value Actor when no AdminAuth is
// present — the caller decides how to surface that.
func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

// errJSON shapes the standard error envelope CP returns.
func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}
