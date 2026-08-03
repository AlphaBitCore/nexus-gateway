// Package peer is the request-time seam Control Plane handlers use to reach
// peer Nexus services (AI Gateway, Compliance Proxy). A service never
// configures another service's URL: each peer reports its address to the Hub
// at registration, and the shared Hub-backed resolver
// (shared/transport/peerurl) serves it here. Handlers hold a URLProvider and
// resolve per call, so a peer that has not reported yet surfaces as a clean
// 503 (ServiceUnavailable) instead of a baked-in stale address.
package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/peerurl"
)

// URLProvider resolves a peer service's base URL (scheme + host[:port], no
// trailing slash) at request time. An error means the peer is not resolvable
// right now — callers treat it as transient and answer 503 via
// ServiceUnavailable.
type URLProvider func(ctx context.Context) (string, error)

// FromResolver binds the shared Hub-backed resolver to one thing type,
// yielding the peer's private (service-to-service) base URL.
func FromResolver(r *peerurl.Resolver, thingType string) URLProvider {
	return func(ctx context.Context) (string, error) {
		return r.PrivateURL(ctx, thingType)
	}
}

// Static returns a URLProvider that always yields u. Test fixtures and
// fixed-address harnesses.
func Static(u string) URLProvider {
	return func(context.Context) (string, error) { return u, nil }
}

// ErrUnavailable marks a peer-URL resolution failure inside a component that
// cannot write the HTTP response itself (e.g. a dispatcher seam). Handlers
// check errors.Is(err, ErrUnavailable) and answer via ServiceUnavailable.
var ErrUnavailable = errors.New("peer service unavailable")

// CodeUnavailable is the machine-readable error code for a failed peer
// resolution. Clients retry: the peer is booting or the Hub is briefly down.
const CodeUnavailable = "PEER_SERVICE_UNAVAILABLE"

// ServiceUnavailable writes the canonical 503 envelope for a failed peer
// resolution. service is the user-facing peer name ("ai-gateway",
// "compliance-proxy"). The resolver detail (which embeds the internal Hub
// URL on transport failures) is LOGGED server-side, never echoed into the
// response body — the caller gets a fixed retryable message.
func ServiceUnavailable(c echo.Context, service string, err error) error {
	slog.Warn("peer service unresolved", "service", service, "error", err)
	msg := fmt.Sprintf("%s not yet available; retry shortly", service)
	return c.JSON(http.StatusServiceUnavailable, httperr.ErrJSON(msg, "peer_unavailable", CodeUnavailable))
}
