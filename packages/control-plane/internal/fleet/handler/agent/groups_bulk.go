package agent

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// Bulk-by-group admin ops.
//
// Wire-up lives in admin_device_groups.go RegisterDeviceGroupRoutes.
// Each endpoint resolves the group's members (static + smart-cache
// UNION via MembersOfGroup) and fans out the corresponding
// per-device admin action concurrently with bounded parallelism.
// Returns a per-device result map so the UI can render which
// devices succeeded / failed.
//
//   POST /api/admin/device-groups/:id/force-refresh
//   POST /api/admin/device-groups/:id/rotate-cert
//
// Diag mode is intentionally NOT bulk-by-group at this layer — the
// existing diagModeApi.bulk already accepts a filter (with the
// proper "207 partial success" semantics + a 500-thing cap), and
// composing that with group membership is best done client-side.

// bulkActionResult is one row of the per-device fan-out result.
type bulkActionResult struct {
	DeviceID string `json:"deviceId"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// bulkActionResponse wraps the fan-out outcome with a top-level
// success/failure count so the UI can render "12/15 succeeded" at a
// glance without iterating items.
type bulkActionResponse struct {
	GroupID   string             `json:"groupId"`
	Action    string             `json:"action"`
	Total     int                `json:"total"`
	Succeeded int                `json:"succeeded"`
	Failed    int                `json:"failed"`
	Results   []bulkActionResult `json:"results"`
}

const bulkFanoutConcurrency = 16

// runBulkFanout is the shared fan-out helper. fn is called once per
// device with bounded parallelism. Errors from fn surface as
// `{ok:false, error:"..."}` on the result map, NOT as a top-level
// 500 — operators expect partial success to be observable, not
// short-circuited.
func runBulkFanout(ctx context.Context, devices []string, fn func(context.Context, string) error) []bulkActionResult {
	results := make([]bulkActionResult, len(devices))
	sem := make(chan struct{}, bulkFanoutConcurrency)
	var wg sync.WaitGroup
	for i, did := range devices {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := fn(ctx, did)
			r := bulkActionResult{DeviceID: did, OK: err == nil}
			if err != nil {
				r.Error = err.Error()
			}
			results[i] = r
		}()
	}
	wg.Wait()
	return results
}

func summarize(results []bulkActionResult) (succeeded, failed int) {
	for _, r := range results {
		if r.OK {
			succeeded++
		} else {
			failed++
		}
	}
	return
}

// BulkForceRefreshGroup handles POST /api/admin/device-groups/:id/force-refresh.
// IAM: admin:device-group.update (write-level since it triggers config push
// on every member; the per-device check happens via Hub which knows the
// caller is authorised at the group level).
func (h *Handler) BulkForceRefreshGroup(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("group id is required", "validation_error", ""))
	}
	g, err := h.agents.GetDeviceGroup(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("bulk force-refresh: get group", "groupId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if g == nil {
		return c.JSON(http.StatusNotFound, errJSON("Group not found", "not_found", "NOT_FOUND"))
	}
	members, err := h.agents.MembersOfGroup(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}

	results := runBulkFanout(c.Request().Context(), members, func(ctx context.Context, deviceID string) error {
		if h.hub == nil {
			return errors.New("hub not configured")
		}
		_, hubErr := h.hub.ForceResyncAll(ctx, deviceID)
		if errors.Is(hubErr, hub.ErrNotConfigured) {
			return errors.New("hub not configured")
		}
		return hubErr
	})
	succ, fail := summarize(results)

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"action": "force-refresh", "total": len(members), "succeeded": succ, "failed": fail}
	h.audit.LogObserved(c.Request().Context(), ae)

	status := http.StatusOK
	if fail > 0 && succ > 0 {
		status = http.StatusMultiStatus
	}
	return c.JSON(status, bulkActionResponse{
		GroupID: id, Action: "force-refresh",
		Total: len(members), Succeeded: succ, Failed: fail,
		Results: results,
	})
}

// BulkRotateCertGroup handles POST /api/admin/device-groups/:id/rotate-cert.
// IAM: admin:device-group.update (write-level; same rationale as force-refresh).
func (h *Handler) BulkRotateCertGroup(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("group id is required", "validation_error", ""))
	}
	g, err := h.agents.GetDeviceGroup(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if g == nil {
		return c.JSON(http.StatusNotFound, errJSON("Group not found", "not_found", "NOT_FOUND"))
	}
	members, err := h.agents.MembersOfGroup(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}

	results := runBulkFanout(c.Request().Context(), members, func(ctx context.Context, deviceID string) error {
		if h.hub == nil {
			return errors.New("hub not configured")
		}
		_, hubErr := h.hub.RotateAgentCert(ctx, deviceID)
		if errors.Is(hubErr, hub.ErrNotConfigured) {
			return errors.New("hub not configured")
		}
		return hubErr
	})
	succ, fail := summarize(results)

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"action": "rotate-cert", "total": len(members), "succeeded": succ, "failed": fail}
	h.audit.LogObserved(c.Request().Context(), ae)

	status := http.StatusOK
	if fail > 0 && succ > 0 {
		status = http.StatusMultiStatus
	}
	return c.JSON(status, bulkActionResponse{
		GroupID: id, Action: "rotate-cert",
		Total: len(members), Succeeded: succ, Failed: fail,
		Results: results,
	})
}
