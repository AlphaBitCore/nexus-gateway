package compliance

import (
	"context"
	"github.com/goccy/go-json"
	"sync/atomic"
	"testing"

	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Test helpers — hooks that implement ConnectionStageCompatible so the
// resolver lets them bind at stage="connection".

type rejectAllConnHook struct {
	hooks.AnyEndpointAnyModality
	reason string
}

func (h *rejectAllConnHook) Execute(_ context.Context, _ *hooks.HookInput) (*hooks.HookResult, error) {
	reason := h.reason
	if reason == "" {
		reason = "blocked by test policy"
	}
	return &hooks.HookResult{Decision: hooks.RejectHard, Reason: reason}, nil
}

func (*rejectAllConnHook) ConnectionStageOK() {}

type approveConnHook struct {
	hooks.AnyEndpointAnyModality
}

func (*approveConnHook) Execute(_ context.Context, _ *hooks.HookInput) (*hooks.HookResult, error) {
	return &hooks.HookResult{Decision: hooks.Approve}, nil
}

func (*approveConnHook) ConnectionStageOK() {}

// buildPipelineWithConnectionHook constructs an AgentPipeline whose resolver
// has a single connection-stage hook bound to the given stub. Uses an
// isolated hook registry to avoid mutating the package-global hooks.Registry.
func buildPipelineWithConnectionHook(t *testing.T, h hooks.Hook) *AgentPipeline {
	t.Helper()
	registry := hooks.NewHookRegistry()
	registry.Register("stub-conn", func(_ *hooks.HookConfig) (hooks.Hook, error) {
		return h, nil
	})
	p := newAgentPipelineWithRegistry(silentLogger(), registry)

	// Seat a config with the connection-stage hook via the shadow path so
	// we exercise the same codepath production does.
	payload := map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{
				ID:                "h1",
				ImplementationID:  "stub-conn",
				Name:              "stub-connection-hook",
				Stage:             "connection",
				Enabled:           true,
				FailBehavior:      "fail-open",
				ApplicableIngress: []string{"ALL"},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := p.ApplyHooksShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyHooksShadowState: %v", err)
	}
	return p
}

func TestAgentPipeline_EvaluateConnection_Reject(t *testing.T) {
	p := buildPipelineWithConnectionHook(t, &rejectAllConnHook{reason: "host not allowed"})

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "evil.example.com",
		SNI:        "evil.example.com",
	})
	if !blocked {
		t.Fatalf("expected blocked=true, got blocked=%v reason=%q", blocked, reason)
	}
	if reason == "" {
		t.Fatal("expected non-empty reason on RejectHard")
	}
	if reason != "host not allowed" {
		t.Fatalf("reason = %q, want %q", reason, "host not allowed")
	}
}

func TestAgentPipeline_EvaluateConnection_Reject_EmptyReasonFallback(t *testing.T) {
	// A hook that returns RejectHard with no reason should receive the
	// default "connection blocked by compliance policy" string so callers
	// always have something to log.
	p := buildPipelineWithConnectionHook(t, &rejectAllConnHook{reason: ""})

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "evil.example.com",
		SNI:        "evil.example.com",
	})
	if !blocked {
		t.Fatalf("expected blocked=true, got blocked=%v", blocked)
	}
	if reason == "" {
		t.Fatal("expected default reason when hook's Reason is empty")
	}
}

func TestAgentPipeline_EvaluateConnection_Approve(t *testing.T) {
	p := buildPipelineWithConnectionHook(t, &approveConnHook{})

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "api.openai.com",
		SNI:        "api.openai.com",
	})
	if blocked {
		t.Fatalf("expected blocked=false on Approve, got blocked=true reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason on Approve, got %q", reason)
	}
}

func TestAgentPipeline_EvaluateConnection_NoHooks(t *testing.T) {
	// Fresh pipeline: no hook configs have been applied, so
	// BuildPipeline("connection", ...) returns (nil, nil) and we fail open.
	p := NewAgentPipeline(silentLogger())

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "api.openai.com",
		SNI:        "api.openai.com",
	})
	if blocked {
		t.Fatalf("expected blocked=false when no hooks configured, got blocked=true reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}

func TestAgentPipeline_EvaluateConnection_FreshPipeline_FailsOpen(t *testing.T) {
	// A pipeline that has never received hook configs (no Apply* call)
	// still serves a non-nil empty PolicyResolver via its
	// HookConfigCache. EvaluateConnection must return blocked=false in
	// that state, matching the fail-open policy used by ai-gateway and
	// compliance-proxy.
	p := NewAgentPipeline(silentLogger())

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "api.openai.com",
	})
	if blocked {
		t.Fatalf("expected blocked=false on fresh pipeline, got blocked=true reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason on fresh pipeline, got %q", reason)
	}
}

func TestAgentPipeline_EvaluateConnection_PopulatesTLSInfo(t *testing.T) {
	// Assert that the hook receives an input carrying SNI and source IP —
	// this is the contract future consumers (host-blocklist, ip-access)
	// will rely on.
	var captured atomic.Pointer[hooks.HookInput]
	capturing := &capturingConnHook{seen: &captured}

	p := buildPipelineWithConnectionHook(t, capturing)

	_, _ = p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		RequestID:             "req-42",
		SourceIP:              "192.0.2.9",
		TargetHost:            "api.openai.com",
		SNI:                   "api.openai.com",
		ClientCertFingerprint: "sha256:abc",
	})

	seen := captured.Load()
	if seen == nil {
		t.Fatal("expected hook to be invoked")
	}
	if seen.Stage != "connection" {
		t.Fatalf("Stage = %q, want connection", seen.Stage)
	}
	if seen.IngressType != "AGENT" {
		t.Fatalf("IngressType = %q, want AGENT", seen.IngressType)
	}
	if seen.SourceIP != "192.0.2.9" {
		t.Fatalf("SourceIP = %q, want 192.0.2.9", seen.SourceIP)
	}
	if seen.TargetHost != "api.openai.com" {
		t.Fatalf("TargetHost = %q, want api.openai.com", seen.TargetHost)
	}
	if seen.TLS == nil {
		t.Fatal("TLS info must be populated on connection stage")
	}
	if seen.TLS.SNI != "api.openai.com" {
		t.Fatalf("TLS.SNI = %q, want api.openai.com", seen.TLS.SNI)
	}
	if seen.TLS.ClientCertFingerprint != "sha256:abc" {
		t.Fatalf("TLS.ClientCertFingerprint = %q, want sha256:abc", seen.TLS.ClientCertFingerprint)
	}
	if seen.RequestID != "req-42" {
		t.Fatalf("RequestID = %q, want req-42", seen.RequestID)
	}
}

type capturingConnHook struct {
	hooks.AnyEndpointAnyModality
	seen *atomic.Pointer[hooks.HookInput]
}

func (h *capturingConnHook) Execute(_ context.Context, in *hooks.HookInput) (*hooks.HookResult, error) {
	// Copy to isolate from any in-place mutation by the pipeline.
	cp := *in
	h.seen.Store(&cp)
	return &hooks.HookResult{Decision: hooks.Approve}, nil
}

func (*capturingConnHook) ConnectionStageOK() {}
