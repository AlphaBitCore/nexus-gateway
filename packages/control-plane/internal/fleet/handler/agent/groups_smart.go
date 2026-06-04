package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

// Smart-group admin endpoints. Wire-up in admin_device_groups.go
// (RegisterDeviceGroupRoutes) so the new endpoints appear under the
// same Echo group with the same IAM gating.

// previewMembershipRequest is the body shape for the dry-run preview.
// Operators paste a draft predicate here before saving to gauge
// blast radius. The CP runs the same evaluator the Hub recompute job
// uses; counts + a sample of matching device IDs come back so the
// operator can sanity-check.
type previewMembershipRequest struct {
	MembershipQuery device.Predicate `json:"membershipQuery"`
}

// previewMembershipResponse is the body shape returned. Sample is
// capped at 50 device IDs to keep the payload small — operators
// don't need a full enumeration to validate a predicate.
type previewMembershipResponse struct {
	Matched int      `json:"matched"`
	Sample  []string `json:"sample"`
}

// PreviewMembership handles POST /api/admin/device-groups/preview-membership.
// IAM: admin:device-group.read (same as listing groups — doesn't
// mutate any state, just runs the predicate).
func (h *Handler) PreviewMembership(c echo.Context) error {
	var req previewMembershipRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", ""))
	}

	devices, err := h.loadPreviewDevices(c.Request().Context())
	if err != nil {
		h.logger.Error("preview membership: load devices", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}

	matched := []string{}
	nowSec := nowUnix()
	for i := range devices {
		ok, evalErr := device.Evaluate(req.MembershipQuery, &devices[i].Dev, nowSec)
		if evalErr != nil {
			return c.JSON(http.StatusBadRequest, errJSON(evalErr.Error(), "validation_error", "INVALID_PREDICATE"))
		}
		if ok {
			matched = append(matched, devices[i].ID)
		}
	}
	sample := matched
	if len(sample) > 50 {
		sample = sample[:50]
	}
	return c.JSON(http.StatusOK, previewMembershipResponse{
		Matched: len(matched),
		Sample:  sample,
	})
}

// setSmartQueryRequest carries the optional new predicate. A nil
// `membershipQuery` field (i.e. `null` in JSON) switches the group
// back to static mode and drops cached memberships. A non-nil
// (possibly empty) predicate marks the group smart.
type setSmartQueryRequest struct {
	MembershipQuery *device.Predicate `json:"membershipQuery"`
}

// SetGroupMembershipQuery handles PUT /api/admin/device-groups/:id/membership-query.
// IAM: admin:device-group.update.
//
// The recompute is intentionally deferred to the next Hub tick (or
// the next heartbeat-driven recompute). Operators care more about
// the mode flip being atomic than about the cache being current at
// the very moment of the PUT — and a synchronous recompute on the
// admin path would create a slow CP request whenever the fleet is
// large. The preview endpoint gives operators the cache outcome
// pre-save; the 60s safety job converges within one tick.
func (h *Handler) SetGroupMembershipQuery(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("group id is required", "validation_error", ""))
	}
	var req setSmartQueryRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", ""))
	}
	var raw []byte
	if req.MembershipQuery != nil {
		// Validate predicate shape by running it against a sentinel
		// device — rejects unknown fields/ops cleanly before persisting.
		_, evalErr := device.Evaluate(*req.MembershipQuery, &device.Device{}, nowUnix())
		if evalErr != nil {
			return c.JSON(http.StatusBadRequest, errJSON(evalErr.Error(), "validation_error", "INVALID_PREDICATE"))
		}
		b, err := json.Marshal(req.MembershipQuery)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
		}
		raw = b
	}

	updated, err := h.agents.SetSmartGroupQuery(c.Request().Context(), id, raw)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return c.JSON(http.StatusNotFound, errJSON("Group not found", "not_found", "NOT_FOUND"))
		}
		h.logger.Error("set smart query", "groupId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if updated == nil {
		return c.JSON(http.StatusNotFound, errJSON("Group not found", "not_found", "NOT_FOUND"))
	}

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbUpdate)
	ae.EntityID = id
	mode := "smart"
	if raw == nil {
		mode = "static"
	}
	ae.AfterState = map[string]any{"groupId": id, "mode": mode}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

