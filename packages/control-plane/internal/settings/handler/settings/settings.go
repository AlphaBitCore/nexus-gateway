package settings

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

const settingsMetaKey = "gateway.settings"

var (
	defaultSettings = map[string]any{
		"maintenanceMode":     false,
		"logLevel":            "info",
		"defaultHookTimeout":  5000,
		"defaultFailBehavior": "open",
	}
	startTime = time.Now()
)

func (h *Handler) loadSettings(ctx context.Context) map[string]any {
	result := map[string]any{}
	for k, v := range defaultSettings {
		result[k] = v
	}
	raw, err := h.meta.GetSystemMetadata(ctx, settingsMetaKey)
	if err == nil && raw != nil {
		var stored map[string]any
		if json.Unmarshal(raw, &stored) == nil {
			for k, v := range stored {
				result[k] = v
			}
		}
	}
	return result
}

func (h *Handler) GetSettings(c echo.Context) error {
	settings := h.loadSettings(c.Request().Context())
	settings["uptime"] = int(time.Since(startTime).Seconds())
	ver := os.Getenv("APP_VERSION")
	if ver == "" {
		ver = "dev"
	}
	settings["version"] = ver
	settings["goVersion"] = runtime.Version()
	return c.JSON(http.StatusOK, settings)
}

func (h *Handler) UpdateSettings(c echo.Context) error {
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	ctx := c.Request().Context()
	before := h.loadSettings(ctx)

	current := h.loadSettings(ctx)
	if v, ok := body["maintenanceMode"].(bool); ok {
		current["maintenanceMode"] = v
	}
	if v, ok := body["logLevel"].(string); ok {
		validLevels := map[string]bool{"error": true, "warn": true, "info": true, "debug": true}
		if validLevels[v] {
			current["logLevel"] = v
		}
	}
	if v, ok := body["defaultHookTimeout"].(float64); ok && v > 0 && v <= 30000 {
		current["defaultHookTimeout"] = int(v)
	}
	if v, ok := body["defaultFailBehavior"].(string); ok && (v == "open" || v == "closed") {
		current["defaultFailBehavior"] = v
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.meta.SetSystemMetadata(ctx, settingsMetaKey, current, updatedBy); err != nil {
		h.logger.Error("save settings", "error", err)
		return internalServerError(c, "Failed to save settings")
	}

	afterSnapshot := map[string]any{}
	for k, v := range current {
		afterSnapshot[k] = v
	}

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.BeforeState = before
	ae.AfterState = afterSnapshot
	h.audit.LogObserved(ctx, ae)

	return h.GetSettings(c)
}
