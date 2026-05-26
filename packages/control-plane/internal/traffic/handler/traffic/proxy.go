package traffic

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/compliancestore"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterProxyRoutes registers the forward-proxy operator dashboard under
// /api/admin/proxy/*. The "proxy" segment is product naming (transparent forward
// proxy), not "this group always reverse-proxies to compliance-proxy".
//
// Split:
//   - registerComplianceProxyRuntimeForwardRoutes — GETs that must hit the live
//     compliance-proxy HTTP runtime (/healthz, /connections, /metrics). Answers
//     come from process memory and Prometheus, not PostgreSQL.
//   - registerForwardProxyPostgresReadRoutes — Control Plane reads Postgres
//     (traffic_event, rollups, system_metadata). Nothing here is forwarded to
//     compliance-proxy; that binary has no HTTP audit/analytics surface anymore
//     (runtimeapi-slimming).
//
// Mutating killswitch, exemptions, and alert config live under /api/admin/compliance/*
// and Hub shadow push, not under /proxy/*.
func (h *Handler) RegisterProxyRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	h.registerComplianceProxyRuntimeForwardRoutes(g, iamMW)
	h.registerForwardProxyPostgresReadRoutes(g, iamMW)
}

func (h *Handler) registerComplianceProxyRuntimeForwardRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/proxy/health", h.ProxyHealth, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/proxy/connections", h.ProxyConnections, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/proxy/metrics", h.ProxyMetrics, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

func (h *Handler) registerForwardProxyPostgresReadRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/proxy/compliance/coverage", h.ProxyComplianceCoverage, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	g.GET("/proxy/compliance/hook-health", h.ProxyComplianceHookHealth, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	g.GET("/proxy/compliance/reject-stats", h.ProxyComplianceRejectStats, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	g.GET("/proxy/compliance/export", h.ProxyComplianceExport, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	g.GET("/proxy/reject-config", h.ProxyRejectConfigGet, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

// defaultProxyHTTPClient is the fallback used when AdminHandler.ComplianceProxyClient
// is nil (e.g. tests that build AdminHandler by hand). Production wiring in
// cmd/control-plane/main.go injects a client built from
// cfg.HTTPClients.ComplianceProxyAdmin.TimeoutSec.
var defaultProxyHTTPClient = nexushttp.New(nexushttp.Config{
	Timeout:        10 * time.Second,
	Caller:         "cp-admin-proxy",
	PropagateReqID: true,
})

func (h *Handler) proxyClient() *http.Client {
	if h.httpClient != nil {
		return h.httpClient
	}
	return defaultProxyHTTPClient
}

func (h *Handler) proxyForward(c echo.Context, method, path string) error {
	url := h.proxy.ComplianceProxyRuntimeURL + path
	token := h.proxy.ComplianceProxyAPIToken

	var bodyReader io.Reader
	if method == http.MethodPost || method == http.MethodPut {
		bodyReader = c.Request().Body
	}

	req, err := http.NewRequestWithContext(c.Request().Context(), method, url, bodyReader)
	if err != nil {
		h.logger.Error("proxy: failed to create request", "method", method, "path", path, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create proxy request", "server_error", ""))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Forward authenticated user identity so downstream services can record
	// who performed the action (e.g. kill switch toggle history).
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		req.Header.Set("X-Nexus-Actor-Id", aa.KeyID)
		req.Header.Set("X-Nexus-Actor-Name", aa.KeyName)
	}
	// Forward query params
	req.URL.RawQuery = c.QueryString()

	start := time.Now()
	resp, err := h.proxyClient().Do(req)
	if err != nil {
		h.logger.Warn("proxy: compliance-proxy unreachable", "method", method, "path", path, "duration", time.Since(start), "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Compliance proxy unreachable", "server_error", "COMPLIANCE_PROXY_UNREACHABLE"))
	}
	defer resp.Body.Close() //nolint:errcheck

	h.logger.Debug("proxy: forwarded", "method", method, "path", path, "status", resp.StatusCode, "duration", time.Since(start))

	// Copy response headers
	for k, vals := range resp.Header {
		for _, v := range vals {
			c.Response().Header().Add(k, v)
		}
	}
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = io.Copy(c.Response(), resp.Body)
	return nil
}

