// Package virtualkey owns the Control Plane admin API for virtual
// key CRUD (/api/admin/virtual-keys) + the approval workflow
// (/approve, /reject, /renew, /revoke). R6 fifth domain extracted
// from the flat handler/ package; recipe documented in
// docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
package virtualkey

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

const vkPrefix = "nvk_"

// HubVKInvalidator is the narrow Hub surface virtualkey/ needs:
// targeted invalidate-by-hash via NotifyConfigChange (for VK
// update/delete/regenerate) PLUS fire-and-forget InvalidateConfig
// (for approve/renew/revoke — the approval workflow doesn't carry a
// per-hash payload). Fourth narrow-Hub pattern in the catalog.
type HubVKInvalidator interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool   vkstore.PgxPool
	Hub    HubVKInvalidator
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler is the per-domain admin handler for /api/admin/virtual-keys*
// endpoints.
type Handler struct {
	vks    *vkstore.Store
	iam    *iamstore.Store
	hub    HubVKInvalidator
	audit  *audit.Writer
	logger *slog.Logger
}

// New constructs a virtualkey Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{hub: d.Hub, audit: d.Audit, logger: d.Logger}
	if d.Pool != nil {
		h.vks = vkstore.New(d.Pool)
		h.iam = iamstore.New(d.Pool)
	}
	return h
}

// RegisterRoutes registers virtual key CRUD routes.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/virtual-keys", h.ListVirtualKeys, iamMW(iam.ResourceVirtualKey.Action(iam.VerbRead)))
	g.POST("/virtual-keys", h.CreateVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbCreate)))
	g.GET("/virtual-keys/:id", h.GetVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbRead)))
	g.PUT("/virtual-keys/:id", h.UpdateVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbUpdate)))
	g.DELETE("/virtual-keys/:id", h.DeleteVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbDelete)))
	g.POST("/virtual-keys/:id/regenerate", h.RegenerateVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbUpdate)))
}

// RegisterApprovalRoutes registers the approval workflow routes
// (kept separate per the original RegisterVKApprovalRoutes signature).
func (h *Handler) RegisterApprovalRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/virtual-keys/:id/approve", h.ApproveVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbApprove)))
	g.POST("/virtual-keys/:id/reject", h.RejectVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbReject)))
	g.POST("/virtual-keys/:id/renew", h.RenewVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbRenew)))
	g.POST("/virtual-keys/:id/revoke", h.RevokeVirtualKey, iamMW(iam.ResourceVirtualKey.Action(iam.VerbRevoke)))
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

type actor struct {
	UserID string
	Name   string
}

func actorFromContext(c echo.Context) actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return actor{}
	}
	return actor{UserID: aa.KeyID, Name: aa.KeyName}
}

func sourceIP(c echo.Context) string { return c.RealIP() }

type pagination struct {
	Limit  int
	Offset int
}

func parsePagination(c echo.Context) pagination {
	limit := 50
	offset := 0
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return pagination{Limit: limit, Offset: offset}
}

// isSuperAdmin checks whether the authenticated principal belongs to
// the "super-admins" IAM group. Returns false when aa is nil or DB
// lookup fails. Local copy of *AdminHandler.isSuperAdmin (R6
// helper-copy strategy).
func (h *Handler) isSuperAdmin(c echo.Context, aa *auth.AdminAuth) bool {
	if aa == nil {
		return false
	}
	pt := aa.AuthPrincipalType
	if pt == "admin_user" {
		pt = "nexus_user"
	}
	groups, err := h.iam.ListGroupNamesForPrincipal(c.Request().Context(), pt, aa.KeyID)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == "super-admins" {
			return true
		}
	}
	return false
}

// notifyVKInvalidate pushes a targeted invalidate-by-hash to ai-gateway
// for the affected VK key hash. The data plane drops just that LRU
// entry instead of purging the whole VK cache. Best-effort.
func (h *Handler) notifyVKInvalidate(c echo.Context, keyHash *string) {
	if h.hub == nil || keyHash == nil || *keyHash == "" {
		return
	}
	payload := map[string]any{
		"op":  "invalidate",
		"ids": []string{*keyHash},
	}
	a := actorFromContext(c)
	if _, err := h.hub.NotifyConfigChange(c.Request().Context(), hub.ConfigChangeRequest{
		ThingType: "ai-gateway",
		ConfigKey: configkey.VirtualKeys,
		State:     payload,
		ActorID:   a.UserID,
		ActorName: a.Name,
		SourceIP:  sourceIP(c),
	}); err != nil {
		h.logger.Error("notify hub vk invalidate",
			"key_hash", *keyHash,
			"error", err,
		)
	}
}

// resolveVK loads the VK row identified by the :id path param.
func (h *Handler) resolveVK(c echo.Context) (*vkstore.VirtualKey, error) {
	return h.vks.GetVirtualKey(c.Request().Context(), c.Param("id"))
}

// generateVirtualKey produces a fresh raw key, its hash, and the
// short prefix.
func generateVirtualKey() (rawKey, keyHash, keyPrefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", err
	}
	rawKey = vkPrefix + hex.EncodeToString(b)
	keyHash = auth.HashAPIKey(rawKey)
	keyPrefix = rawKey[:12]
	return
}

// extractNullableTimeFromBody detects the three caller intents for a
// nullable datetime field in a PUT body:
//
//	field absent       → present=false (leave column unchanged)
//	field == null      → present=true, t=nil (clear column to SQL NULL)
//	field == "date"    → present=true, t=&parsedTime (set new value)
func extractNullableTimeFromBody(body []byte, field string) (present bool, t *time.Time, errMsg string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false, nil, ""
	}
	raw, ok := m[field]
	if !ok {
		return false, nil, ""
	}
	if string(raw) == "null" || len(raw) == 0 {
		return true, nil, ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, nil, "invalid " + field + " format"
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			return true, &parsed, ""
		}
	}
	return false, nil, "invalid " + field + ": expected RFC3339 or YYYY-MM-DD"
}
