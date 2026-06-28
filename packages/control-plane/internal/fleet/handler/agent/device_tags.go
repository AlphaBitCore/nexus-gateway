package agent

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// runUpdateDeviceTags executes the device-tags UPDATE either via the
// production *pgxpool.Pool or, in tests, via the updateDeviceTagsFn
// seam. Centralising the routing keeps PutDeviceTags free of the
// fallback ladder.
func (h *Handler) runUpdateDeviceTags(ctx context.Context, deviceID string, tags []string) error {
	if h.updateDeviceTagsFn != nil {
		return h.updateDeviceTagsFn(ctx, deviceID, tags)
	}
	_, err := h.pool.Exec(ctx, `
		UPDATE thing SET tags = $2, updated_at = NOW() WHERE id = $1
	`, deviceID, tags)
	return err
}

// Admin endpoints for managing free-form tags on a device.
// Tags compose into smart-group predicates (`tags_contains`) and
// surface as filter chips on the Devices list. Wire-up lives in
// admin_agent_devices.go RegisterAdminAgentDeviceRoutes.
//
//   PUT /api/admin/agent-devices/:id/tags — replace the full tag set
//
// PUT (full replace) is the chosen verb because tags are an
// unordered set and partial deltas (add/remove) compose awkwardly
// with PATCH for arrays. The body is `{"tags": ["finance", "byod"]}`;
// empty array clears all tags.

type putDeviceTagsRequest struct {
	Tags []string `json:"tags"`
}

// PutDeviceTags handles PUT /api/admin/agent-devices/:id/tags.
// IAM: admin:agent-device.update.
func (h *Handler) PutDeviceTags(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("device id is required", "validation_error", ""))
	}
	var req putDeviceTagsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", ""))
	}
	// Defensive: enforce simple tag shape — non-empty, no control
	// chars, max length 64. Prevents accidental "tag" payloads
	// becoming JSON-encoded objects.
	for _, t := range req.Tags {
		if t == "" {
			return c.JSON(http.StatusBadRequest, errJSON("tags must be non-empty strings", "validation_error", "INVALID_TAG"))
		}
		if len(t) > 64 {
			return c.JSON(http.StatusBadRequest, errJSON("tag length must be ≤ 64 chars", "validation_error", "TAG_TOO_LONG"))
		}
	}

	// 404 when the device itself doesn't exist.
	existing, err := h.agents.GetThingNode(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Device not found", "not_found", "NOT_FOUND"))
	}

	// Tags array is owned by `thing.tags`. UPDATE
	// directly — no separate junction table. Routed through the
	// updateDeviceTagsFn test seam so unit tests can drive the
	// handler without a concrete *pgxpool.Pool.
	if err := h.runUpdateDeviceTags(c.Request().Context(), id, req.Tags); err != nil {
		h.logger.Error("put device tags", "deviceId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceAgentDevice, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"deviceId": id, "tags": req.Tags}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"id": id, "tags": req.Tags})
}
