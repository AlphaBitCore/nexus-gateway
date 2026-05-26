// Package handler — diag_silences.go: HTTP handlers for the
// /api/admin/diag-silences surface. A silence collapses repeating noise
// in the /infrastructure/errors page so triage stays focused on what's
// new. Read/Write are gated under the observability resource (same as
// the diag-events stream they ack).
package infra

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/diag/diagstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterDiagSilencesRoutes wires the three diag-silences endpoints.
//
// IAM: writes are gated on observability:update (silencing affects what
// other operators see, comparable in blast-radius to changing dashboards).
// Reads use observability:read (same as the underlying diag events).
func (h *Handler) RegisterDiagSilencesRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/diag-silences", h.DiagSilencesList, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.POST("/diag-silences", h.DiagSilencesCreate, iamMW(iam.ResourceObservability.Action(iam.VerbWrite)))
	g.DELETE("/diag-silences/:id", h.DiagSilencesDelete, iamMW(iam.ResourceObservability.Action(iam.VerbWrite)))
}

type createDiagSilenceRequest struct {
	MessageHash string `json:"messageHash"`
	Level       string `json:"level"`
	// TTL in seconds; 0 = permanent silence. Capped at 30 days to discourage
	// "silence forever and forget".
	TTLSeconds int    `json:"ttlSeconds"`
	Reason     string `json:"reason"`
}

const (
	maxSilenceTTLSeconds = 30 * 24 * 60 * 60 // 30 days
)

// DiagSilencesList returns active silences. The Errors page uses this
// to surface "what's currently silenced" in the filter panel.
func (h *Handler) DiagSilencesList(c echo.Context) error {
	if h.diag == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	silences, err := h.diag.ListActiveDiagSilences(c.Request().Context())
	if err != nil {
		h.logger.Error("list_diag_silences", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list silences", "server_error", "INTERNAL_ERROR"))
	}
	if silences == nil {
		silences = []diagstore.DiagSilence{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": silences})
}

// DiagSilencesCreate creates a new silence row. Returns 400 on validation
// failure, 200 on success with the persisted row.
func (h *Handler) DiagSilencesCreate(c echo.Context) error {
	if h.diag == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	var req createDiagSilenceRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", "VALIDATION_ERROR"))
	}
	req.MessageHash = strings.TrimSpace(req.MessageHash)
	req.Level = strings.ToLower(strings.TrimSpace(req.Level))
	req.Reason = strings.TrimSpace(req.Reason)
	if req.MessageHash == "" {
		return c.JSON(http.StatusBadRequest, errJSON("messageHash is required", "validation_error", "VALIDATION_ERROR"))
	}
	switch req.Level {
	case "debug", "info", "warn", "error", "fatal":
	default:
		return c.JSON(http.StatusBadRequest, errJSON("level must be one of debug|info|warn|error|fatal", "validation_error", "VALIDATION_ERROR"))
	}
	if req.TTLSeconds < 0 {
		return c.JSON(http.StatusBadRequest, errJSON("ttlSeconds must be >= 0", "validation_error", "VALIDATION_ERROR"))
	}
	if req.TTLSeconds > maxSilenceTTLSeconds {
		return c.JSON(http.StatusBadRequest, errJSON("ttlSeconds exceeds 30-day cap", "validation_error", "VALIDATION_ERROR"))
	}
	if len(req.Reason) > 500 {
		return c.JSON(http.StatusBadRequest, errJSON("reason exceeds 500 chars", "validation_error", "VALIDATION_ERROR"))
	}

	var expiresAt *time.Time
	if req.TTLSeconds > 0 {
		t := time.Now().UTC().Add(time.Duration(req.TTLSeconds) * time.Second)
		expiresAt = &t
	}

	actor := actorFromContext(c)
	s, err := h.diag.CreateDiagSilence(c.Request().Context(), diagstore.CreateDiagSilenceParams{
		MessageHash: req.MessageHash,
		Level:       req.Level,
		SilencedBy:  actor.UserID,
		ExpiresAt:   expiresAt,
		Reason:      req.Reason,
	})
	if err != nil {
		h.logger.Error("create_diag_silence", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create silence", "server_error", "INTERNAL_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceObservability, iam.VerbWrite)
	ae.EntityID = s.ID
	ae.AfterState = map[string]any{
		"messageHash": s.MessageHash,
		"level":       s.Level,
		"expiresAt":   s.ExpiresAt,
		"reason":      s.Reason,
		"action":      "diag_silence_create",
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"silence": s})
}

// DiagSilencesDelete removes a silence by id (unsilence). Captures the
// pre-delete row in the audit before-state.
func (h *Handler) DiagSilencesDelete(c echo.Context) error {
	if h.diag == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}

	before, err := h.diag.GetDiagSilence(c.Request().Context(), id)
	if err != nil {
		if errors.Is(err, diagstore.ErrSilenceNotFound) {
			return c.JSON(http.StatusNotFound, errJSON("silence not found", "not_found", "SILENCE_NOT_FOUND"))
		}
		h.logger.Error("get_diag_silence", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to load silence", "server_error", "INTERNAL_ERROR"))
	}

	if err := h.diag.DeleteDiagSilence(c.Request().Context(), id); err != nil {
		if errors.Is(err, diagstore.ErrSilenceNotFound) {
			return c.JSON(http.StatusNotFound, errJSON("silence not found", "not_found", "SILENCE_NOT_FOUND"))
		}
		h.logger.Error("delete_diag_silence", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete silence", "server_error", "INTERNAL_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceObservability, iam.VerbWrite)
	ae.EntityID = id
	ae.BeforeState = map[string]any{
		"messageHash": before.MessageHash,
		"level":       before.Level,
		"expiresAt":   before.ExpiresAt,
		"reason":      before.Reason,
	}
	ae.AfterState = map[string]any{"action": "diag_silence_delete"}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true, "id": id})
}
