package quota

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/quotastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// validPeriodTypes defines the allowed quota period types.
var validPeriodTypes = map[string]bool{"daily": true, "weekly": true, "monthly": true}

// isMissingOrJSONNull reports whether a json.RawMessage from request binding is
// effectively absent — either the JSON field was omitted entirely (zero-length
// slice) or the caller sent an explicit JSON `null`. Both are treated as
// "client did not supply a value" by the QuotaPolicy create/update paths.
func isMissingOrJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	// Trim ASCII whitespace before matching `null` so `" null "` is also caught.
	trimmed := raw
	for len(trimmed) > 0 {
		switch trimmed[0] {
		case ' ', '\t', '\n', '\r':
			trimmed = trimmed[1:]
		default:
			goto rtrim
		}
	}
rtrim:
	for len(trimmed) > 0 {
		switch trimmed[len(trimmed)-1] {
		case ' ', '\t', '\n', '\r':
			trimmed = trimmed[:len(trimmed)-1]
		default:
			return string(trimmed) == "null"
		}
	}
	return true
}

// RegisterQuotaPolicyRoutes registers quota policy CRUD routes.
func (h *Handler) RegisterQuotaPolicyRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/quota-policies", h.ListQuotaPolicies, iamMW(iam.ResourceQuotaPolicy.Action(iam.VerbRead)))
	g.POST("/quota-policies", h.CreateQuotaPolicy, iamMW(iam.ResourceQuotaPolicy.Action(iam.VerbCreate)))
	g.GET("/quota-policies/:id", h.GetQuotaPolicy, iamMW(iam.ResourceQuotaPolicy.Action(iam.VerbRead)))
	g.PUT("/quota-policies/:id", h.UpdateQuotaPolicy, iamMW(iam.ResourceQuotaPolicy.Action(iam.VerbUpdate)))
	g.DELETE("/quota-policies/:id", h.DeleteQuotaPolicy, iamMW(iam.ResourceQuotaPolicy.Action(iam.VerbDelete)))
}

var validQuotaPolicyScopes = map[string]bool{
	"user":         true,
	"vk":           true,
	"project":      true,
	"organization": true,
}

var validQuotaEnforcementModes = map[string]bool{
	"reject":             true,
	"downgrade":          true,
	"notify-and-proceed": true,
	"track-only":         true,
}

var validQuotaVKTypes = map[string]bool{"personal": true, "application": true}

// validateScopeCombination enforces the legal matrix of (scope × organizationId × vkType)
// defined in the quota-policies-ux-redesign-design §2.
// A nil *string or a pointer to empty string both count as "not set".
func validateScopeCombination(scope string, organizationID, vkType *string) error {
	hasOrg := organizationID != nil && *organizationID != ""
	hasVKType := vkType != nil && *vkType != ""

	switch scope {
	case "organization":
		if !hasOrg {
			return errors.New("organizationId is required when scope=organization")
		}
		if hasVKType {
			return errors.New("vkType must not be set when scope=organization")
		}
	case "user":
		if hasVKType {
			return errors.New("vkType must not be set when scope=user")
		}
	case "project":
		if hasOrg {
			return errors.New("organizationId must not be set when scope=project")
		}
		if hasVKType {
			return errors.New("vkType must not be set when scope=project")
		}
	case "vk":
		if hasOrg {
			return errors.New("organizationId must not be set when scope=vk")
		}
		if !hasVKType {
			return errors.New("vkType is required when scope=vk")
		}
		if !validQuotaVKTypes[*vkType] {
			return errors.New("vkType must be 'personal' or 'application' when scope=vk")
		}
	}
	return nil
}

