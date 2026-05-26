package compliance_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/contract"
)

// TestContract_Agent runs the three-end contract suite against the
// hooks registered in the agent test binary. Any schema drift between
// shared/hooks and the agent release surfaces here before merge.
func TestContract_Agent(t *testing.T) {
	contract.Suite(t)
}
