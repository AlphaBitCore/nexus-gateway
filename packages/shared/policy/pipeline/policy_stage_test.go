package pipeline

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// noopHook is a minimal Hook that does NOT implement
// core.ConnectionStageCompatible — used to verify that the policy resolver
// rejects connection-stage configs bound to non-compatible implementations.
type noopHook struct {
	core.AnyEndpointAnyModality
}

func (*noopHook) Execute(ctx context.Context, in *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}

// connStageHook is a test hook that opts into the connection stage by
// implementing the ConnectionStageCompatible marker.
type connStageHook struct {
	core.AnyEndpointAnyModality
}

func (*connStageHook) Execute(ctx context.Context, in *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}

func (*connStageHook) ConnectionStageOK() {}

// TestResolve_ConnectionStage_SkipsIncompatibleHook verifies the
// availability-first degradation posture (F-0274): a connection-stage config
// bound to a non-ConnectionStageCompatible impl is SKIPPED+LOGGED, not fatal.
// One misconfigured hook must degrade to "that hook off", never abort the
// whole connection-stage pipeline build.
func TestResolve_ConnectionStage_SkipsIncompatibleHook(t *testing.T) {
	logger := testLogger()
	registry := core.NewHookRegistry()
	registry.Register("rejects-modify", func(cfg *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})

	resolver := NewPolicyResolver([]core.HookConfig{
		{ID: "h1", ImplementationID: "rejects-modify", Name: "incompat",
			Stage: "connection", Enabled: true, FailBehavior: "fail-open"},
	}, registry, logger)

	hks, err := resolver.ResolveHooks("connection", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("incompatible hook must be skipped, not abort the build: %v", err)
	}
	if len(hks) != 0 {
		t.Fatalf("incompatible connection-stage hook must be dropped; got %d hooks", len(hks))
	}
}

// TestResolve_ConnectionStage_SkipsOnlyIncompatibleHook verifies that a mix of
// one incompatible and one compatible connection-stage hook drops only the
// incompatible one — the rest of the pipeline still builds.
func TestResolve_ConnectionStage_SkipsOnlyIncompatibleHook(t *testing.T) {
	logger := testLogger()
	registry := core.NewHookRegistry()
	registry.Register("rejects-modify", func(cfg *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})
	registry.Register("accepts", func(cfg *core.HookConfig) (core.Hook, error) {
		return &connStageHook{}, nil
	})

	resolver := NewPolicyResolver([]core.HookConfig{
		{ID: "bad", ImplementationID: "rejects-modify", Name: "incompat",
			Stage: "connection", Enabled: true, FailBehavior: "fail-open"},
		{ID: "good", ImplementationID: "accepts", Name: "compat",
			Stage: "connection", Enabled: true, FailBehavior: "fail-open"},
	}, registry, logger)

	hks, err := resolver.ResolveHooks("connection", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hks) != 1 {
		t.Fatalf("expected only the compatible hook to survive; got %d", len(hks))
	}
	if hks[0].config.ID != "good" {
		t.Fatalf("surviving hook should be the compatible one; got %q", hks[0].config.ID)
	}
}

func TestResolve_ConnectionStage_AcceptsCompatibleHook(t *testing.T) {
	logger := testLogger()
	registry := core.NewHookRegistry()
	registry.Register("accepts", func(cfg *core.HookConfig) (core.Hook, error) {
		return &connStageHook{}, nil
	})

	resolver := NewPolicyResolver([]core.HookConfig{
		{ID: "h1", ImplementationID: "accepts", Name: "compat",
			Stage: "connection", Enabled: true, FailBehavior: "fail-open"},
	}, registry, logger)

	hks, err := resolver.ResolveHooks("connection", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(hks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hks))
	}
}
