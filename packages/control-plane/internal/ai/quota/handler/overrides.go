package quota

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/quotastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterQuotaOverrideRoutes registers quota override CRUD routes.
func (h *Handler) RegisterQuotaOverrideRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/quota-overrides", h.ListQuotaOverrides, iamMW(iam.ResourceQuotaOverride.Action(iam.VerbRead)))
	g.POST("/quota-overrides", h.CreateQuotaOverride, iamMW(iam.ResourceQuotaOverride.Action(iam.VerbCreate)))
	g.GET("/quota-overrides/:id", h.GetQuotaOverride, iamMW(iam.ResourceQuotaOverride.Action(iam.VerbRead)))
	g.PUT("/quota-overrides/:id", h.UpdateQuotaOverride, iamMW(iam.ResourceQuotaOverride.Action(iam.VerbUpdate)))
	g.DELETE("/quota-overrides/:id", h.DeleteQuotaOverride, iamMW(iam.ResourceQuotaOverride.Action(iam.VerbDelete)))
}

var validOverrideTargetTypes = map[string]bool{
	"user":         true,
	"vk":           true,
	"project":      true,
	"organization": true,
}

func (h *Handler) ListQuotaOverrides(c echo.Context) error {
	pg := parsePagination(c)
	params := quotastore.QuotaOverrideListParams{
		TargetType: c.QueryParam("targetType"),
		Q:          c.QueryParam("q"),
		Limit:      pg.Limit,
		Offset:     pg.Offset,
	}

	overrides, total, err := h.quota.ListQuotaOverrides(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list quota overrides", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list quota overrides", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": overrides, "total": total})
}

func (h *Handler) GetQuotaOverride(c echo.Context) error {
	o, err := h.quota.GetQuotaOverride(c.Request().Context(), c.Param("id"))
	if err != nil {
		h.logger.Error("get quota override", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get quota override", "server_error", ""))
	}
	if o == nil {
		return c.JSON(http.StatusNotFound, errJSON("Quota override not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, o)
}

func (h *Handler) CreateQuotaOverride(c echo.Context) error {
	var body struct {
		TargetType      string   `json:"targetType"`
		TargetID        string   `json:"targetId"`
		Reason          *string  `json:"reason"`
		CostLimitUsd    *float64 `json:"costLimitUsd"`
		TokenLimit      *int64   `json:"tokenLimit"`
		EnforcementMode *string  `json:"enforcementMode"`
		PeriodType      *string  `json:"periodType"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.TargetType == "" {
		return c.JSON(http.StatusBadRequest, errJSON("targetType is required", "validation_error", ""))
	}
	if !validOverrideTargetTypes[body.TargetType] {
		return c.JSON(http.StatusBadRequest, errJSON("targetType must be one of: user, vk, project, organization", "validation_error", ""))
	}
	if body.TargetID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("targetId is required", "validation_error", ""))
	}
	if body.EnforcementMode != nil && !validQuotaEnforcementModes[*body.EnforcementMode] {
		return c.JSON(http.StatusBadRequest, errJSON("enforcementMode must be one of: reject, downgrade, notify-and-proceed, track-only", "validation_error", ""))
	}
	if body.PeriodType != nil && !validPeriodTypes[*body.PeriodType] {
		return c.JSON(http.StatusBadRequest, errJSON("periodType must be daily, weekly, or monthly", "validation_error", ""))
	}

	var createdBy *string
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		createdBy = &aa.KeyID
	}

	o, err := h.quota.CreateQuotaOverride(c.Request().Context(), quotastore.CreateQuotaOverrideParams{
		TargetType:      body.TargetType,
		TargetID:        body.TargetID,
		Reason:          body.Reason,
		CostLimitUsd:    body.CostLimitUsd,
		TokenLimit:      body.TokenLimit,
		EnforcementMode: body.EnforcementMode,
		PeriodType:      body.PeriodType,
		CreatedBy:       createdBy,
	})
	if err != nil {
		if errors.Is(err, quotastore.ErrQuotaOverrideConflict) {
			return c.JSON(http.StatusConflict, errJSON("A quota override already exists for this target", "conflict", "quota_override_conflict"))
		}
		h.logger.Error("create quota override", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create quota override", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaOverride, iam.VerbCreate)
	ae.EntityID = o.ID
	ae.AfterState = o
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "quota_overrides")
	}

	return c.JSON(http.StatusCreated, o)
}

func (h *Handler) UpdateQuotaOverride(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.quota.GetQuotaOverride(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get quota override for update", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get quota override", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Quota override not found", "not_found", ""))
	}

	var body struct {
		Reason          *string  `json:"reason"`
		CostLimitUsd    *float64 `json:"costLimitUsd"`
		TokenLimit      *int64   `json:"tokenLimit"`
		EnforcementMode *string  `json:"enforcementMode"`
		PeriodType      *string  `json:"periodType"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.EnforcementMode != nil && !validQuotaEnforcementModes[*body.EnforcementMode] {
		return c.JSON(http.StatusBadRequest, errJSON("enforcementMode must be one of: reject, downgrade, notify-and-proceed, track-only", "validation_error", ""))
	}
	if body.PeriodType != nil && !validPeriodTypes[*body.PeriodType] {
		return c.JSON(http.StatusBadRequest, errJSON("periodType must be daily, weekly, or monthly", "validation_error", ""))
	}

	updated, err := h.quota.UpdateQuotaOverride(c.Request().Context(), id, quotastore.UpdateQuotaOverrideParams{
		Reason:          body.Reason,
		CostLimitUsd:    body.CostLimitUsd,
		TokenLimit:      body.TokenLimit,
		EnforcementMode: body.EnforcementMode,
		PeriodType:      body.PeriodType,
	})
	if err != nil {
		h.logger.Error("update quota override", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update quota override", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaOverride, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "quota_overrides")
	}

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteQuotaOverride(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.quota.GetQuotaOverride(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get quota override for delete", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get quota override", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Quota override not found", "not_found", ""))
	}

	if err := h.quota.DeleteQuotaOverride(c.Request().Context(), id); err != nil {
		h.logger.Error("delete quota override", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete quota override", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaOverride, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "quota_overrides")
	}

	return c.NoContent(http.StatusNoContent)
}
