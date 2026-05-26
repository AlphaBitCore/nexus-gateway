// Package traffic owns the Control Plane admin API for traffic
// events + admin audit logs + the forward-proxy dashboard.
// R6 ninth domain.
package traffic

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/dsar/dsarstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/compliancestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// ProxyConfig is the BFF proxy snapshot the proxy/* admin routes use.
type ProxyConfig struct {
	ComplianceProxyRuntimeURL string
	ComplianceProxyAPIToken   string
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool       cpgx.PgxPool
	Audit      *audit.Writer
	Logger     *slog.Logger
	SpillStore spillstore.SpillStore
	Proxy      ProxyConfig
	HTTPClient *http.Client // optional; nil falls back to 10s default
}

// Handler is the per-domain admin handler for /api/admin/traffic*
// + /api/admin/proxy* + /api/admin/admin-audit-logs* endpoints.
type Handler struct {
	traffic            *trafficstore.Store
	dsar               *dsarstore.Store
	metrics            *metricsstore.Store
	compliance         *compliancestore.Store
	meta               *systemmetastore.Store
	audit              *audit.Writer
	logger             *slog.Logger
	spillStore         spillstore.SpillStore
	proxy              ProxyConfig
	httpClient *http.Client
}

// New constructs a traffic Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{
		audit:      d.Audit,
		logger:     d.Logger,
		spillStore: d.SpillStore,
		proxy:      d.Proxy,
		httpClient: d.HTTPClient,
	}
	if d.Pool != nil {
		ms := metricsstore.New(d.Pool)
		h.traffic = trafficstore.New(d.Pool)
		h.dsar = dsarstore.New(d.Pool)
		h.metrics = ms
		h.compliance = compliancestore.New(d.Pool, ms)
		h.meta = systemmetastore.NewFromPool(d.Pool)
	}
	return h
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

// queryMetricsOrFallback runs the rollup query and returns the built
// result. Local copy of analytics.Handler.queryMetricsOrFallback per
// R6 helper-copy strategy.
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

// strPtr returns a pointer to s. Tiny helper local copy.
func strPtr(s string) *string { return &s }

// firstNonEmpty returns the first non-empty string from the provided
// values. Local copy of handler/helpers.go's firstNonEmpty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// _ keeps middleware imported for future per-method context lookups
// that the sed-ported files will use.
var _ = middleware.AdminAuthFromContext
