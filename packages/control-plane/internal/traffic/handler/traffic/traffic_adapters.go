package traffic

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
)

// RegisterTrafficAdapterCatalogRoute registers GET /traffic-adapters — the
// canonical list of built-in traffic adapter IDs from packages/shared/traffic
// (same table RegisterBuiltins uses at data-plane startup).
func (h *Handler) RegisterTrafficAdapterCatalogRoute(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/traffic-adapters", h.ListBuiltinTrafficAdapters, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

// ListBuiltinTrafficAdapters returns sorted adapter IDs for interception domain
// forms and other admin UIs.
func (h *Handler) ListBuiltinTrafficAdapters(c echo.Context) error {
	ids := adapters.BuiltinTrafficAdapterIDs()
	return c.JSON(http.StatusOK, map[string]any{"data": ids})
}
