package compliance_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/contract"
)

// TestContract_ComplianceProxy runs the three-end contract suite. This
// test replaces the legacy TestSharedGoPiiDetector drift test, which was
// schema-specific and broke when the pii-detector config shape evolved.
func TestContract_ComplianceProxy(t *testing.T) {
	contract.Suite(t)
}