func (h *Handler) ProxyHealth(c echo.Context) error {
	return h.proxyForward(c, http.MethodGet, "/healthz")
}

func (h *Handler) ProxyConnections(c echo.Context) error {
	return h.proxyForward(c, http.MethodGet, "/connections")
}

func (h *Handler) ProxyMetrics(c echo.Context) error {
	return h.proxyForward(c, http.MethodGet, "/metrics")
}

// ProxyComplianceCoverage returns compliance coverage stats from rollup data.
func (h *Handler) ProxyComplianceCoverage(c echo.Context) error {
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		now := time.Now()
		yearAgo := now.AddDate(-1, 0, 0)
		if start == nil {
			start = &yearAgo
		}
		if end == nil {
			end = &now
		}
	}
	result, _ := h.tryRollupComplianceCoverage(c.Request().Context(), *start, *end)
	if result != nil {
		return c.JSON(http.StatusOK, result)
	}
	// Rollup returned no data — return empty coverage.
	return c.JSON(http.StatusOK, &compliancestore.ComplianceCoverageStats{
		Breakdown: map[string]int{},
		Period:    compliancestore.TimePeriod{Start: *start, End: *end},
	})
}

// ProxyComplianceHookHealth returns hook health stats from rollup data.
func (h *Handler) ProxyComplianceHookHealth(c echo.Context) error {
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		now := time.Now()
		yearAgo := now.AddDate(-1, 0, 0)
		if start == nil {
			start = &yearAgo
		}
		if end == nil {
			end = &now
		}
	}
	result, _ := h.tryRollupHookHealth(c.Request().Context(), *start, *end)
	if result != nil {
		return c.JSON(http.StatusOK, result)
	}
	// Rollup returned no data — return empty hook health.
	return c.JSON(http.StatusOK, &compliancestore.HookHealthStats{
		Period:         compliancestore.TimePeriod{Start: *start, End: *end},
		TopReasonCodes: []compliancestore.LabelCount{},
	})
}

// ProxyComplianceRejectStats returns reject stats from rollup data.
func (h *Handler) ProxyComplianceRejectStats(c echo.Context) error {
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		now := time.Now()
		yearAgo := now.AddDate(-1, 0, 0)
		if start == nil {
			start = &yearAgo
		}
		if end == nil {
			end = &now
		}
	}
	result, _ := h.tryRollupRejectStats(c.Request().Context(), *start, *end)
	if result != nil {
		return c.JSON(http.StatusOK, result)
	}
	// Rollup returned no data — return empty reject stats.
	return c.JSON(http.StatusOK, &compliancestore.RejectStats{
		Period:         compliancestore.TimePeriod{Start: *start, End: *end},
		TopTargets:     []compliancestore.LabelCount{},
		TopReasonCodes: []compliancestore.LabelCount{},
		BySource:       []compliancestore.LabelCount{},
	})
}

// ProxyComplianceExport streams compliance-proxy + agent traffic events as
// CSV for offline audit review. The button that fires this request labels
// itself "Export CSV", so we emit real RFC 4180 CSV with a Content-Type
// browsers will treat as a download and a filename stamped with the active
// time window. `encoding/csv` already quotes fields that contain commas,
// newlines, or double quotes, so no manual escaping is needed.
//
// Hard caps:
//   - time window defaults to the last 24h when either bound is omitted so a
//     malformed filter can't trigger a table-scan export.
//   - row budget is capped at 50k to protect the API worker and keep the
//     download under Excel's 1.04M-row ceiling; future work can stream the
//     full table via a background job.
const complianceExportMaxRows = 50000

