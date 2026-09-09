// Package pipeline — a hook that could not be built must say so on every
// request the pipeline serves.
//
// Named failure modes:
//   - a hook the operator configured never runs, and nothing in the request's
//     audit record distinguishes that from "no hook was configured"
//   - the only signal is a startup log deduplicated per reload epoch, so a
//     persistently-broken hook is silent from its second minute onward
package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestUnbuildableHookTagsEveryResult is the gate.
//
// The pipeline is built from two configs: one that resolves, and one whose
// implementationId has no factory. The buildable hook keeps the pipeline alive
// — which is the availability-first posture, and exactly why the gap is
// invisible without this — so the result is an ordinary APPROVE. What must
// differ from a healthy pipeline is the tag set.
func TestUnbuildableHookTagsEveryResult(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("noop-ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &approveEverythingHook{}, nil
	})

	configs := []core.HookConfig{
		{
			ID: "h-ok", Name: "ok", ImplementationID: "noop-ok",
			Stage: "request", Enabled: true, ApplicableIngress: []string{"ALL"},
			FailBehavior: "fail-open",
		},
		{
			ID: "h-missing", Name: "missing", ImplementationID: "detector-that-does-not-exist",
			Stage: "request", Enabled: true, ApplicableIngress: []string{"ALL"},
			// fail-open, so the strict build does NOT refuse — this is the
			// silent path, not the loud one.
			FailBehavior: "fail-open",
		},
	}

	r := NewPolicyResolver(configs, reg, slog.New(slog.DiscardHandler))
	p, _, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
		time.Second, 5*time.Second, false, true, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	if p == nil {
		t.Fatal("pipeline is nil — the buildable hook should have kept it alive, and with no " +
			"pipeline this test would be asserting nothing")
	}

	res := p.Execute(context.Background(), &core.HookInput{Stage: "request"})
	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if res.Decision != core.Approve {
		t.Fatalf("decision = %s, want APPROVE — the point is that the gap is INVISIBLE in the "+
			"decision, which is why it has to be visible in the tags", res.Decision)
	}

	var found string
	for _, tag := range res.Tags {
		if strings.HasPrefix(tag, "hook-unbuildable:") {
			found = tag
		}
	}
	if found == "" {
		t.Errorf("no hook-unbuildable tag on the result. A hook the operator configured did not "+
			"run, the request went out unguarded by it, and the audit row is indistinguishable "+
			"from one where no such hook was ever configured. tags = %v", res.Tags)
	}
	if found != "" && !strings.Contains(found, "detector-that-does-not-exist") {
		t.Errorf("tag %q does not name the implementation that failed — an operator cannot act "+
			"on 'something was unbuildable'", found)
	}
}

// TestHealthyPipelineCarriesNoUnbuildableTag is the other half. A tag that is
// always present says nothing; this pins that it appears only when a hook
// really failed to build.
func TestHealthyPipelineCarriesNoUnbuildableTag(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("noop-ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &approveEverythingHook{}, nil
	})

	r := NewPolicyResolver([]core.HookConfig{{
		ID: "h-ok", Name: "ok", ImplementationID: "noop-ok",
		Stage: "request", Enabled: true, ApplicableIngress: []string{"ALL"},
		FailBehavior: "fail-open",
	}}, reg, slog.New(slog.DiscardHandler))

	p, _, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
		time.Second, 5*time.Second, false, true, slog.New(slog.DiscardHandler))
	if err != nil || p == nil {
		t.Fatalf("BuildPipeline: p=%v err=%v", p, err)
	}
	res := p.Execute(context.Background(), &core.HookInput{Stage: "request"})
	for _, tag := range res.Tags {
		if strings.HasPrefix(tag, "hook-unbuildable:") {
			t.Errorf("a fully-resolved pipeline reported %q — a tag that is always there cannot "+
				"be alerted on", tag)
		}
	}
}

// approveEverythingHook is the buildable half of the pair: it keeps the pipeline
// alive so the unbuildable one is the only difference under test.
type approveEverythingHook struct {
	core.AnyEndpointAnyModality
}

func (*approveEverythingHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}
