package hooks

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/hookstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func (h *Handler) ListHookConfigs(c echo.Context) error {
	pg := parsePagination(c)
	params := hookstore.HookConfigListParams{
		Q:        c.QueryParam("q"),
		Pipeline: c.QueryParam("pipeline"),
		Limit:    pg.Limit,
		Offset:   pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	hookList, total, err := h.hooks.ListHookConfigs(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list hook configs", "error", err)
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, map[string]any{"data": enrichHooks(hookList), "total": total})
}

func (h *Handler) GetHookConfig(c echo.Context) error {
	hc, err := h.hooks.GetHookConfig(c.Request().Context(), c.Param("id"))
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	if hc == nil {
		return c.JSON(http.StatusNotFound, errJSON("Hook config not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, enrichHook(hc))
}

func (h *Handler) CreateHookConfig(c echo.Context) error {
	var body struct {
		Name              string          `json:"name"`
		Type              string          `json:"type"`
		ImplementationID  string          `json:"implementationId"`
		Stage             string          `json:"stage"`
		Category          *string         `json:"category"`
		Endpoint          *string         `json:"endpoint"`
		Script            *string         `json:"script"`
		Config            json.RawMessage `json:"config"`
		Priority          int             `json:"priority"`
		TimeoutMs         int             `json:"timeoutMs"`
		FailBehavior      string          `json:"failBehavior"`
		Enabled           *bool           `json:"enabled"`
		ApplicableIngress []string        `json:"applicableIngress"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" || body.Type == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name and type are required", "validation_error", ""))
	}

	if body.ImplementationID == "" {
		body.ImplementationID = "noop"
	}
	if body.Stage == "" {
		body.Stage = "request"
	}
	if body.TimeoutMs == 0 {
		body.TimeoutMs = 5000
	}
	if body.FailBehavior == "" {
		body.FailBehavior = "fail-open"
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if msg := ValidateHookEnums(body.Stage, body.FailBehavior, body.Type, body.ImplementationID); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	// Empty-slice guard: a client that serialized "applicableIngress: []"
	// would otherwise persist an array that matches no ingress type, making
	// the hook a no-op on every pipeline. Reject loudly so the client can
	// omit the field (→ DB default) or send explicit codes.
	if body.ApplicableIngress != nil && len(body.ApplicableIngress) == 0 {
		return c.JSON(http.StatusBadRequest, errJSON(
			"applicableIngress must not be empty (omit the field to default to ALL)",
			"validation_error", "applicableIngress"))
	}

	hc, err := h.hooks.CreateHookConfig(c.Request().Context(), hookstore.CreateHookConfigParams{
		Name:              body.Name,
		Type:              body.Type,
		ImplementationID:  body.ImplementationID,
		Stage:             body.Stage,
		Category:          body.Category,
		Endpoint:          body.Endpoint,
		Script:            body.Script,
		Config:            body.Config,
		Priority:          body.Priority,
		TimeoutMs:         body.TimeoutMs,
		FailBehavior:      body.FailBehavior,
		Enabled:           enabled,
		ApplicableIngress: body.ApplicableIngress,
	})
	if err != nil {
		h.logger.Error("create hook config", "error", err)
		return internalServerError(c, "Internal server error")
	}

	h.invalidateHookConfigEverywhere(c.Request().Context())
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceHook, iam.VerbCreate)
	ae.EntityID = hc.ID
	ae.AfterState = hc
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, enrichHook(hc))
}

func (h *Handler) UpdateHookConfig(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.hooks.GetHookConfig(c.Request().Context(), id)
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Hook config not found", "not_found", ""))
	}

	var body struct {
		Name              *string  `json:"name"`
		Type              *string  `json:"type"`
		ImplementationID  *string  `json:"implementationId"`
		Stage             *string  `json:"stage"`
		Category          *string  `json:"category"`
		Endpoint          *string  `json:"endpoint"`
		Script            *string  `json:"script"`
		Config            any      `json:"config"`
		Priority          *int     `json:"priority"`
		TimeoutMs         *int     `json:"timeoutMs"`
		FailBehavior      *string  `json:"failBehavior"`
		Enabled           *bool    `json:"enabled"`
		ApplicableIngress []string `json:"applicableIngress"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	// nil-safe deref so "field omitted" stays equivalent to empty string.
	stage := deref(body.Stage)
	failBehavior := deref(body.FailBehavior)
	hookType := deref(body.Type)
	implID := deref(body.ImplementationID)
	if msg := ValidateHookEnums(stage, failBehavior, hookType, implID); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	if body.ApplicableIngress != nil && len(body.ApplicableIngress) == 0 {
		return c.JSON(http.StatusBadRequest, errJSON(
			"applicableIngress must not be empty (omit the field to keep the current value)",
			"validation_error", "applicableIngress"))
	}

	params := hookstore.UpdateHookConfigParams{
		Name:              body.Name,
		Type:              body.Type,
		ImplementationID:  body.ImplementationID,
		Stage:             body.Stage,
		Category:          body.Category,
		Endpoint:          body.Endpoint,
		Script:            body.Script,
		Priority:          body.Priority,
		TimeoutMs:         body.TimeoutMs,
		FailBehavior:      body.FailBehavior,
		Enabled:           body.Enabled,
		ApplicableIngress: body.ApplicableIngress,
	}
	if body.Config != nil {
		raw, err := json.Marshal(body.Config)
		if err != nil {
			// Swallowing this silently used to persist NULL / no-change — a
			// hand-crafted config (bad numbers, unsupported types) would
			// disappear with a 200 OK, hiding the error from the operator.
			return c.JSON(http.StatusBadRequest, errJSON(
				"config must be JSON-serializable: "+err.Error(),
				"validation_error", "config"))
		}
		params.Config = raw
	}

	// If nothing was provided, return existing unchanged
	if body.Name == nil && body.Type == nil && body.ImplementationID == nil &&
		body.Stage == nil && body.Category == nil && body.Endpoint == nil &&
		body.Script == nil && body.Config == nil && body.Priority == nil &&
		body.TimeoutMs == nil && body.FailBehavior == nil && body.Enabled == nil &&
		body.ApplicableIngress == nil {
		return c.JSON(http.StatusOK, enrichHook(existing))
	}

	updated, err := h.hooks.UpdateHookConfig(c.Request().Context(), id, params)
	if err != nil {
		h.logger.Error("update hook config", "error", err)
		return internalServerError(c, "Internal server error")
	}

	h.invalidateHookConfigEverywhere(c.Request().Context())
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceHook, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, enrichHook(updated))
}

func (h *Handler) DeleteHookConfig(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.hooks.GetHookConfig(c.Request().Context(), id)
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Hook config not found", "not_found", ""))
	}

	if err := h.hooks.DeleteHookConfig(c.Request().Context(), id); err != nil {
		return internalServerError(c, "Internal server error")
	}

	h.invalidateHookConfigEverywhere(c.Request().Context())
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceHook, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) ReorderHooks(c echo.Context) error {
	var body struct {
		Stage string   `json:"stage"`
		IDs   []string `json:"ids"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Stage == "" || len(body.IDs) == 0 {
		return c.JSON(http.StatusBadRequest, errJSON("stage and ids are required", "validation_error", ""))
	}

	if err := h.hooks.ReorderHooksByStage(c.Request().Context(), body.Stage, body.IDs); err != nil {
		h.logger.Error("reorder hooks", "error", err)
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}

	h.invalidateHookConfigEverywhere(c.Request().Context())
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceHook, iam.VerbUpdate)
	ae.EntityID = body.Stage
	ae.AfterState = map[string]any{"stage": body.Stage, "ids": body.IDs}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true, "stage": body.Stage, "count": len(body.IDs)})
}

// HookForceRefresh publishes a Hub invalidation event to force all
// data-plane services to reload hook configs immediately.
func (h *Handler) HookForceRefresh(c echo.Context) error {
	h.invalidateHookConfigEverywhere(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceHook, iam.VerbUpdate)
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"refreshed": true})
}
