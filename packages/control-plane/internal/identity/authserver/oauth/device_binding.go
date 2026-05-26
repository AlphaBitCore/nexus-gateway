package oauth

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// deviceBindingTTL is the lifetime of a freshly created binding handle.
// It must outlast the browser-redirect + user-login window but stay short
// enough that abandoned flows do not linger in memory.
const deviceBindingTTL = 5 * time.Minute

// DeviceBindingDeps carries the collaborators DeviceBindingHandler needs.
// The BindingStore is concrete because it is an in-memory primitive; tests
// construct it directly without fakes.
type DeviceBindingDeps struct {
	Bindings *store.BindingStore
	Logger   *slog.Logger
}

func (d DeviceBindingDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// deviceBindingRequest is the POST body accepted at /oauth/device-binding.
// The agent computes code_challenge = BASE64URL(SHA256(code_verifier)) and
// sends the same value later via /oauth/authorize?code_challenge=...
type deviceBindingRequest struct {
	BindingID     string `json:"binding_id"`
	State         string `json:"state"`
	CodeChallenge string `json:"code_challenge"`
}

// DeviceBindingHandler returns the Echo handler for POST /oauth/device-binding.
// The caller is responsible for wrapping the route with
// middleware.AgentMTLSAuth so that the peer certificate has been validated and
// the resolved device stashed in the Echo context before this handler runs.
//
// On success the handler inserts a BindingEntry keyed by binding_id that the
// /oauth/authorize handler later consumes to prove the browser flow was
// initiated by an mTLS-authenticated agent.
func DeviceBindingHandler(d DeviceBindingDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		device := middleware.AgentDeviceFromContext(c)
		if device == nil {
			// No mTLS middleware result means either the middleware was not
			// wired or it rejected the request. Either way the client is not
			// a trusted device so we answer uniformly.
			return WriteOAuthError(c, ErrInvalidClient, "no device cert", http.StatusUnauthorized)
		}
		if device.Status != "active" {
			// Defensive: the middleware already rejects "revoked" but leaves
			// other non-active states (e.g. "pending") to downstream handlers.
			return WriteOAuthError(c, ErrInvalidClient, "device not active", http.StatusForbidden)
		}

		var body deviceBindingRequest
		if err := c.Bind(&body); err != nil {
			return WriteOAuthError(c, ErrInvalidRequest, "bad body", http.StatusBadRequest)
		}
		if body.BindingID == "" || body.State == "" || body.CodeChallenge == "" {
			return WriteOAuthError(c, ErrInvalidRequest, "bad body", http.StatusBadRequest)
		}

		now := time.Now()
		d.Bindings.Put(body.BindingID, store.BindingEntry{
			DeviceID:      device.ID,
			State:         body.State,
			CodeChallenge: body.CodeChallenge,
			CreatedAt:     now,
			ExpiresAt:     now.Add(deviceBindingTTL),
		})

		d.logger().Info("oauth device binding created",
			slog.String("device_id", device.ID),
			slog.String("binding_id", body.BindingID),
		)
		return c.NoContent(http.StatusNoContent)
	}
}
