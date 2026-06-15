// Package quota owns the Control Plane admin API for AI-gateway quota
// policies + per-VK overrides + per-tier analytics. R8-B16 leaf
// extraction.
package quota

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/quotastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/orgstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// HubAPI is the narrow surface quota/ uses. Quota policies/overrides cap spend
// per VK/org; a dropped invalidation leaves the gateway enforcing stale caps,
// so the CUD paths fail loud (HTTP 502) via the error-returning
// InvalidateConfigE instead of fire-and-forget.
type HubAPI interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfigE(ctx context.Context, thingType, configKey string) error
}

// quotaDB is the narrow persistence seam for quota policies and overrides.
// *quotastore.Store satisfies this in production; tests supply an in-memory double.
type quotaDB interface {
	ListQuotaPolicies(ctx context.Context, p quotastore.QuotaPolicyListParams) ([]quotastore.QuotaPolicy, int, error)
	ListEnabledPoliciesForScopes(ctx context.Context, scopes []string) ([]quotastore.QuotaPolicy, error)
	GetQuotaPolicy(ctx context.Context, id string) (*quotastore.QuotaPolicy, error)
	CreateQuotaPolicy(ctx context.Context, p quotastore.CreateQuotaPolicyParams) (*quotastore.QuotaPolicy, error)
	UpdateQuotaPolicy(ctx context.Context, id string, p quotastore.UpdateQuotaPolicyParams) (*quotastore.QuotaPolicy, error)
	DeleteQuotaPolicy(ctx context.Context, id string) error
	ListQuotaOverrides(ctx context.Context, p quotastore.QuotaOverrideListParams) ([]quotastore.QuotaOverride, int, error)
	GetQuotaOverride(ctx context.Context, id string) (*quotastore.QuotaOverride, error)
	GetQuotaOverrideByTarget(ctx context.Context, targetType, targetID string) (*quotastore.QuotaOverride, error)
	CreateQuotaOverride(ctx context.Context, p quotastore.CreateQuotaOverrideParams) (*quotastore.QuotaOverride, error)
	UpdateQuotaOverride(ctx context.Context, id string, p quotastore.UpdateQuotaOverrideParams) (*quotastore.QuotaOverride, error)
	DeleteQuotaOverride(ctx context.Context, id string) error
}

// metricsDB is the narrow persistence seam for rollup queries.
// *metricsstore.Store satisfies this in production.
type metricsDB interface {
	QueryRollup(ctx context.Context, q metrics.MetricsQuery) ([]metrics.RollupRow, error)
}

// usersDB is the narrow persistence seam for user lookups.
// *userstore.Store satisfies this in production.
type usersDB interface {
	GetNexusUserSafe(ctx context.Context, id string) (*userstore.NexusUserSafe, error)
	GetNexusUserOrgInfo(ctx context.Context, userID string) (orgID, orgName string, err error)
}

// orgsDB is the narrow persistence seam for organization + project lookups.
// *orgstore.Store satisfies this in production.
type orgsDB interface {
	GetOrganization(ctx context.Context, id string) (*orgstore.Organization, error)
	GetProject(ctx context.Context, id string) (*orgstore.Project, error)
}

// vksDB is the narrow persistence seam for virtual key lookups.
// *vkstore.Store satisfies this in production.
type vksDB interface {
	GetVirtualKey(ctx context.Context, id string) (*vkstore.VirtualKey, error)
}

type Deps struct {
	Pool   quotastore.PgxPool
	Hub    HubAPI
	Audit  *audit.Writer
	Logger *slog.Logger
}

type Handler struct {
	quota   quotaDB
	metrics metricsDB
	users   usersDB
	orgs    orgsDB
	vks     vksDB
	hub     HubAPI
	audit   *audit.Writer
	logger  *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{hub: d.Hub, audit: d.Audit, logger: logger}
	if d.Pool != nil {
		h.quota = quotastore.New(d.Pool)
		h.metrics = metricsstore.New(d.Pool)
		h.users = userstore.New(d.Pool)
		h.orgs = orgstore.New(d.Pool)
		h.vks = vkstore.New(d.Pool)
	}
	return h
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

type Actor struct {
	UserID string
	Name   string
}

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
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

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

// targetEntityExists confirms targetID references a live entity of the given
// type (user / vk / organization / project). It exists to stop a typo'd
// targetId or organizationId from creating a quota override / policy row that
// matches no chain level and silently enforces nothing. Returns
// (exists, lookupErr): a non-nil lookupErr signals a DB failure (caller → 500);
// exists=false signals a missing referent (caller → 400). When the backing
// store is unavailable (dev mode without a DB) it degrades to exists=true so the
// path is not blocked — production always wires the stores.
func (h *Handler) targetEntityExists(ctx context.Context, targetType, targetID string) (bool, error) {
	switch targetType {
	case "user":
		if h.users == nil {
			return true, nil
		}
		u, err := h.users.GetNexusUserSafe(ctx, targetID)
		return u != nil, err
	case "vk", "virtual_key":
		if h.vks == nil {
			return true, nil
		}
		vk, err := h.vks.GetVirtualKey(ctx, targetID)
		return vk != nil, err
	case "organization":
		if h.orgs == nil {
			return true, nil
		}
		o, err := h.orgs.GetOrganization(ctx, targetID)
		return o != nil, err
	case "project":
		if h.orgs == nil {
			return true, nil
		}
		p, err := h.orgs.GetProject(ctx, targetID)
		return p != nil, err
	}
	return true, nil
}
