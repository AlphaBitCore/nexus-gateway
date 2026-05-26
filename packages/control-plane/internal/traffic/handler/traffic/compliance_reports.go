package traffic

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/compliancestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/labstack/echo/v4"
)

// RegisterComplianceRoutes registers compliance report + unified audit + trinity routes.
// Gated on admin:compliance-report.read — the canonical catalog row for this
// surface — so security/compliance can read these dashboards without inheriting
// the broader admin:audit-log scope (which exposes every admin's actions).
func (h *Handler) RegisterComplianceRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/compliance/report", h.ComplianceReport, iamMW(iam.ResourceComplianceReport.Action(iam.VerbRead)))
	g.GET("/compliance/audit", h.ComplianceAudit, iamMW(iam.ResourceComplianceReport.Action(iam.VerbRead)))
	g.GET("/compliance/audit/:id", h.ComplianceAuditDetail, iamMW(iam.ResourceComplianceReport.Action(iam.VerbRead)))
	g.GET("/compliance/trinity", h.ComplianceTrinity, iamMW(iam.ResourceComplianceReport.Action(iam.VerbRead)))
	g.GET("/compliance/overview", h.ComplianceOverview, iamMW(iam.ResourceComplianceReport.Action(iam.VerbRead)))
	g.GET("/compliance/overview/export", h.ComplianceOverviewExport, iamMW(iam.ResourceComplianceReport.Action(iam.VerbRead)))
}

// ComplianceAuditDetail returns a single compliance traffic event by ID.
func (h *Handler) ComplianceAuditDetail(c echo.Context) error {
	id := c.Param("id")
	evt, err := h.compliance.GetMatrixAuditEvent(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Audit event not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, evt)
}

func (h *Handler) ComplianceReport(c echo.Context) error {
	startStr := c.QueryParam("startTime")
	endStr := c.QueryParam("endTime")
	if startStr == "" || endStr == "" {
		return c.JSON(http.StatusBadRequest, errJSON("startTime and endTime are required", "validation_error", ""))
	}

	start, ok := parseRFC3339Flexible(startStr)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid startTime", "validation_error", ""))
	}
	end, ok2 := parseRFC3339Flexible(endStr)
	if !ok2 {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid endTime", "validation_error", ""))
	}

	// Max 366 days
	if end.Sub(start) > 366*24*time.Hour {
		return c.JSON(http.StatusBadRequest, errJSON("Time window may not exceed 366 days", "validation_error", ""))
	}

	ctx := c.Request().Context()

	coverage, err := h.compliance.GetComplianceCoverage(ctx, start, end)
	if err != nil {
		h.logger.Error("compliance coverage query", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	hookHealth, err := h.compliance.GetHookHealth(ctx, start, end)
	if err != nil {
		h.logger.Error("hook health query", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// DSAR counts
	dsarCounts, _ := h.dsar.GetDSARStatusCounts(ctx)
	completedInPeriod, _ := h.dsar.GetDSARCompletedInPeriod(ctx, start, end)

	report := map[string]any{
		"period": map[string]any{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)},
		"coverage": map[string]any{
			"coveragePercent": coverage.CoveragePct,
			"breakdown":       coverage.Breakdown,
		},
		"hookHealth": map[string]any{
			"total":      hookHealth.Total,
			"byDecision": hookHealth.ByDecision,
		},
		"dsar": map[string]any{
			"pending":           dsarCounts.Pending,
			"inProgress":        dsarCounts.InProgress,
			"completed":         dsarCounts.Completed,
			"rejected":          dsarCounts.Rejected,
			"completedInPeriod": completedInPeriod,
		},
	}

	ae := audit.EntryFor(c, iam.ResourceComplianceReport, iam.VerbRead)
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, report)
}

// ComplianceOverview returns the full global compliance dashboard payload in a
// single call: KPIs, Trinity per-layer stats, hook health, and top-10 blocked
// lists. Data comes from rollup cascade (bump + hook metrics) and direct
// traffic_event queries (trinity decisions and top-N lists).
func (h *Handler) ComplianceOverview(c echo.Context) error {
	startStr := c.QueryParam("startTime")
	endStr := c.QueryParam("endTime")

	now := time.Now()
	var start, end time.Time
	if startStr == "" {
		start = now.Add(-7 * 24 * time.Hour)
	} else {
		var ok bool
		if start, ok = parseRFC3339Flexible(startStr); !ok {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid startTime", "validation_error", ""))
		}
	}
	if endStr == "" {
		end = now
	} else {
		var ok bool
		if end, ok = parseRFC3339Flexible(endStr); !ok {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid endTime", "validation_error", ""))
		}
	}
	if end.Sub(start) > 366*24*time.Hour {
		return c.JSON(http.StatusBadRequest, errJSON("Time window may not exceed 366 days", "validation_error", ""))
	}

	ctx := c.Request().Context()
	data, err := h.compliance.GetComplianceDashboard(ctx, start, end)
	if err != nil {
		h.logger.Error("compliance overview query", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, data)
}

const complianceOverviewExportMaxRows = 50000

// ComplianceOverviewExport streams compliance events from all three enforcement
// layers (ai-gateway, compliance-proxy, agent) as CSV. Uses
// ListComplianceAuditEvents which already handles all sources.
func (h *Handler) ComplianceOverviewExport(c echo.Context) error {
	start, end := parseTimeRange(c)
	now := time.Now()
	if start == nil {
		s := now.Add(-24 * time.Hour)
		start = &s
	}
	if end == nil {
		end = &now
	}

	from := &compliancestore.ComplianceAuditParams{
		Start:  start,
		End:    end,
		Limit:  complianceOverviewExportMaxRows,
		Offset: 0,
	}
	data, _, err := h.compliance.ListComplianceAuditEvents(c.Request().Context(), *from)
	if err != nil {
		h.logger.Error("compliance overview export", "error", err)
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
		"timestamp", "id", "transactionId", "source", "sourceIp", "targetHost",
		"method", "path", "statusCode", "hookDecision", "hookReasonCode",
		"bumpStatus", "latencyMs", "complianceTags",
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
			r.Source,
			r.SourceIP,
			r.TargetHost,
			derefStrPtr(r.Method),
			derefStrPtr(r.Path),
			formatOptIntPtr(r.StatusCode),
			derefStrPtr(r.HookDecision),
			derefStrPtr(r.HookReasonCode),
			derefStrPtr(r.BumpStatus),
			formatOptIntPtr(r.LatencyMs),
			strings.Join(r.ComplianceTags, ","),
		}
		if err := w.Write(row); err != nil {
			h.logger.Error("csv write row", "error", err, "id", r.ID)
			return nil
		}
	}
	return nil
}

func derefStrPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func formatOptIntPtr(p *int) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%d", *p)
}
