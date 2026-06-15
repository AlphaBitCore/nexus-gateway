package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestConnectionStage_UniformIngressDecision verifies that a connection-stage
// PolicyResolver produces the same RejectHard decision regardless of which
// ingress (ai-gateway / compliance-proxy / agent) drives it. This is the
// three-end decision-uniformity contract established by P-A §3.2.
//
// Service-specific middleware tests live in each service's package:
//   - ai-gateway:       internal/middleware/connection_stage_test.go
//   - compliance-proxy: internal/proxy/listener_connection_stage_test.go
//   - agent:            internal/compliance/pipeline_connection_test.go
//
// This test sits at the resolver layer (below all three) to ensure the
// common contract they depend on is itself stable.
func TestConnectionStage_UniformIngressDecision(t *testing.T) {
	logger := testLogger()
	registry := core.NewHookRegistry()
	registry.Register("test-reject-all-conn", func(cfg *core.HookConfig) (core.Hook, error) {
		return &rejectAllConnStageHook{}, nil
	})

	resolver := NewPolicyResolver([]core.HookConfig{{
		ID: "e2e-1", ImplementationID: "test-reject-all-conn",
		Name: "e2e reject-all", Stage: "connection", Enabled: true,
		FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"},
	}}, registry, logger)

	ingresses := []string{"AI_GATEWAY", "COMPLIANCE_PROXY", "AGENT"}
	for _, ing := range ingresses {
		t.Run(ing, func(t *testing.T) {
			pipe, err := resolver.BuildPipeline("connection", ing, "", nil, 5*time.Second, 30*time.Second, false, false, logger)
			if err != nil {
				t.Fatalf("BuildPipeline(%q) error: %v", ing, err)
			}
			if pipe == nil {
				t.Fatalf("BuildPipeline(%q) returned nil, expected pipeline", ing)
			}
			res := pipe.Execute(context.Background(), &core.HookInput{
				Stage:       "connection",
				IngressType: ing,
				TargetHost:  "forbidden.example.com",
				TLS:         &core.TLSInfo{SNI: "forbidden.example.com"},
			})
			if res.Decision != core.RejectHard {
				t.Fatalf("decision for %s: got %s, want REJECT_HARD", ing, res.Decision)
			}
			if res.Reason == "" {
				t.Fatalf("decision for %s: empty reason", ing)
			}
		})
	}
}

// rejectAllConnStageHook is a minimal connection-stage-compatible hook
// that rejects every connection. Only used by the three-end contract test.
type rejectAllConnStageHook struct {
	core.AnyEndpointAnyModality
}

func (*rejectAllConnStageHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.RejectHard, Reason: "blocked"}, nil
}

func (*rejectAllConnStageHook) ConnectionStageOK() {}
