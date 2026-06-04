// Package me owns the Control Plane admin API for /api/my/* + the
// /api/user/api-keys/* surface (personal user-level CRUD for the
// authenticated principal).
package me

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
)

// HubAPI is the narrow Hub surface me/ needs. The virtualkey
// sub-package needs both InvalidateConfig + NotifyConfigChange for
// per-VK hash invalidation; expose both.
type HubAPI interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
}

// meUserStore is the narrow userstore surface that me/ handlers need.
// Satisfied by *userstore.Store; test stubs implement this interface.
type meUserStore interface {
	FindNexusUserByID(ctx context.Context, id string) (*userstore.NexusUser, error)
	UpdateNexusUser(ctx context.Context, id string, p userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error)
	ListAdminAPIKeys(ctx context.Context, ownerUserID string) ([]userstore.AdminAPIKey, error)
	CreateAdminAPIKey(ctx context.Context, p userstore.CreateAdminAPIKeyParams) (*userstore.AdminAPIKey, error)
	GetAdminAPIKey(ctx context.Context, id string) (*userstore.AdminAPIKey, error)
	DeleteAdminAPIKey(ctx context.Context, id string) error
	RegenerateAdminAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error
}

// meIAMStore is the narrow iamstore surface that me/ handlers need.
// Satisfied by *iamstore.Store; test stubs implement this interface.
type meIAMStore interface {
	ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error)
}

// meTrafficStore is the narrow trafficstore surface that me/ handlers need.
// Satisfied by *trafficstore.Store; test stubs implement this interface.
type meTrafficStore interface {
	ListAdminAuditLogs(ctx context.Context, p trafficstore.AdminAuditLogListParams) ([]trafficstore.AdminAuditLogEntry, int, error)
}

// Deps is the construction-time arg shape. Pool is used to construct
// all sub-store instances and to wire the virtualkeys sub-handler.
type Deps struct {
	Pool   *pgxpool.Pool
	Hub    HubAPI
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler owns the /api/my/* + /api/user/api-keys/* admin surface.
// Sub-stores are constructed from Pool at New() time.
type Handler struct {
	pool    *pgxpool.Pool  // passed to virtualkeys sub-handler
	users   meUserStore    // FindNexusUserByID, UpdateNexusUser, ListAdminAPIKeys, etc.
	iam     meIAMStore     // ListGroupNamesForPrincipal
	traffic meTrafficStore // ListAdminAuditLogs
	hub     HubAPI
	audit   *audit.Writer
	logger  *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{pool: d.Pool, hub: d.Hub, audit: d.Audit, logger: logger}
	if d.Pool != nil {
		h.users = userstore.New(d.Pool)
		h.iam = iamstore.New(d.Pool)
		h.traffic = trafficstore.New(d.Pool)
	}
	return h
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "code": code}}
}

type Actor struct{ UserID, Name string }

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

func currentUserID(c echo.Context) string {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return ""
	}
	return aa.KeyID
}

type pagination struct{ Limit, Offset int }

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

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

func parseRFC3339Flexible(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func parseTimeRange(c echo.Context) (start, end *time.Time) {
	if v := c.QueryParam("startTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			start = &t
		}
	}
	if v := c.QueryParam("endTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			end = &t
		}
	}
	return
}

func parseAdminAuditParams(c echo.Context) trafficstore.AdminAuditLogListParams {
	pg := parsePagination(c)
	params := trafficstore.AdminAuditLogListParams{
		Action:     c.QueryParam("action"),
		EntityType: c.QueryParam("entityType"),
		Limit:      pg.Limit,
		Offset:     pg.Offset,
	}
	start, end := parseTimeRange(c)
	params.StartTime = start
	params.EndTime = end
	return params
}
