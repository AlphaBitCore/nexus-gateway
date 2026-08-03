// internal_things_serviceurl.go — peer service URL resolution:
// GET /api/internal/things/service-url/:thing_type serves a peer service's
// Hub-reported base URLs (staticInfo privateUrl/publicUrl) to service-token
// callers, so no service ever configures another service's address locally.
package hubapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/thingtype"
)

// serviceURLTypes is the set of server Thing types whose reported URLs a peer
// service may resolve. Client types (agent) are excluded by design: an agent
// is an external caller that reaches services via their public URL, and the
// private URL must never be pushed toward end-user devices.
var serviceURLTypes = map[string]bool{
	thingtype.NexusHub:        true,
	thingtype.ControlPlane:    true,
	thingtype.AIGateway:       true,
	thingtype.ComplianceProxy: true,
}

// GetServiceURL handles GET /api/internal/things/service-url/:thing_type.
//
// Peer services call this to resolve another service's reported base URLs
// (staticInfo.privateUrl / publicUrl) instead of carrying the peer's address
// in their own config, which drifts. Selection rule: the most-recently-seen
// Thing of the requested type that reports a URL — one reachable base per
// service type (a horizontally scaled fleet is expected to sit behind a
// single LB base); when several Things report URLs a warning is logged and
// the freshest wins deterministically.
//
// Returns 404 with code SERVICE_URL_NOT_REPORTED when no Thing of the type
// has reported a URL yet (e.g. within the post-registration static_info push
// window) — callers treat that as retry-next-use, not an error state.
//
// Auth: service-token callers only. Device-token callers (agents) get 403.
func (h *InternalThingsAPI) GetServiceURL(c echo.Context) error {
	if ThingFromContext(c) != nil {
		return forbidden(c, "service-token callers only")
	}
	t := strings.TrimSpace(c.Param("thing_type"))
	if !serviceURLTypes[t] {
		return badRequest(c, "unknown or non-service thing type")
	}
	res, err := h.Mgr.ListThings(c.Request().Context(), store.ListThingsParams{Type: t})
	if err != nil {
		return handleErr(c, err)
	}
	var (
		bestPrivate, bestPublic string
		bestSeen                time.Time
		reporting               int
	)
	for i := range res.Things {
		th := &res.Things[i]
		si, _ := th.Metadata["staticInfo"].(map[string]any)
		if si == nil {
			continue
		}
		priv, _ := si["privateUrl"].(string)
		pub, _ := si["publicUrl"].(string)
		if priv == "" && pub == "" {
			continue
		}
		reporting++
		seen := time.Time{}
		if th.LastSeenAt != nil {
			seen = *th.LastSeenAt
		}
		if reporting == 1 || seen.After(bestSeen) {
			bestPrivate, bestPublic, bestSeen = priv, pub, seen
		}
	}
	if reporting == 0 {
		return c.JSON(http.StatusNotFound, map[string]any{
			"error": "no thing of this type has reported a URL yet",
			"code":  "SERVICE_URL_NOT_REPORTED",
		})
	}
	if reporting > 1 {
		c.Logger().Warnf("service-url: %d things of type %s report URLs; using most recently seen", reporting, t)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"thingType":  t,
		"privateUrl": bestPrivate,
		"publicUrl":  bestPublic,
	})
}
