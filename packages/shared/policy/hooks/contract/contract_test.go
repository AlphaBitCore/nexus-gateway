package contract_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/contract"
)

// TestContractSuite runs the canonical suite in the contract package's own
// test binary so schema drift is caught even when no consumer embeds it.
// Consumer services additionally call contract.Suite(t) from their own
// *_test.go to guarantee the same invariants hold in their build.
func TestContractSuite(t *testing.T) {
	contract.Suite(t)
}
