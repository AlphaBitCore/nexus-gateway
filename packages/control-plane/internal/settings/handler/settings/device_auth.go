package settings

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	authserver_store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

const deviceAuthModeKey = "device.auth.mode"

// deviceAuthSettings categorises enabled IdP rows for the Device-Auth
// settings response and the validator. Returned booleans/list are
// nil-safe: a nil DB or list error degrades to zero-value (no external
// SSO providers, no local IdP available) without surfacing the error.
type deviceAuthSettings struct {
	SsoConfigured       bool
	SsoProviders        []map[string]string
	LocalLoginAvailable bool
}

// categoriseDeviceAuthIdPs is the pure-function core of the device-auth
// view: given a list of enabled IdP rows, decide which device-auth
// modes the operator may select. Local rows feed `LocalLoginAvailable`;
// non-local rows feed `SsoConfigured` and surface in the response
// list. Pulled out of collectDeviceAuthSettings so the categorisation
// can be unit-tested without a database.
func categoriseDeviceAuthIdPs(idps []authserver_store.IdentityProvider) deviceAuthSettings {
	out := deviceAuthSettings{SsoProviders: []map[string]string{}}
	for _, idp := range idps {
		if idp.Type == "local" {
			out.LocalLoginAvailable = true
			continue
		}
		out.SsoConfigured = true
		out.SsoProviders = append(out.SsoProviders, map[string]string{
			"id":   idp.ID,
			"type": idp.Type,
			"name": idp.Name,
		})
	}
	return out
}

// isValidDeviceAuthMode is the closed enum check shared by the mode
// validator. Extracted so unit tests can pin the accepted set without
// having to spin up an Echo context.
func isValidDeviceAuthMode(mode string) bool {
	switch mode {
	case "mtls-only", "enterprise-login", "local-login":
		return true
	default:
		return false
	}
}

func (h *Handler) collectDeviceAuthSettings(ctx context.Context) deviceAuthSettings {
	idps, err := h.listIdPs(ctx)
	if err != nil {
		h.logger.Warn("device-auth: list IDPs", "error", err)
		return deviceAuthSettings{SsoProviders: []map[string]string{}}
	}
	return categoriseDeviceAuthIdPs(idps)
}

// listIdPs is the seam between the device-auth view and the IdP store.
// Production path constructs a NewIdPStore from the concrete pool;
// tests inject listIdPsFn directly to skip the *pgxpool.Pool dependency.
// When h.db is nil (defensive — production wiring always passes a DB),
// the helper returns the zero-value "no IdPs configured" result without
// surfacing an error so the calling endpoint still renders a usable
// response.
func (h *Handler) listIdPs(ctx context.Context) ([]authserver_store.IdentityProvider, error) {
	if h.listIdPsFn != nil {
		return h.listIdPsFn(ctx)
	}
	if h.pool == nil {
		return nil, nil
	}
	return authserver_store.NewIdPStore(h.pool).ListEnabled(ctx)
}

func (h *Handler) GetDeviceAuthSettings(c echo.Context) error {
	ctx := c.Request().Context()

	mode := "mtls-only"
	raw, err := h.meta.GetSystemMetadata(ctx, deviceAuthModeKey)
	if err == nil && raw != nil {
		var m string
		if json.Unmarshal(raw, &m) == nil && m != "" {
			mode = m
		}
	}

	s := h.collectDeviceAuthSettings(ctx)
	return c.JSON(http.StatusOK, map[string]any{
		"mode":                mode,
		"ssoConfigured":       s.SsoConfigured,
		"ssoProviders":        s.SsoProviders,
		"localLoginAvailable": s.LocalLoginAvailable,
	})
}

func (h *Handler) UpdateDeviceAuthSettings(c echo.Context) error {
	var body struct {
		Mode string         `json:"mode"`
		OIDC map[string]any `json:"oidc"` // silently ignored if present (backward compat)
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if !isValidDeviceAuthMode(body.Mode) {
		return c.JSON(http.StatusBadRequest, errJSON(
			"mode must be one of mtls-only, enterprise-login, local-login",
			"validation_error", "VALIDATION_ERROR"))
	}

	ctx := c.Request().Context()

	// Mode-specific pre-conditions. Reuses the same IdP-listing helper as
	// GET so the validator and the UI agree on which mode is selectable.
	if (h.listIdPsFn != nil || h.pool != nil) && body.Mode != "mtls-only" {
		s := h.collectDeviceAuthSettings(ctx)
		switch body.Mode {
		case "enterprise-login":
			// Existing constraint: at least one non-local enabled IdP.
			if !s.SsoConfigured {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": "no_sso_provider"})
			}
		case "local-login":
			// Defensive: the seeded local IdP row is always present in
			// a stock install, but an operator could have disabled it
			// via direct DB write. Reject so the agent never lands in
			// a state where its bootstrap reports local-login but the
			// CP login page has no password form to render.
			if !s.LocalLoginAvailable {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": "local_idp_unavailable"})
			}
		}
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}

	if err := h.meta.SetSystemMetadata(ctx, deviceAuthModeKey, body.Mode, updatedBy); err != nil {
		h.logger.Error("save device auth mode", "error", err)
		return internalServerError(c, "Failed to save settings")
	}

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = map[string]any{"mode": body.Mode}
	h.audit.LogObserved(ctx, ae)

	return h.GetDeviceAuthSettings(c)
}
