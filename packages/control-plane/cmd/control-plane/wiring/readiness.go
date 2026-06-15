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
// Both probes are bound on the root echo instance (NOT adminGroup) and are
// INTENTIONALLY unauthenticated: load balancers, k8s, and uptime checks
// must reach them without an admin token. The response carries only
// dependency-health booleans ({checks:{db,hub}, status}) — no mutation, no
// sensitive data — so open access is safe.
//
// Note: the `/api/admin/` prefix on the second path is a
// naming smell — it can falsely read as admin-gated. It is kept for client
// compatibility; the iam-exempt markers below make the intent explicit so a
// reviewer (and the check-iam-route-coverage scanner) never mistakes the
// missing iamMW for an accidental gap.
func InitReadiness(e *echo.Echo, rc ReadinessChecker) {
	e.GET("/ready", rc.ReadinessCheck)           // iam-exempt: readiness probe (health booleans only)
	e.GET("/api/admin/ready", rc.ReadinessCheck) // iam-exempt: readiness probe (health booleans only)
}

// NewReadinessHandler constructs the infra.Handler used for readiness checks.
func NewReadinessHandler(d infra.Deps) ReadinessChecker {
	return infra.New(d)
}
