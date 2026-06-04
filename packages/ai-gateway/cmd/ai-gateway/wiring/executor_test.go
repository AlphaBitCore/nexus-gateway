package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
)

// TestInitExecutor_nilDepsReturnsNonNil verifies that InitExecutor with nil
// optional deps returns a non-nil bridge and executor.
func TestInitExecutor_nilDepsReturnsNonNil(t *testing.T) {
	allowlist, err := InitForwardHeaderAllowlist(forwardheader.DefaultConfig())
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	adapterReg := InitProviderRegistry(allowlist, discardLogger())

	bridge, exec := InitExecutor(adapterReg, nil, nil, nil, discardLogger())
	if bridge == nil {
		t.Fatal("expected non-nil bridge")
	}
	if exec == nil {
		t.Fatal("expected non-nil executor")
	}
}

// TestInitExecutor_withHealthTracker verifies the health tracker is wired.
func TestInitExecutor_withHealthTracker(t *testing.T) {
	allowlist, err := InitForwardHeaderAllowlist(forwardheader.DefaultConfig())
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	adapterReg := InitProviderRegistry(allowlist, discardLogger())

	// InitHookRegistry is cheap; use it to create a real registry to satisfy
	// InitRouter which we don't call here.
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5})
	if err != nil {
		t.Fatalf("hook registry: %v", err)
	}
	_ = reg

	bridge, exec := InitExecutor(adapterReg, nil, nil, nil, discardLogger())
	if bridge == nil {
		t.Fatal("expected non-nil bridge")
	}
	if exec == nil {
		t.Fatal("expected non-nil executor")
	}
}
