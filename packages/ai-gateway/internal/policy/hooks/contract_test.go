package hooks_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/contract"
)

// TestContract_AIGateway runs the three-end contract suite against the
// hooks registered in this service's test binary. Runs both shared
// fixtures and (when added) ai-gateway-local extension fixtures for
// webhook-forward + quality-checker.
func TestContract_AIGateway(t *testing.T) {
	contract.Suite(t)
}
