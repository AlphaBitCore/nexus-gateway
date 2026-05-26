package dsar

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/dsar/dsarstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterDSARRoutes registers DSAR (Data Subject Access Request) routes.
// All DSAR mutations gate on the canonical admin:dsar.<verb> action — see
// shared/iam.Catalog "dsar" row. Previously the writes shared the read-only
// admin:audit-log.read gate, which let any read-only viewer fulfill DSARs.
func (h *Handler) RegisterDSARRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/dsar", h.ListDSAR, iamMW(iam.ResourceDSAR.Action(iam.VerbRead)))
	g.POST("/dsar", h.CreateDSAR, iamMW(iam.ResourceDSAR.Action(iam.VerbCreate)))
	g.GET("/dsar/:id", h.GetDSAR, iamMW(iam.ResourceDSAR.Action(iam.VerbRead)))
	g.PUT("/dsar/:id", h.UpdateDSAR, iamMW(iam.ResourceDSAR.Action(iam.VerbUpdate)))
	g.POST("/dsar/:id/fulfill", h.FulfillDSAR, iamMW(iam.ResourceDSAR.Action(iam.VerbFulfill)))
}

func (h *Handler) ListDSAR(c echo.Context) error {
	pg := parsePagination(c)
	status := c.QueryParam("status")
	requests, total, err := h.db.ListDSARRequests(c.Request().Context(), status, pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("list dsar", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"requests": requests, "total": total, "limit": pg.Limit, "offset": pg.Offset})
}

func (h *Handler) GetDSAR(c echo.Context) error {
	req, err := h.db.GetDSARRequest(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if req == nil {
		return c.JSON(http.StatusNotFound, errJSON("DSAR request not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, req)
}

func (h *Handler) CreateDSAR(c echo.Context) error {
	var body struct {
		SubjectID string  `json:"subjectId"`
		Contact   *string `json:"contact"`
		Type      string  `json:"type"` // ACCESS | ERASURE
		Notes     *string `json:"notes"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.SubjectID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("subjectId is required", "validation_error", ""))
	}
	if body.Type != "ACCESS" && body.Type != "ERASURE" {
		return c.JSON(http.StatusBadRequest, errJSON("type must be ACCESS or ERASURE", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyID
	}

	req, err := h.db.CreateDSARRequest(c.Request().Context(), dsarstore.CreateDSARRequestParams{
		SubjectID: body.SubjectID,
		Contact:   body.Contact,
		Type:      body.Type,
		Notes:     body.Notes,
		CreatedBy: createdBy,
	})
	if err != nil {
		h.logger.Error("create dsar", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDSAR, iam.VerbCreate)
	ae.EntityID = req.ID
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, req)
}

var validDSARTransitions = map[string]map[string]bool{
	"PENDING":     {"IN_PROGRESS": true, "REJECTED": true},
	"IN_PROGRESS": {"COMPLETED": true, "REJECTED": true},
}

func (h *Handler) UpdateDSAR(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.db.GetDSARRequest(c.Request().Context(), id)
	if err != nil || existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("DSAR request not found", "not_found", ""))
	}

	var body struct {
		Notes  *string `json:"notes"`
		Status string  `json:"status"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := "unknown"
	if aa != nil {
		updatedBy = aa.KeyID
	}

	params := dsarstore.UpdateDSARParams{
		UpdatedBy: &updatedBy,
	}
	if body.Notes != nil {
		params.Notes = body.Notes
	}
	if body.Status != "" {
		transitions := validDSARTransitions[existing.Status]
		if transitions == nil || !transitions[body.Status] {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid status transition", "validation_error", ""))
		}
		params.Status = &body.Status
		if body.Status == "COMPLETED" || body.Status == "REJECTED" {
			now := time.Now().UTC()
			params.CompletedAt = &now
		}
	}

	updated, err := h.db.UpdateDSARRequest(c.Request().Context(), id, params)
	if err != nil {
		h.logger.Error("update dsar", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDSAR, iam.VerbUpdate)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) FulfillDSAR(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	existing, err := h.db.GetDSARRequest(ctx, id)
	if err != nil || existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("DSAR request not found", "not_found", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := "unknown"
	if aa != nil {
		updatedBy = aa.KeyID
	}

	if existing.Type == "ACCESS" {
		export, err := h.db.FulfillDSARAccess(ctx, existing.SubjectID)
		if err != nil {
			h.logger.Error("dsar access fulfill", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Fulfillment failed", "server_error", ""))
		}

		outcomeJSON, _ := json.Marshal(export)
		now := time.Now().UTC()
		completedStatus := "COMPLETED"
		updated, err := h.db.UpdateDSARRequest(ctx, id, dsarstore.UpdateDSARParams{
			Status:      &completedStatus,
			CompletedAt: &now,
			Outcome:     outcomeJSON,
			UpdatedBy:   &updatedBy,
		})
		if err != nil {
			h.logger.Error("dsar update after access", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to update DSAR status", "server_error", ""))
		}

		ae := audit.EntryFor(c, iam.ResourceDSAR, iam.VerbFulfill)
		ae.EntityID = id
		ae.AfterState = map[string]any{"type": "ACCESS", "export": export}
		h.audit.LogObserved(ctx, ae)

		return c.JSON(http.StatusOK, map[string]any{"request": updated, "export": export})
	}

	// ERASURE
	result, err := h.db.FulfillDSARErasure(ctx, existing.SubjectID)
	if err != nil {
		h.logger.Error("dsar erasure fulfill", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Fulfillment failed", "server_error", ""))
	}

	outcomeJSON, _ := json.Marshal(map[string]any{
		"mode":            "ERASURE",
		"vkAnonymised":    result.VKAnonymised,
		"agentAnonymised": result.AgentAnonymised,
		"totalAnonymised": result.TotalAnonymised,
		"fulfilledAt":     time.Now().UTC(),
	})
	now := time.Now().UTC()
	completedStatus := "COMPLETED"
	updated, err := h.db.UpdateDSARRequest(ctx, id, dsarstore.UpdateDSARParams{
		Status:      &completedStatus,
		CompletedAt: &now,
		Outcome:     outcomeJSON,
		UpdatedBy:   &updatedBy,
	})
	if err != nil {
		h.logger.Error("dsar update after erasure", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update DSAR status", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDSAR, iam.VerbFulfill)
	ae.EntityID = id
	ae.AfterState = map[string]any{"type": "ERASURE", "vkAnonymised": result.VKAnonymised, "agentAnonymised": result.AgentAnonymised, "totalAnonymised": result.TotalAnonymised}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{"request": updated, "outcome": result})
}
