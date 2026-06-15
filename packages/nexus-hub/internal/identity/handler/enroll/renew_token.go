package enroll

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
)

// RenewToken handles POST /api/internal/things/renew-token.
//
// An enrolled agent rotates its bearer token before expiry so a healthy device
// never lets its token lapse, and
// the previous token is immediately invalidated (its hash is overwritten),
// bounding a stolen token's replay window to a single rotation period.
//
// The endpoint takes no body and trusts no caller-supplied thing ID. It runs
// behind DeviceOrServiceAuth, which has already validated the *current* device
// token and attached the resolved Thing to the request context — so the identity
// being rotated is exactly the one that authenticated. This means:
//   - An expired token cannot reach here: DeviceOrServiceAuth's ValidateDeviceToken
//     fails closed on expiry, so renewal must happen while the token is still
//     valid (the standard refresh-while-valid discipline). The agent's days-long
//     renewal window guarantees that.
//   - A service-token caller has no Thing in context (path 1 of the middleware),
//     so it is rejected — only a device authenticates its own token rotation.
func (h *EnrollmentAPI) RenewToken(c echo.Context) error {
	thing := ThingFromContext(c)
	if thing == nil {
		// Reached only via the internal service token, which has no device
		// identity to rotate. Device-token callers always have a Thing here.
		return unauthorized(c, "device token authentication required to rotate a device token")
	}

	plainToken, hashedToken, err := deviceTokenGenFn()
	if err != nil {
		return internalError(c, "device token generation failed")
	}

	expiresAt := agentca.DeviceTokenExpiry(timeNow())
	if err := h.Mgr.Store().RegistryStore().StoreDeviceTokenHash(c.Request().Context(), thing.ID, hashedToken, expiresAt); err != nil {
		return internalError(c, "device token storage failed")
	}

	h.logger().Info("device token rotated", "thing_id", thing.ID, "expires_at", expiresAt.Format(time.RFC3339))
	return c.JSON(http.StatusOK, map[string]any{
		"deviceToken":          plainToken,
		"deviceTokenExpiresAt": expiresAt.Format(time.RFC3339),
	})
}
