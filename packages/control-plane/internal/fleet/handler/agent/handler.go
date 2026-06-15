// Package agent owns the Control Plane admin API for agent device
// management: enrollment, fleet listing, device groups + bulk ops,
// smart-group membership eval, fleet user admin. R8-B2 — see
// r8-directory-size-decomp-plan.md.
package agent

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/providerstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentauditstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// rawPool is the minimal pgx surface used by device-tags and
// smart-group-preview, which issue raw SQL outside of the sub-store layer.
// *pgxpool.Pool satisfies this in production; pgxmock.PgxPoolIface satisfies
// it in tests.
type rawPool interface {
	agentstore.PgxPool
}

// HubAPI is the union Hub surface this package needs.
type HubAPI interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
	CreateEnrollmentToken(ctx context.Context, req hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error)
	ForceResyncAll(ctx context.Context, thingID string) (map[string]any, error)
}

// Deps is the construction-time arg shape. Pool accepts the concrete
// *pgxpool.Pool in production; tests may pass a pgxmock.PgxPoolIface
// (which satisfies agentstore.PgxPool = rawPool) so that sub-store
// SQL expectations work without a live database.
type Deps struct {
	Pool   rawPool
	Hub    HubAPI
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler owns the agent admin API surface.
type Handler struct {
	pool       rawPool // interface-typed so tests can inject pgxmock; set from Deps.Pool
	agents     *agentstore.Store
	agentAudit *agentauditstore.Store
	fleet      *fleetstore.Store
	users      *userstore.Store
	metrics    *metricsstore.Store
	provStore  *providerstore.Store
	hub        HubAPI
	audit      *audit.Writer
	logger     *slog.Logger

	// updateDeviceTagsFn is the unit-test seam for the per-device tag
	// UPDATE that runs through the pool. Default uses h.pool.Exec; tests
	// inject a fake so the handler can be driven without a live pool.
	// Production callers (cmd/control-plane/main.go) leave this nil —
	// New() does not set it.
	updateDeviceTagsFn func(ctx context.Context, deviceID string, tags []string) error

	// previewDevicesFn is the unit-test seam for smart-group membership
	// preview. Default uses previewDevicesForSmartEval which runs the
	// fleet-loading SELECT against the pool; tests inject a fake to skip
	// the live pool. Production callers leave it nil.
	previewDevicesFn func(ctx context.Context) ([]previewDevice, error)
}

// New constructs a Handler from Deps.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{pool: d.Pool, hub: d.Hub, audit: d.Audit, logger: logger}
	if d.Pool != nil {
		h.agents = agentstore.New(d.Pool)
		h.agentAudit = agentauditstore.New(d.Pool)
		h.fleet = fleetstore.New(d.Pool)
		h.users = userstore.New(d.Pool)
		h.metrics = metricsstore.New(d.Pool)
		h.provStore = providerstore.New(d.Pool)
	}
	return h
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

// Actor mirrors handler.Actor.
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

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
	startStr := c.QueryParam("startTime")
	if startStr == "" {
		startStr = c.QueryParam("start")
	}
	if startStr != "" {
		if t, ok := parseRFC3339Flexible(startStr); ok {
			start = &t
		}
	}
	endStr := c.QueryParam("endTime")
	if endStr == "" {
		endStr = c.QueryParam("end")
	}
	if endStr != "" {
		if t, ok := parseRFC3339Flexible(endStr); ok {
			end = &t
		}
	}
	return
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func currentUserID(c echo.Context) string {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return ""
	}
	return aa.KeyID
}

// parseAdminAuditParams reads pagination + action/entity-type filters
// + time range for /agent-users/:id/audit endpoints.
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

// queryMetricsOrFallback runs the rollup query (TimeSeries-aware vs cascade
// path) and returns the built result. Local copy of analytics.queryMetricsOrFallback
// (R6 helper-copy strategy) used by fleet analytics endpoints.
func (h *Handler) queryMetricsOrFallback(ctx context.Context, q metricspkg.MetricsQuery) (*metricspkg.MetricsResult, error) {
	var rows []metricspkg.RollupRow
	var err error
	if q.TimeSeries {
		rows, err = h.metrics.QueryRollupAware(ctx, q)
	} else {
		rows, err = h.metrics.QueryRollupCascade(ctx, q)
	}
	if err == nil && len(rows) > 0 {
		gran := metricspkg.SelectGranularity(q.StartTime, q.EndTime)
		return metricspkg.BuildResult(q, rows, gran), nil
	}
	return nil, nil
}

// RegisterRoutes mounts every agent admin endpoint under the
// supplied group. The DeviceGroupLookup-aware variant is wired by
// the caller and passed as iamMWDevice.
func (h *Handler) RegisterRoutes(
	g *echo.Group,
	iamMW func(action string) echo.MiddlewareFunc,
	iamMWDevice func(action, deviceIDParam string) echo.MiddlewareFunc,
) {
	h.RegisterAdminAgentDeviceRoutes(g, iamMW, iamMWDevice)
	h.RegisterDeviceGroupRoutes(g, iamMW)
	h.RegisterFleetRoutes(g, iamMW)
	h.RegisterFleetAnalyticsRoutes(g, iamMW)
}
