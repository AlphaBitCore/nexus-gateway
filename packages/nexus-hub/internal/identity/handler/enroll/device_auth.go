package enroll

import (
	"crypto/subtle"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

const thingContextKey = "thing"

// ThingFromContext retrieves the validated Thing from the Echo context.
// Returns nil if no Thing was set (e.g. service token auth without Thing lookup).
func ThingFromContext(c echo.Context) *store.Thing {
	v := c.Get(thingContextKey)
	if v == nil {
		return nil
	}
	t, _ := v.(*store.Thing)
	return t
}

// DeviceOrServiceAuth returns middleware that accepts either:
// 1. The internal service token (for CP and other services calling Hub)
// 2. A device token (Bearer + X-Thing-Id header, validated against thing.metadata)
func DeviceOrServiceAuth(st *store.Store, serviceToken string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return unauthorized(c, "missing authorization header")
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			if token == auth {
				return unauthorized(c, "invalid authorization format")
			}

			// Path 1: internal service token. Constant-time compare to
			// avoid leaking the token via byte-by-byte timing.
			if subtle.ConstantTimeCompare([]byte(token), []byte(serviceToken)) == 1 {
				return next(c)
			}

			// Path 2: device token — requires X-Thing-Id
			thingID := c.Request().Header.Get("X-Thing-Id")
			if thingID == "" {
				thingID = c.QueryParam("id")
			}
			if thingID == "" {
				return unauthorized(c, "X-Thing-Id header required for device token auth")
			}

			tokenHash, err := agentca.HashDeviceToken(token)
			if err != nil {
				return unauthorized(c, "invalid token format")
			}

			thing, err := st.RegistryStore().ValidateDeviceToken(c.Request().Context(), thingID, tokenHash)
			if err != nil {
				return unauthorized(c, "invalid or revoked device token")
			}

			c.Set(thingContextKey, thing)
			return next(c)
		}
	}
}
