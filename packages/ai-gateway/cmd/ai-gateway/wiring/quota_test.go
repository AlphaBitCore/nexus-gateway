package wiring

import (
	"context"
	"testing"
)

// TestInitQuota_nilDBReturnsNils verifies that nil DB returns (nil, nil)
// without panicking — quota enforcement is skipped in degraded mode.
func TestInitQuota_nilDBReturnsNils(t *testing.T) {
	engine, policyCache := InitQuota(context.Background(), nil, nil, discardLogger())
	if engine != nil {
		t.Error("expected nil quota engine for nil DB")
	}
	if policyCache != nil {
		t.Error("expected nil policy cache for nil DB")
	}
}
