package siem

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterSIEMRoutes registers SIEM settings routes.
func (h *Handler) RegisterSIEMRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/settings/siem", h.GetSIEMConfig, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	g.PUT("/settings/siem", h.UpdateSIEMConfig, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
	g.POST("/settings/siem/test", h.TestSIEMConfig, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
	g.GET("/settings/siem/event-types", h.ListSIEMEventTypes, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

const siemConfigKey = "siem.config"

// SIEMConfig holds the SIEM integration configuration.
type SIEMConfig struct {
	Enabled    bool              `json:"enabled"`
	URL        string            `json:"url"`
	Format     string            `json:"format"` // json | cef | syslog
	Headers    map[string]string `json:"headers"`
	EventTypes []string          `json:"eventTypes"`
}

func (h *Handler) GetSIEMConfig(c echo.Context) error {
	raw, err := h.db.GetSystemMetadata(c.Request().Context(), siemConfigKey)
	if err != nil {
		h.logger.Error("get siem config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if raw == nil {
		return c.JSON(http.StatusOK, &SIEMConfig{Format: "json"})
	}

	var cfg SIEMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return c.JSON(http.StatusOK, &SIEMConfig{Format: "json"})
	}

	// Mask auth headers
	if cfg.Headers != nil {
		masked := make(map[string]string)
		for k, v := range cfg.Headers {
			if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "x-api-key") {
				if len(v) > 8 {
					masked[k] = v[:4] + "****" + v[len(v)-4:]
				} else {
					masked[k] = "****"
				}
			} else {
				masked[k] = v
			}
		}
		cfg.Headers = masked
	}

	return c.JSON(http.StatusOK, cfg)
}

func (h *Handler) UpdateSIEMConfig(c echo.Context) error {
	var cfg SIEMConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	validFormats := map[string]bool{"json": true, "cef": true, "syslog": true}
	if cfg.Format != "" && !validFormats[cfg.Format] {
		return c.JSON(http.StatusBadRequest, errJSON("format must be json, cef, or syslog", "validation_error", ""))
	}
	if cfg.URL != "" && !strings.HasPrefix(cfg.URL, "https://") && !strings.HasPrefix(cfg.URL, "http://") {
		return c.JSON(http.StatusBadRequest, errJSON("url must be a valid HTTP(S) URL", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := "unknown"
	if aa != nil {
		updatedBy = aa.KeyID
	}

	if err := h.db.SetSystemMetadata(c.Request().Context(), siemConfigKey, cfg, updatedBy); err != nil {
		h.logger.Error("update siem config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// The Hub-side SIEM Bridge re-reads system_metadata['siem.config'] on
	// every Poll cycle, so this DB write propagates within one poll
	// interval (default 30s) without any shadow / NOTIFY plumbing.

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = map[string]any{"enabled": cfg.Enabled, "format": cfg.Format}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) TestSIEMConfig(c echo.Context) error {
	ctx := c.Request().Context()
	raw, err := h.db.GetSystemMetadata(ctx, siemConfigKey)
	if err != nil || raw == nil {
		return c.JSON(http.StatusBadRequest, errJSON("SIEM is not configured", "validation_error", ""))
	}

	var cfg SIEMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.URL == "" {
		return c.JSON(http.StatusBadRequest, errJSON("SIEM URL is not configured", "validation_error", ""))
	}

	testEvent := map[string]any{
		"type":      "siem_test",
		"severity":  "info",
		"timestamp": time.Now().Format(time.RFC3339),
		"message":   "SIEM integration test event from Nexus Gateway",
		"source":    "nexus-control-plane",
	}

	body, _ := json.Marshal(testEvent)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	client := nexushttp.New(nexushttp.Config{
		Timeout:        10 * time.Second,
		Caller:         "cp-admin-siem",
		PropagateReqID: true,
	})
	resp, err := client.Do(req)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "Failed to reach SIEM endpoint: " + err.Error()})
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "SIEM returned HTTP " + resp.Status, "statusCode": resp.StatusCode})
	}

	return c.JSON(http.StatusOK, map[string]any{"ok": true, "statusCode": resp.StatusCode})
}

// ListSIEMEventTypes returns the full set of SIEM event types the
// Control Plane could emit, for the admin filter picker. Each row
// carries Service + Resource + Type so the UI can render a three-level
// drill-down (service → resource → event-type), matching the CatalogPicker
// hierarchy in the IAM policy editor.
//
// Admin (resource.verb) rows derive from the canonical iam.Catalog —
// the same source the IAM engine and audit pipeline use. Traffic.*
// rows are hand-listed because those names come from the SIEM bridge's
// compliance classifier (packages/nexus-hub/internal/observability/siem/classify.go),
// not from the IAM catalog; they are tagged Service "compliance" and
// grouped under a synthetic "traffic" resource so the filter UI shows
// them under Compliance > traffic.
//
// GET /api/admin/settings/siem/event-types
func (h *Handler) ListSIEMEventTypes(c echo.Context) error {
	type eventTypeInfo struct {
		Type     string `json:"type"`
		Resource string `json:"resource"`
		Service  string `json:"service"`
	}

	// Traffic event types — fixed, mirror packages/nexus-hub/internal/observability/siem/classify.go.
	// Service "compliance" because all three are compliance-classifier outputs
	// (rate limit, budget exceeded, request blocked by compliance rule).
	types := []eventTypeInfo{
		{Type: "traffic.rate_limited", Resource: "traffic", Service: string(iam.ServiceCompliance)},
		{Type: "traffic.budget_exceeded", Resource: "traffic", Service: string(iam.ServiceCompliance)},
		{Type: "traffic.request_blocked", Resource: "traffic", Service: string(iam.ServiceCompliance)},
	}

	// Admin event types — one entry per (resource, verb) pair in the
	// canonical catalog. The picker drills service → resource → verb.
	for i := range iam.Catalog {
		r := &iam.Catalog[i]
		for _, v := range r.Verbs {
			types = append(types, eventTypeInfo{
				Type:     iam.SIEMEventType(r.Name, v),
				Resource: r.Name,
				Service:  string(r.Service),
			})
		}
	}

	return c.JSON(http.StatusOK, map[string]any{"eventTypes": types})
}