// previewDevicesForSmartEval loads the device fleet in the shape the
// predicate evaluator understands. Mirrors the Hub-side query so
// PreviewMembership and the recompute job produce identical results
// for the same predicate.
//
// Kept as a method on AdminHandler (not a free function) so future
// `payload-capture` / `device-defaults` previews can reuse it without
// re-implementing the join.
type previewDevice struct {
	ID  string
	Dev device.Device
}

// loadPreviewDevices is the routing helper that honours the
// previewDevicesFn test seam before falling back to the production
// *pgxpool.Pool query.
func (h *Handler) loadPreviewDevices(ctx context.Context) ([]previewDevice, error) {
	if h.previewDevicesFn != nil {
		return h.previewDevicesFn(ctx)
	}
	return h.previewDevicesForSmartEval(ctx)
}

func (h *Handler) previewDevicesForSmartEval(ctx context.Context) ([]previewDevice, error) {
	// Mirror smart_group.go's IdP-group aggregate so preview
	// matches recompute exactly.
	rows, err := h.pool.Query(ctx, `
		SELECT
			t.id,
			COALESCE(t.os, ''),
			COALESCE(t.os_version, ''),
			COALESCE(t.version, ''),
			COALESCE(t.hostname, ''),
			COALESCE(t.primary_ip, ''),
			COALESCE(t.physical_id, ''),
			COALESCE(t.status, ''),
			COALESCE(da."userId", ''),
			COALESCE(o.path, ''),
			COALESCE(EXTRACT(EPOCH FROM t.enrolled_at)::bigint, 0),
			COALESCE(EXTRACT(EPOCH FROM t.last_seen_at)::bigint, 0),
			COALESCE(
				(
					SELECT array_agg(igm."groupId" ORDER BY igm."groupId")
					FROM "IamGroupMembership" igm
					WHERE igm."principalType" IN ('nexus_user', 'admin_user')
					  AND igm."principalId" = da."userId"
				),
				ARRAY[]::text[]
			),
			COALESCE(t.tags, ARRAY[]::text[])
		FROM thing t
		LEFT JOIN "DeviceAssignment" da
		    ON da."deviceId" = t.id AND da."releasedAt" IS NULL
		LEFT JOIN "NexusUser" u ON u.id = da."userId"
		LEFT JOIN "Organization" o ON o.id = u."organizationId"
		WHERE t.type = 'agent'
		ORDER BY t.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []previewDevice{}
	for rows.Next() {
		var pd previewDevice
		if err := rows.Scan(
			&pd.ID,
			&pd.Dev.OS, &pd.Dev.OSVersion, &pd.Dev.AgentVersion,
			&pd.Dev.Hostname, &pd.Dev.PrimaryIP, &pd.Dev.PhysicalID, &pd.Dev.Status,
			&pd.Dev.BoundUserID, &pd.Dev.BoundUserOrgPath,
			&pd.Dev.EnrolledAtSec, &pd.Dev.LastHeartbeatSec,
			&pd.Dev.IdpGroupIDs,
			&pd.Dev.Tags,
		); err != nil {
			return nil, err
		}
		out = append(out, pd)
	}
	return out, rows.Err()
}

// errNotFound is the in-package marker for "row not found"; the
// store returns nil + nil error on miss for SetSmartGroupQuery so we
// can rely on the nil-check at the call site rather than an error
// sentinel. This var exists only to avoid an unused-import lint when
// the file's other error wrapping path stays simple.
var errNotFound = errors.New("not found")

// nowUnix is a tiny helper that exists so tests can shadow it for
// deterministic time-based predicate evaluation. Production code
// returns real time.
var nowUnix = nowUnixImpl

func nowUnixImpl() int64 { return time.Now().UTC().Unix() }
