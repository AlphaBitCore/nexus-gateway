package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
)

const agentDeviceContextKey = "agentDevice"

// AgentDeviceFromContext extracts the agent device from an Echo context.
func AgentDeviceFromContext(c echo.Context) *agentstore.ThingNodeInfo {
	if v := c.Get(agentDeviceContextKey); v != nil {
		return v.(*agentstore.ThingNodeInfo)
	}
	return nil
}

// ThingNodeLookup is called by the mTLS middleware to find a device by cert serial.
type ThingNodeLookup interface {
	LookupThingNodeByCertSerial(ctx context.Context, serial string) (*agentstore.ThingNodeInfo, error)
}

// AgentMTLSAuth returns Echo middleware that authenticates agent requests via
// client TLS certificate. It extracts the peer certificate serial, normalizes
// it (uppercase, no colons), and looks up the device in the database.
func AgentMTLSAuth(lookup ThingNodeLookup) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			tls := c.Request().TLS
			if tls == nil || len(tls.PeerCertificates) == 0 {
				return c.JSON(http.StatusUnauthorized, errorResp(
					"Valid client certificate required",
					"authentication_error",
					"AGENT_CERT_REQUIRED",
				))
			}

			cert := tls.PeerCertificates[0]
			serial := fmt.Sprintf("%032X", cert.SerialNumber)
			serial = strings.ToUpper(strings.ReplaceAll(serial, ":", ""))

			device, err := lookup.LookupThingNodeByCertSerial(c.Request().Context(), serial)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, errorResp(
					"Authentication service error",
					"server_error",
					"AUTH_SERVICE_ERROR",
				))
			}
			if device == nil {
				return c.JSON(http.StatusUnauthorized, errorResp(
					"Unknown device certificate",
					"authentication_error",
					"AGENT_CERT_UNKNOWN",
				))
			}
			if device.Status == "revoked" {
				return c.JSON(http.StatusForbidden, errorResp(
					"Device has been revoked",
					"authorization_error",
					"AGENT_DEVICE_REVOKED",
				))
			}

			c.Set(agentDeviceContextKey, device)
			return next(c)
		}
	}
}
