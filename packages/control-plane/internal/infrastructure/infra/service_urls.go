package infra

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterServiceURLRoutes registers the admin endpoint that exposes
// the externally-reachable URLs of every server Thing currently
// registered with Hub. Server Things (Hub itself, Control Plane,
// AI Gateway, Compliance Proxy) publish their `publicURL` yaml field
// via staticInfo on every WS connect — this endpoint reads
// `thing.metadata.staticInfo.publicUrl` grouped by `thing.type` and
// surfaces the result to the UI so install / enrollment / SSO copy
// can render real-environment URLs instead of hardcoding hostnames.
//
// Canonical type values (see runtimeintrospect.New service names):
//   - "nexus-hub"        (Hub itself)
//   - "control-plane"
//   - "ai-gateway"
//   - "compliance-proxy"
func (h *Handler) RegisterServiceURLRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/services/public-urls", h.GetServicePublicURLs, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

// ServicePublicURLs is the JSON shape returned by
// GET /api/admin/services/public-urls.
//
// Keys are populated when a Thing of that type has reported a non-empty
// publicURL via staticInfo. Missing keys mean either (a) no Thing of
// that type has registered yet, or (b) the registered Thing's yaml
// did not set publicURL.
//
// Fields use lowerCamelCase to match the UI's JSON conventions.
type ServicePublicURLs struct {
	Hub             string `json:"hub,omitempty"`
	ControlPlane    string `json:"controlPlane,omitempty"`
	AIGateway       string `json:"aiGateway,omitempty"`
	ComplianceProxy string `json:"complianceProxy,omitempty"`
}

// GetServicePublicURLs returns one publicURL per server Thing type,
// picking the most-recently-seen instance of each. Useful for fleets
// with multiple Hub or ai-gateway nodes — the UI gets the freshest
// reachable URL.
//
// SQL: DISTINCT ON (thing_type) gives one row per type; the
// last_seen_at DESC tiebreaker picks the freshest. NULLS LAST keeps
// instances that have never set last_seen_at (just registered) at
// the bottom rather than dropping them.
func (h *Handler) GetServicePublicURLs(c echo.Context) error {
	ctx := c.Request().Context()
	rows, err := h.queryServicePublicURLs(ctx)
	if err != nil {
		h.logger.Error("query service public urls", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	out := ServicePublicURLs{}
	for _, r := range rows {
		switch r.ThingType {
		case "nexus-hub":
			out.Hub = r.PublicURL
		case "control-plane":
			out.ControlPlane = r.PublicURL
		case "ai-gateway":
			out.AIGateway = r.PublicURL
		case "compliance-proxy":
			out.ComplianceProxy = r.PublicURL
		}
	}

	return c.JSON(http.StatusOK, out)
}

// queryServicePublicURLs dispatches to the test seam when set; otherwise
// runs the production SQL against the concrete *pgxpool.Pool (or the test
// override pool).
func (h *Handler) queryServicePublicURLs(ctx context.Context) ([]servicePublicURLRow, error) {
	if h.servicePublicURLsQueryFn != nil {
		return h.servicePublicURLsQueryFn(ctx)
	}
	pool := h.servicePoolFor()
	if pool == nil {
		return nil, errPoolNil
	}
	// thing.type is the canonical column (the old "thing_type" name is not used;
	// the Prisma schema + Go store both use `type`).
	pgrows, err := pool.Query(ctx, `
		SELECT DISTINCT ON (type)
		  type,
		  metadata->'staticInfo'->>'publicUrl' AS public_url
		FROM thing
		WHERE metadata->'staticInfo'->>'publicUrl' IS NOT NULL
		  AND metadata->'staticInfo'->>'publicUrl' <> ''
		  AND type IN ('nexus-hub', 'control-plane', 'ai-gateway', 'compliance-proxy')
		ORDER BY type, last_seen_at DESC NULLS LAST
	`)
	if err != nil {
		return nil, err
	}
	defer pgrows.Close()

	var out []servicePublicURLRow
	for pgrows.Next() {
		var r servicePublicURLRow
		if err := pgrows.Scan(&r.ThingType, &r.PublicURL); err != nil {
			h.logger.Error("scan service public urls row", "error", err)
			continue
		}
		out = append(out, r)
	}
	if err := pgrows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// servicePoolQueryer is the minimum pgx pool surface this file's SQL needs.
// *pgxpool.Pool satisfies it in production; pgxmock satisfies it in tests
// via the servicePoolOverride seam set by infra_test.go.
type servicePoolQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// errPoolNil is returned when neither the test override nor the production
// *store.DB.Pool is wired — defensive guard against a misconfigured Handler.
var errPoolNil = errPoolNilSentinel("infra: service-urls pool not configured")

type errPoolNilSentinel string

func (e errPoolNilSentinel) Error() string { return string(e) }

// servicePoolFor returns the prod-time *pgxpool.Pool when present, or the
// test override otherwise. The signature stays narrow so the concrete
// *pgxpool.Pool keeps satisfying it.
func (h *Handler) servicePoolFor() servicePoolQueryer {
	if h.servicePoolOverride != nil {
		return h.servicePoolOverride
	}
	if h.db == nil || h.db.Pool == nil {
		return nil
	}
	return h.db.Pool
}
