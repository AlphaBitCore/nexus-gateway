package pipeline

import (
	"context"
	"strings"
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

func TestResolve_ConnectionStage_RejectsIncompatibleHook(t *testing.T) {
	logger := testLogger()
	registry := core.NewHookRegistry()
	registry.Register("rejects-modify", func(cfg *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})

	resolver := NewPolicyResolver([]core.HookConfig{
		{ID: "h1", ImplementationID: "rejects-modify", Name: "incompat",
			Stage: "connection", Enabled: true, FailBehavior: "fail-open"},
	}, registry, logger)

	_, err := resolver.ResolveHooks("connection", "AI_GATEWAY")
	if err == nil {
		t.Fatal("expected error for hook that doesn't implement ConnectionStageCompatible")
	}
	if !strings.Contains(err.Error(), "not connection-stage compatible") {
		t.Fatalf("unexpected error: %v", err)
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

	hks, err := resolver.ResolveHooks("connection", "AI_GATEWAY")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(hks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hks))
	}
}
