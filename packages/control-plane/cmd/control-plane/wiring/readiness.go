package wiring

import (
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/infrastructure/infra"
)

// ReadinessChecker is implemented by infra.Handler.ReadinessCheck.
type ReadinessChecker interface {
	ReadinessCheck(c echo.Context) error
}

// InitReadiness registers the /ready and /api/admin/ready probes on e.
// Both probes are bound on the root echo instance (not adminGroup) so they
// are reachable without an admin token.
func InitReadiness(e *echo.Echo, rc ReadinessChecker) {
	e.GET("/ready", rc.ReadinessCheck)
	e.GET("/api/admin/ready", rc.ReadinessCheck)
}

// NewReadinessHandler constructs the infra.Handler used for readiness checks.
func NewReadinessHandler(d infra.Deps) ReadinessChecker {
	return infra.New(d)
}