func (h *Handler) ProxyComplianceExport(c echo.Context) error {
	start, end := parseTimeRange(c)
	now := time.Now()
	if start == nil {
		s := now.Add(-24 * time.Hour)
		start = &s
	}
	if end == nil {
		end = &now
	}

	data, _, err := h.compliance.ListMatrixAuditEvents(c.Request().Context(), start, end, complianceExportMaxRows, 0)
	if err != nil {
		h.logger.Error("proxy compliance export", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	filename := fmt.Sprintf("compliance-events-%s_%s.csv",
		start.UTC().Format("20060102T150405Z"),
		end.UTC().Format("20060102T150405Z"),
	)
	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, "text/csv; charset=utf-8")
	resp.Header().Set(echo.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="%s"`, filename))
	resp.WriteHeader(http.StatusOK)

	w := csv.NewWriter(resp)
	defer w.Flush()

	header := []string{
		"timestamp", "id", "transactionId", "sourceIp", "targetHost",
		"method", "path", "statusCode", "hookDecision", "hookReasonCode",
		"latencyMs", "complianceTags",
	}
	if err := w.Write(header); err != nil {
		h.logger.Error("csv write header", "error", err)
		return nil
	}
	for _, r := range data {
		row := []string{
			formatCSVTimestamp(r.Timestamp),
			r.ID,
			r.TransactionID,
			r.SourceIP,
			r.TargetHost,
			derefString(r.Method),
			derefString(r.Path),
			formatOptionalInt(r.StatusCode),
			derefString(r.HookDecision),
			derefString(r.HookReasonCode),
			formatOptionalInt(r.LatencyMs),
			strings.Join(r.ComplianceTags, ","),
		}
		if err := w.Write(row); err != nil {
			h.logger.Error("csv write row", "error", err, "id", r.ID)
			return nil
		}
	}
	return nil
}

// formatCSVTimestamp renders a traffic_event.timestamp into RFC3339Nano so
// Excel / Google Sheets recognise it as a date. MatrixAuditRow.Timestamp is
// typed `any` because pgx can return it as either time.Time or a string
// depending on the column's Postgres type, so we handle both.
func formatCSVTimestamp(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func formatOptionalInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

// ProxyRejectConfigGet returns the reject config from system_metadata.
func (h *Handler) ProxyRejectConfigGet(c echo.Context) error {
	val, err := h.meta.GetSystemMetadata(c.Request().Context(), "reject_config")
	if err != nil {
		h.logger.Error("proxy reject-config get", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if val == nil {
		return c.JSON(http.StatusOK, map[string]any{"defaultLevel": 0, "contactInfo": ""})
	}
	return c.JSONBlob(http.StatusOK, val)
}

// ComplianceAudit returns unified compliance traffic events across all three
// enforcement layers (ai-gateway, compliance-proxy, agent).
func (h *Handler) ComplianceAudit(c echo.Context) error {
	pg := parsePagination(c)
	start, end := parseTimeRange(c)

	var tags []string
	if raw := c.QueryParam("complianceTags"); raw != "" {
		tags = strings.Split(raw, ",")
	}

	params := compliancestore.ComplianceAuditParams{
		Source:         c.QueryParam("source"),
		HookDecision:   c.QueryParam("hookDecision"),
		ComplianceTags: tags,
		SourceIP:       c.QueryParam("sourceIp"),
		TargetHost:     c.QueryParam("targetHost"),
		Start:          start,
		End:            end,
		Limit:          pg.Limit,
		Offset:         pg.Offset,
	}
	data, total, err := h.compliance.ListComplianceAuditEvents(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("compliance audit", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total})
}

// ComplianceTrinity returns per-layer compliance stats (ai-gateway, compliance-proxy, agent)
// for the given time range (defaults to last 24h).
func (h *Handler) ComplianceTrinity(c echo.Context) error {
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		now := time.Now()
		yearAgo := now.AddDate(-1, 0, 0)
		if start == nil {
			start = &yearAgo
		}
		if end == nil {
			end = &now
		}
	}
	stats, err := h.compliance.GetTrinityStats(c.Request().Context(), *start, *end)
	if err != nil {
		h.logger.Error("compliance trinity", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, stats)
}
