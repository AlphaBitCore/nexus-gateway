package virtualkey

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func (h *Handler) ApproveVirtualKey(c echo.Context) error {
	id := c.Param("id")

	aa := middleware.AdminAuthFromContext(c)
	approvedBy := "unknown"
	if aa != nil {
		approvedBy = aa.KeyID
	}

	if err := h.vks.ApproveVirtualKey(c.Request().Context(), id, approvedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Virtual key not found or not in pending status", "not_found", ""))
		}
		h.logger.Error("approve virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to approve virtual key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbApprove)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "virtual_keys")
	}

	return c.JSON(http.StatusOK, map[string]any{"message": "Virtual key approved"})
}

func (h *Handler) RejectVirtualKey(c echo.Context) error {
	id := c.Param("id")

	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	rejectedBy := "unknown"
	if aa != nil {
		rejectedBy = aa.KeyID
	}

	if err := h.vks.RejectVirtualKey(c.Request().Context(), id, rejectedBy, body.Reason); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Virtual key not found or not in pending status", "not_found", ""))
		}
		h.logger.Error("reject virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to reject virtual key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbReject)
	ae.EntityID = id
	ae.AfterState = map[string]any{"reason": body.Reason}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"message": "Virtual key rejected"})
}

func (h *Handler) RenewVirtualKey(c echo.Context) error {
	id := c.Param("id")

	var body struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.ExpiresAt.IsZero() {
		return c.JSON(http.StatusBadRequest, errJSON("expiresAt is required", "validation_error", ""))
	}

	now := time.Now().UTC()
	maxExpiry := now.AddDate(0, 3, 0)
	if body.ExpiresAt.After(maxExpiry) {
		return c.JSON(http.StatusBadRequest, errJSON("expiresAt must not exceed 3 months from now", "validation_error", ""))
	}
	if !body.ExpiresAt.After(now) {
		return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be in the future", "validation_error", ""))
	}

	if err := h.vks.RenewVirtualKey(c.Request().Context(), id, body.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Virtual key not found, not active, or not an active application key", "not_found", ""))
		}
		h.logger.Error("renew virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to renew virtual key", "server_error", ""))
	}

	// Invalidate the ai-gateway VK cache so the renewed expiry takes
	// effect on the next request rather than the next cache TTL — users
	// who renew a key expect requests through it to stop being rejected
	// as expired immediately.
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "virtual_keys")
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbRenew)
	ae.EntityID = id
	ae.AfterState = map[string]any{"expiresAt": body.ExpiresAt}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"message": "Virtual key renewed", "expiresAt": body.ExpiresAt})
}

func (h *Handler) RevokeVirtualKey(c echo.Context) error {
	id := c.Param("id")

	if err := h.vks.RevokeVirtualKey(c.Request().Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Virtual key not found or not in active status", "not_found", ""))
		}
		h.logger.Error("revoke virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to revoke virtual key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbRevoke)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "virtual_keys")
	}

	return c.JSON(http.StatusOK, map[string]any{"message": "Virtual key revoked"})
}
