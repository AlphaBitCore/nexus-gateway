// Package infra owns the Control Plane admin API for infrastructure
// surfaces: nodes (BFF reverse proxy to Hub) + config-sync + jobs +
// enrollment tokens, per-Thing runtime + config-override admin,
// service public-URL aggregation, agent setup helpers, diagnostic
// events + silences + diag-mode windows. R8-B3 — see
// r8-directory-size-decomp-plan.md.
package infra

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/interception/interceptionstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/diag/diagstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// HubAPI is the union surface infra/ needs (BFF reverse proxy uses
// most of HubNotifier; node admin uses InvalidateConfig +
// NotifyConfigChange + ForceResyncAll).
type HubAPI interface {
	BaseURL() string
	Token() string
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
	ForceResyncAll(ctx context.Context, thingID string) (map[string]any, error)
	CreateEnrollmentToken(ctx context.Context, req hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error)
	GetThingRuntime(ctx context.Context, thingID string) ([]byte, int, error)
	GetThingServiceMeta(ctx context.Context, thingID string) (*hub.ThingServiceMeta, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	DB                       *store.DB
	Hub                      HubAPI
	Audit                    *audit.Writer
	Logger                   *slog.Logger
	ThingOverrideGroupLookup ThingOverrideGroupLookup
	HubProxyClient           *http.Client
	ComplianceProxyClient    *http.Client
	Proxy                    ProxyConfig
}

// ProxyConfig holds BFF proxy settings the setup helper needs.
type ProxyConfig struct {
	ComplianceProxyRuntimeURL string
	ComplianceProxyAPIToken   string
	AIGatewayURL              string
}

// ThingOverrideGroupLookup mirrors handler/thingOverrideGroupLookup.
type ThingOverrideGroupLookup interface {
	ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error)
}

// Handler owns the infra admin API surface.
type Handler struct {
	db                          *store.DB // kept for Pool.Query in service_urls.go + native store methods
	ops                         *opsstore.Store
	diag                        *diagstore.Store
	fleet                       *fleetstore.Store
	interception                *interceptionstore.Store
	hub                         HubAPI
	audit                       *audit.Writer
	logger                      *slog.Logger
	thingOverrideGroupLookupRef ThingOverrideGroupLookup
	hubProxyClientRef           *http.Client
	complianceProxyClient       *http.Client
	proxy                       ProxyConfig

	// servicePublicURLsQueryFn is the unit-test seam for the direct
	// `h.db.Pool.Query` in GetServicePublicURLs. The default uses the
	// concrete *pgxpool.Pool reachable via h.db.Pool; tests inject a
	// fake so the handler can be driven without standing up Postgres.
	// Production callers leave this nil — New() does not set it.
	servicePublicURLsQueryFn func(ctx context.Context) ([]servicePublicURLRow, error)

	// servicePoolOverride is the lower-level test seam that exercises the
	// real queryServicePublicURLs body against a pgxmock pool — kept
	// separate from servicePublicURLsQueryFn so test code can choose
	// either the high-level (return rows directly) or low-level
	// (pgxmock-driven cursor/iterate) form.
	servicePoolOverride servicePoolQueryer

	// dbPingFn is the unit-test seam for the h.db.Pool.Ping call inside
	// ReadinessCheck. Production callers leave this nil — the handler
	// falls through to h.db.Pool.Ping directly. Tests inject a func so
	// the DB health branch can be exercised without a live *pgxpool.Pool.
	dbPingFn func(ctx context.Context) error
}

// servicePublicURLRow is the per-row shape GetServicePublicURLs consumes.
// Kept exported-private so the function-seam signature stays narrow.
type servicePublicURLRow struct {
	ThingType string
	PublicURL string
}

// New constructs a Handler from Deps.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		db:                          d.DB,
		hub:                         d.Hub,
		audit:                       d.Audit,
		logger:                      logger,
		thingOverrideGroupLookupRef: d.ThingOverrideGroupLookup,
		hubProxyClientRef:           d.HubProxyClient,
		complianceProxyClient:       d.ComplianceProxyClient,
		proxy:                       d.Proxy,
	}
	if d.DB != nil {
		h.ops = opsstore.New(d.DB.InternalPool())
		h.diag = diagstore.New(d.DB.InternalPool())
		h.fleet = fleetstore.New(d.DB.InternalPool())
		h.interception = interceptionstore.New(d.DB.InternalPool())
	}
	return h
}

// RegisterRoutes mounts every infra admin endpoint.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	h.RegisterNodeRuntimeRoutes(g, iamMW)
	h.RegisterAdminNodeOverridesRoutes(g, iamMW)
	h.RegisterServiceURLRoutes(g, iamMW)
	h.RegisterNodeRoutes(g, iamMW)
	h.RegisterSetupRoutes(g, iamMW)
	h.RegisterDiagSilencesRoutes(g, iamMW)
	h.RegisterDiagEventsRoutes(g, iamMW)
	h.RegisterDiagModeRoutes(g, iamMW)
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

// httpErr / badReq / parseFromTo copied from handler/opsmetrics.go
// per R6 runbook §4.2.
type httpErr struct {
	status int
	body   map[string]any
}

func badReq(msg string) *httpErr {
	return &httpErr{
		status: http.StatusBadRequest,
		body:   errJSON(msg, "validation_error", "VALIDATION_ERROR"),
	}
}

func parseFromTo(c echo.Context) (time.Time, time.Time, *httpErr) {
	fromStr := c.QueryParam("from")
	toStr := c.QueryParam("to")
	if fromStr == "" || toStr == "" {
		return time.Time{}, time.Time{}, badReq("from and to are required (RFC3339)")
	}
	from, ok := parseRFC3339Flexible(fromStr)
	if !ok {
		return time.Time{}, time.Time{}, badReq("invalid from (need RFC3339)")
	}
	to, ok2 := parseRFC3339Flexible(toStr)
	if !ok2 {
		return time.Time{}, time.Time{}, badReq("invalid to (need RFC3339)")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, badReq("from must be < to")
	}
	return from, to, nil
}
