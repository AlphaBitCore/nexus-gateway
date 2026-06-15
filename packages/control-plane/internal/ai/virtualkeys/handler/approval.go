package virtualkey

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
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

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "virtual_keys"); err != nil {
			h.logger.Error("approve virtual key: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbApprove)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

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
	// A non-super-admin may only renew a VK they own (mirrors the
	// Update/Delete CRUD owner re-check); without this a narrow virtual-key:renew
	// grant could extend any application key's expiry.
	if vk, resp := h.ownedVKOrDeny(c); vk == nil {
		return resp
	}
	id := c.Param("id")

	var body struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	// Renew only ever targets an active APPLICATION key (the store-side query
	// filters to vk_type='application'), so the same cap as create/update applies.
	// A zero/omitted expiresAt is the nil case the shared helper rejects.
	var expiresAt *time.Time
	if !body.ExpiresAt.IsZero() {
		expiresAt = &body.ExpiresAt
	}
	if msg := capApplicationExpiry(expiresAt); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
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
	// as expired immediately. Fail loud on push failure so the admin
	// retries instead of believing the renewal already took effect.
	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "virtual_keys"); err != nil {
			h.logger.Error("renew virtual key: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbRenew)
	ae.EntityID = id
	ae.AfterState = map[string]any{"expiresAt": body.ExpiresAt}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"message": "Virtual key renewed", "expiresAt": body.ExpiresAt})
}

func (h *Handler) RevokeVirtualKey(c echo.Context) error {
	// A non-super-admin may only revoke a VK they own (mirrors the
	// Update/Delete CRUD owner re-check); without this a narrow virtual-key:revoke
	// grant could disable any other principal's production key (data-plane DoS).
	if vk, resp := h.ownedVKOrDeny(c); vk == nil {
		return resp
	}
	id := c.Param("id")

	if err := h.vks.RevokeVirtualKey(c.Request().Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Virtual key not found or not in active status", "not_found", ""))
		}
		h.logger.Error("revoke virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to revoke virtual key", "server_error", ""))
	}

	// Revocation MUST reach the gateway or the revoked key keeps
	// authenticating from the VK cache — fail loud so the admin retries.
	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "virtual_keys"); err != nil {
			h.logger.Error("revoke virtual key: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbRevoke)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"message": "Virtual key revoked"})
}
