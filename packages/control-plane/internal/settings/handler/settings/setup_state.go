package settings

import (
	"github.com/goccy/go-json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// GetSetupState returns the current setup wizard completion state from
// system_metadata["setup-wizard-state"].
func (h *Handler) GetSetupState(c echo.Context) error {
	raw, _ := h.meta.GetSystemMetadata(c.Request().Context(), "setup-wizard-state")
	if raw == nil {
		return c.JSON(http.StatusOK, map[string]any{"completed": false})
	}
	var state any
	_ = json.Unmarshal(raw, &state)
	return c.JSON(http.StatusOK, state)
}

// UpdateSetupState writes the setup wizard completion state to
// system_metadata["setup-wizard-state"] and emits an audit entry.
func (h *Handler) UpdateSetupState(c echo.Context) error {
	var body any
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid body", "validation_error", ""))
	}
	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.meta.SetSystemMetadata(c.Request().Context(), "setup-wizard-state", body, updatedBy); err != nil {
		h.logger.Error("save setup state", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to save setup state", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = body
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, body)
}
