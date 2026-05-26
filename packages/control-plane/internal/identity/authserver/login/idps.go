// Package login serves the SPA-facing JSON endpoints behind the
// Control-Plane hosted login flow. Two handlers live here: GET
// /authserver/idps lists the enabled identity providers tied to a pending
// authorize request, and POST /authserver/password authenticates against
// the local IdP and mints an authorization code. The SPA owns the HTML,
// so there is no template rendering in this package.
package login

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// IdPsDeps carries the collaborators the method-picker JSON endpoint needs.
// Pending is used only to validate that `authctx` still refers to a live
// pending authorize request — without it a stranger could enumerate the
// enabled IdP list outside of an OAuth flow.
type IdPsDeps struct {
	IdPs    *store.IdPStore
	Pending *store.PendingAuthzStore
}

// IdpEntry is one row in the IdP list the SPA renders on the method picker.
// The shape is deliberately minimal — the SPA decides button copy and icon
// locally rather than trusting server-rendered strings.
type IdpEntry struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "local" | "oidc" | "saml"
	Name string `json:"name"`
}

// IdpListResponse is the success payload for GET /authserver/idps.
type IdpListResponse struct {
	Providers []IdpEntry `json:"providers"`
}

// IdpsHandler returns an echo.HandlerFunc that answers GET
// /authserver/idps?authctx=<id> with the enabled IdPs the SPA should render
// as buttons on the method picker. The handler is intentionally cheap — it
// hits IdPStore.ListEnabled which is typically a handful of rows — so no
// caching layer is warranted.
func IdpsHandler(d IdPsDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		authctx := c.QueryParam("authctx")
		if authctx == "" || !d.Pending.Has(authctx) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		providers, err := d.IdPs.ListEnabled(c.Request().Context())
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}

		out := make([]IdpEntry, 0, len(providers))
		for _, p := range providers {
			out = append(out, IdpEntry{ID: p.ID, Type: p.Type, Name: p.Name})
		}
		return c.JSON(http.StatusOK, IdpListResponse{Providers: out})
	}
}