func (h *Handler) ListQuotaPolicies(c echo.Context) error {
	pg := parsePagination(c)

	var enabled *bool
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		enabled = &t
	} else if v == "false" {
		f := false
		enabled = &f
	}

	params := quotastore.QuotaPolicyListParams{
		Scope:   c.QueryParam("scope"),
		VKType:  c.QueryParam("vkType"),
		Enabled: enabled,
		Q:       c.QueryParam("q"),
		Limit:   pg.Limit,
		Offset:  pg.Offset,
	}

	policies, total, err := h.quota.ListQuotaPolicies(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list quota policies", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list quota policies", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": policies, "total": total})
}

func (h *Handler) GetQuotaPolicy(c echo.Context) error {
	pol, err := h.quota.GetQuotaPolicy(c.Request().Context(), c.Param("id"))
	if err != nil {
		h.logger.Error("get quota policy", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get quota policy", "server_error", ""))
	}
	if pol == nil {
		return c.JSON(http.StatusNotFound, errJSON("Quota policy not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, pol)
}

func (h *Handler) CreateQuotaPolicy(c echo.Context) error {
	var body struct {
		Name            string          `json:"name"`
		Description     *string         `json:"description"`
		Scope           string          `json:"scope"`
		OrganizationID  *string         `json:"organizationId"`
		VKType          *string         `json:"vkType"`
		PeriodType      string          `json:"periodType"`
		CostLimitUsd    *float64        `json:"costLimitUsd"`
		TokenLimit      *int64          `json:"tokenLimit"`
		EnforcementMode string          `json:"enforcementMode"`
		AlertThresholds json.RawMessage `json:"alertThresholds"`
		Priority        int             `json:"priority"`
		Enabled         *bool           `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}
	if body.Scope == "" {
		return c.JSON(http.StatusBadRequest, errJSON("scope is required", "validation_error", ""))
	}
	if !validQuotaPolicyScopes[body.Scope] {
		return c.JSON(http.StatusBadRequest, errJSON("scope must be one of: user, vk, project, organization", "validation_error", ""))
	}
	if body.PeriodType == "" {
		return c.JSON(http.StatusBadRequest, errJSON("periodType is required", "validation_error", ""))
	}
	if !validPeriodTypes[body.PeriodType] {
		return c.JSON(http.StatusBadRequest, errJSON("periodType must be daily, weekly, or monthly", "validation_error", ""))
	}
	if body.EnforcementMode == "" {
		body.EnforcementMode = "reject"
	}
	if !validQuotaEnforcementModes[body.EnforcementMode] {
		return c.JSON(http.StatusBadRequest, errJSON("enforcementMode must be one of: reject, downgrade, notify-and-proceed, track-only", "validation_error", ""))
	}
	if err := validateScopeCombination(body.Scope, body.OrganizationID, body.VKType); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}

	// Default alertThresholds when caller omits the field — QuotaPolicy.alertThresholds
	// is Json NOT NULL with schema default [80, 90]; passing a nil json.RawMessage to the
	// store serialises to SQL NULL and trips a 23502 (not_null_violation). Mirror the
	// Prisma schema default here so the create path is robust to clients that omit the
	// field or send an explicit JSON null.
	if isMissingOrJSONNull(body.AlertThresholds) {
		body.AlertThresholds = json.RawMessage("[80, 90]")
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	var createdBy *string
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		createdBy = &aa.KeyID
	}

	pol, err := h.quota.CreateQuotaPolicy(c.Request().Context(), quotastore.CreateQuotaPolicyParams{
		Name:            body.Name,
		Description:     body.Description,
		Scope:           body.Scope,
		OrganizationID:  body.OrganizationID,
		VKType:          body.VKType,
		PeriodType:      body.PeriodType,
		CostLimitUsd:    body.CostLimitUsd,
		TokenLimit:      body.TokenLimit,
		EnforcementMode: body.EnforcementMode,
		AlertThresholds: body.AlertThresholds,
		Priority:        body.Priority,
		Enabled:         enabled,
		CreatedBy:       createdBy,
	})
	if err != nil {
		h.logger.Error("create quota policy", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create quota policy", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaPolicy, iam.VerbCreate)
	ae.EntityID = pol.ID
	ae.AfterState = pol
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "quota_policies")
	}

	return c.JSON(http.StatusCreated, pol)
}

func (h *Handler) UpdateQuotaPolicy(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.quota.GetQuotaPolicy(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get quota policy for update", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get quota policy", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Quota policy not found", "not_found", ""))
	}

	var body struct {
		Name            *string         `json:"name"`
		Description     *string         `json:"description"`
		Scope           *string         `json:"scope"`
		OrganizationID  *string         `json:"organizationId"`
		VKType          *string         `json:"vkType"`
		PeriodType      *string         `json:"periodType"`
		CostLimitUsd    *float64        `json:"costLimitUsd"`
		TokenLimit      *int64          `json:"tokenLimit"`
		EnforcementMode *string         `json:"enforcementMode"`
		AlertThresholds json.RawMessage `json:"alertThresholds"`
		Priority        *int            `json:"priority"`
		Enabled         *bool           `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Scope != nil && !validQuotaPolicyScopes[*body.Scope] {
		return c.JSON(http.StatusBadRequest, errJSON("scope must be one of: user, vk, project, organization", "validation_error", ""))
	}
	if body.PeriodType != nil && !validPeriodTypes[*body.PeriodType] {
		return c.JSON(http.StatusBadRequest, errJSON("periodType must be daily, weekly, or monthly", "validation_error", ""))
	}
	if body.EnforcementMode != nil && !validQuotaEnforcementModes[*body.EnforcementMode] {
		return c.JSON(http.StatusBadRequest, errJSON("enforcementMode must be one of: reject, downgrade, notify-and-proceed, track-only", "validation_error", ""))
	}

	// Validate the merged effective state (existing row + partial update).
	effScope := existing.Scope
	if body.Scope != nil {
		effScope = *body.Scope
	}
	effOrgID := existing.OrganizationID
	if body.OrganizationID != nil {
		effOrgID = body.OrganizationID
	}
	effVKType := existing.VKType
	if body.VKType != nil {
		effVKType = body.VKType
	}
	if err := validateScopeCombination(effScope, effOrgID, effVKType); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}

	// Normalise alertThresholds for the COALESCE-based update: an omitted field or
	// explicit JSON `null` means "no change" and must reach the store as a true Go
	// nil so the SQL `$11` parameter is NULL and COALESCE retains the existing row
	// value. (json.RawMessage("null") would otherwise be sent as the literal bytes
	// `null` and could violate the NOT NULL constraint depending on driver coercion.)
	if isMissingOrJSONNull(body.AlertThresholds) {
		body.AlertThresholds = nil
	}

	updated, err := h.quota.UpdateQuotaPolicy(c.Request().Context(), id, quotastore.UpdateQuotaPolicyParams{
		Name:            body.Name,
		Description:     body.Description,
		Scope:           body.Scope,
		OrganizationID:  body.OrganizationID,
		VKType:          body.VKType,
		PeriodType:      body.PeriodType,
		CostLimitUsd:    body.CostLimitUsd,
		TokenLimit:      body.TokenLimit,
		EnforcementMode: body.EnforcementMode,
		AlertThresholds: body.AlertThresholds,
		Priority:        body.Priority,
		Enabled:         body.Enabled,
	})
	if err != nil {
		h.logger.Error("update quota policy", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update quota policy", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaPolicy, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "quota_policies")
	}

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteQuotaPolicy(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.quota.GetQuotaPolicy(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get quota policy for delete", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get quota policy", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Quota policy not found", "not_found", ""))
	}

	if err := h.quota.DeleteQuotaPolicy(c.Request().Context(), id); err != nil {
		h.logger.Error("delete quota policy", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete quota policy", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceQuotaPolicy, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "quota_policies")
	}

	return c.NoContent(http.StatusNoContent)
}
