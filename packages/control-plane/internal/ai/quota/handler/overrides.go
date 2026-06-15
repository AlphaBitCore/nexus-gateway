package quota

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/quotastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
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
		TargetType      string     `json:"targetType"`
		TargetID        string     `json:"targetId"`
		Reason          *string    `json:"reason"`
		CostLimitUsd    *float64   `json:"costLimitUsd"`
		EnforcementMode *string    `json:"enforcementMode"`
		PeriodType      *string    `json:"periodType"`
		ExpiresAt       *time.Time `json:"expiresAt"`
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
	// A cost cap <= 0 is rejected (the engine treats it as UNLIMITED). A nil cap
	// is permitted: it means "inherit the governing policy's cost cap" via the
	// engine's override→policy fallback — but only when the override still
	// customises enforcement or period, otherwise it overrides nothing.
	if body.CostLimitUsd != nil && *body.CostLimitUsd <= 0 {
		return c.JSON(http.StatusBadRequest, errJSON("costLimitUsd must be greater than 0", "validation_error", ""))
	}
	if body.CostLimitUsd == nil && body.EnforcementMode == nil && body.PeriodType == nil {
		return c.JSON(http.StatusBadRequest, errJSON("at least one of costLimitUsd, enforcementMode, or periodType must be set", "validation_error", ""))
	}
	// An expiry, when supplied, must be in the future: a temporary exception that
	// is born already-expired would be silently ignored by the engine.
	if body.ExpiresAt != nil && !body.ExpiresAt.After(time.Now()) {
		return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be in the future", "validation_error", ""))
	}
	// Referential validation: a typo'd targetId would create an override
	// row that matches no level of the enforcement chain and silently enforces
	// nothing. Reject up front so the admin learns the target is wrong.
	exists, lookupErr := h.targetEntityExists(c.Request().Context(), body.TargetType, body.TargetID)
	if lookupErr != nil {
		h.logger.Error("create quota override: target existence check failed", "targetType", body.TargetType, "error", lookupErr)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to validate target", "server_error", ""))
	}
	if !exists {
		return c.JSON(http.StatusBadRequest, errJSON(
			fmt.Sprintf("targetId %q does not reference an existing %s", body.TargetID, body.TargetType),
			"validation_error", ""))
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
		EnforcementMode: body.EnforcementMode,
		PeriodType:      body.PeriodType,
		ExpiresAt:       body.ExpiresAt,
		CreatedBy:       createdBy,
	})
	if err != nil {
		if errors.Is(err, quotastore.ErrQuotaOverrideConflict) {
			return c.JSON(http.StatusConflict, errJSON("A quota override already exists for this target", "conflict", "quota_override_conflict"))
		}
		h.logger.Error("create quota override", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create quota override", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "quota_overrides"); err != nil {
			h.logger.Error("create quota override: hub invalidate failed", "id", o.ID, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaOverride, iam.VerbCreate)
	ae.EntityID = o.ID
	ae.AfterState = o
	h.audit.LogObserved(c.Request().Context(), ae)

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
		Reason          *string    `json:"reason"`
		CostLimitUsd    *float64   `json:"costLimitUsd"`
		CostLimitMode   *string    `json:"costLimitMode"`
		EnforcementMode *string    `json:"enforcementMode"`
		PeriodType      *string    `json:"periodType"`
		ExpiresAt       *time.Time `json:"expiresAt"`
		ExpiresAtMode   *string    `json:"expiresAtMode"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	// The edit form owns cost/mode/period in full: the sentinel "_inherit"
	// resets a column to NULL so the engine falls back to the matching policy,
	// a real value sets it, and an omitted field is left untouched. Without the
	// explicit clear path, switching a populated override back to Inherit would
	// silently no-op (a nil value reads as "keep" through the store's COALESCE).
	// Cost is numeric so it carries its clear intent in costLimitMode rather
	// than the value field.
	const inheritSentinel = "_inherit"
	clearCost := body.CostLimitMode != nil && *body.CostLimitMode == inheritSentinel
	clearMode := body.EnforcementMode != nil && *body.EnforcementMode == inheritSentinel
	clearPeriod := body.PeriodType != nil && *body.PeriodType == inheritSentinel
	// Expiry is a timestamp, so it carries its clear intent in expiresAtMode
	// ("_inherit" → restore the override to never-expires) rather than the value.
	clearExpiry := body.ExpiresAtMode != nil && *body.ExpiresAtMode == inheritSentinel
	if clearCost {
		body.CostLimitUsd = nil
	}
	if clearMode {
		body.EnforcementMode = nil
	}
	if clearPeriod {
		body.PeriodType = nil
	}
	if clearExpiry {
		body.ExpiresAt = nil
	}
	if body.EnforcementMode != nil && !validQuotaEnforcementModes[*body.EnforcementMode] {
		return c.JSON(http.StatusBadRequest, errJSON("enforcementMode must be one of: reject, downgrade, notify-and-proceed, track-only", "validation_error", ""))
	}
	if body.PeriodType != nil && !validPeriodTypes[*body.PeriodType] {
		return c.JSON(http.StatusBadRequest, errJSON("periodType must be daily, weekly, or monthly", "validation_error", ""))
	}
	// A cost cap <= 0 is rejected (engine treats it as UNLIMITED). The merged
	// override must still customise at least one governing field; a nil cost cap
	// inherits the policy's cap, so it is valid only alongside an enforcement or
	// period override.
	if body.CostLimitUsd != nil && *body.CostLimitUsd <= 0 {
		return c.JSON(http.StatusBadRequest, errJSON("costLimitUsd must be greater than 0", "validation_error", ""))
	}
	// A newly-set expiry must be in the future (an already-expired exception would
	// be silently ignored by the engine). Clearing it (Inherit) is always valid.
	if !clearExpiry && body.ExpiresAt != nil && !body.ExpiresAt.After(time.Now()) {
		return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be in the future", "validation_error", ""))
	}
	// Effective post-update state, honouring set / clear / keep per column.
	effCost := existing.CostLimitUsd
	if clearCost {
		effCost = nil
	} else if body.CostLimitUsd != nil {
		effCost = body.CostLimitUsd
	}
	effMode := existing.EnforcementMode
	if clearMode {
		effMode = nil
	} else if body.EnforcementMode != nil {
		effMode = body.EnforcementMode
	}
	effPeriod := existing.PeriodType
	if clearPeriod {
		effPeriod = nil
	} else if body.PeriodType != nil {
		effPeriod = body.PeriodType
	}
	if effCost == nil && effMode == nil && effPeriod == nil {
		return c.JSON(http.StatusBadRequest, errJSON("at least one of costLimitUsd, enforcementMode, or periodType must be set", "validation_error", ""))
	}

	updated, err := h.quota.UpdateQuotaOverride(c.Request().Context(), id, quotastore.UpdateQuotaOverrideParams{
		Reason:               body.Reason,
		CostLimitUsd:         body.CostLimitUsd,
		ClearCostLimit:       clearCost,
		EnforcementMode:      body.EnforcementMode,
		ClearEnforcementMode: clearMode,
		PeriodType:           body.PeriodType,
		ClearPeriodType:      clearPeriod,
		ExpiresAt:            body.ExpiresAt,
		ClearExpiresAt:       clearExpiry,
	})
	if err != nil {
		h.logger.Error("update quota override", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update quota override", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "quota_overrides"); err != nil {
			h.logger.Error("update quota override: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaOverride, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

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

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "quota_overrides"); err != nil {
			h.logger.Error("delete quota override: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaOverride, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}
