package providers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/modelstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterModelRoutes registers model CRUD routes.
func (h *Handler) RegisterModelRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/models", h.ListModelsGrouped, iamMW(iam.ResourceModel.Action(iam.VerbRead)))
	g.GET("/models/flat", h.ListModelsFlat, iamMW(iam.ResourceModel.Action(iam.VerbRead)))
	g.GET("/models/:id", h.GetModel, iamMW(iam.ResourceModel.Action(iam.VerbRead)))
	g.PUT("/models/:id", h.UpdateModel, iamMW(iam.ResourceModel.Action(iam.VerbUpdate)))
	g.DELETE("/models/:id", h.DeleteModel, iamMW(iam.ResourceModel.Action(iam.VerbDelete)))
}

func (h *Handler) ListModelsGrouped(c echo.Context) error {
	params := modelstore.GroupedModelsParams{
		IncludeEmpty: c.QueryParam("includeEmptyProviders") == "true",
		ProviderID:   c.QueryParam("providerId"),
		Q:            c.QueryParam("q"),
	}
	groups, err := h.models.ListModelsGroupedByProvider(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list models grouped", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": groups})
}

func (h *Handler) ListModelsFlat(c echo.Context) error {
	pg := parsePagination(c)
	params := modelstore.ModelListParams{
		Q:          c.QueryParam("q"),
		Type:       c.QueryParam("type"),
		Status:     c.QueryParam("status"),
		ProviderID: c.QueryParam("providerId"),
		Limit:      pg.Limit,
		Offset:     pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	models, total, err := h.models.ListModelsFlat(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list models", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": models, "total": total})
}

func (h *Handler) GetModel(c echo.Context) error {
	m, err := h.models.GetModel(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if m == nil {
		return c.JSON(http.StatusNotFound, errJSON("Model not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, m)
}

var validModelTypes = map[string]bool{"chat": true, "embedding": true, "image": true, "audio": true}

func (h *Handler) UpdateModel(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.models.GetModel(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Model not found", "not_found", ""))
	}

	var body struct {
		Code                           *string          `json:"code"`
		ProviderModelID                *string          `json:"providerModelId"`
		Name                           *string          `json:"name"`
		Description                    *string          `json:"description"`
		Type                           *string          `json:"type"`
		InputPricePerMillion           *float64         `json:"inputPricePerMillion"`
		OutputPricePerMillion          *float64         `json:"outputPricePerMillion"`
		CachedInputReadPricePerMillion *float64         `json:"cachedInputReadPricePerMillion"`
		CachedInputWritePricePerMillion *float64        `json:"cachedInputWritePricePerMillion"`
		MaxContextTokens               *int             `json:"maxContextTokens"`
		MaxOutputTokens                *int             `json:"maxOutputTokens"`
		Status                         *string          `json:"status"`
		DeprecationDate                *time.Time       `json:"deprecationDate"`
		ReplacedBy                     *string          `json:"replacedBy"`
		Aliases                        []string         `json:"aliases"`
		Enabled                        *bool            `json:"enabled"`
		Features                       []string         `json:"features"`
		CapabilityJson                 *json.RawMessage `json:"capabilityJson"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	if body.Type != nil && !validModelTypes[*body.Type] {
		return c.JSON(http.StatusBadRequest, errJSON("type must be one of: chat, embedding, image, audio", "validation_error", ""))
	}

	if body.Code != nil && *body.Code == "" {
		return c.JSON(http.StatusBadRequest, errJSON("code must not be empty", "validation_error", ""))
	}
	if body.ProviderModelID != nil && *body.ProviderModelID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("providerModelId must not be empty", "validation_error", ""))
	}

	// Validate capabilityJson structure when provided. A well-formed
	// document must round-trip through JSON; reject outright-invalid JSON
	// before it reaches the DB JSONB column.
	if body.CapabilityJson != nil {
		var cap map[string]json.RawMessage
		if err := json.Unmarshal(*body.CapabilityJson, &cap); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("capabilityJson must be a valid JSON object", "validation_error", ""))
		}
	}

	params := modelstore.UpdateModelParams{
		Code:                           body.Code,
		ProviderModelID:                body.ProviderModelID,
		Name:                           body.Name,
		Description:                    body.Description,
		Type:                           body.Type,
		InputPricePerMillion:           body.InputPricePerMillion,
		OutputPricePerMillion:          body.OutputPricePerMillion,
		CachedInputReadPricePerMillion:  body.CachedInputReadPricePerMillion,
		CachedInputWritePricePerMillion: body.CachedInputWritePricePerMillion,
		MaxContextTokens:               body.MaxContextTokens,
		MaxOutputTokens:                body.MaxOutputTokens,
		Status:                         body.Status,
		DeprecationDate:                body.DeprecationDate,
		ReplacedBy:                     body.ReplacedBy,
		Aliases:                        body.Aliases,
		Enabled:                        body.Enabled,
		Features:                       body.Features,
		CapabilityJson:                 body.CapabilityJson,
	}

	updated, err := h.models.UpdateModel(c.Request().Context(), id, params)
	if err != nil {
		h.logger.Error("update model", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "models")
	}

	ae := audit.EntryFor(c, iam.ResourceModel, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteModel(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.models.GetModel(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Model not found", "not_found", ""))
	}

	if err := h.models.DeleteModel(c.Request().Context(), id); err != nil {
		h.logger.Error("delete model", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "models")
	}

	ae := audit.EntryFor(c, iam.ResourceModel, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}
