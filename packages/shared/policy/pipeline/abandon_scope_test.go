package pipeline

// Abandoning a chain answers for the hooks that never reported. It must answer
// for the ones that would have RUN — and runSequential deliberately skips a hook
// whose applicableTrafficKinds exclude this traffic, "so the audit row does not
// show a phantom hook execution".
//
// The abandonment path had no such filter. Every hook from the abandoned index
// onward got a synthesised verdict, including ones that were never going to be
// consulted, and under fail-closed that verdict is a refusal. A hook scoped to
// http traffic could therefore block an AI request it would never have looked
// at — and the audit row would name it as the blocker.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// inapplicableHook would refuse everything it saw. The point of the test is that
// it never sees anything: its scope excludes this traffic.
type inapplicableHook struct{}

func (inapplicableHook) Name() string { return "http-only" }
func (inapplicableHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.RejectHard, Reason: "http-only hook ran"}, nil
}
func (inapplicableHook) SupportsEndpoint(core.EndpointType) bool { return true }
func (inapplicableHook) SupportsModality(core.Modality) bool     { return true }

func TestAbandonDoesNotSynthesiseVerdictsForInapplicableHooks(t *testing.T) {
	const budget = 50 * time.Millisecond

	hanging := &core.HookConfig{
		ID: "hang-1", Name: "hanging", ImplementationID: "noop",
		Enabled: true, Stage: "request", Priority: 10,
		FailBehavior: "fail-closed", TimeoutMs: int(budget / time.Millisecond),
	}
	httpOnly := &core.HookConfig{
		ID: "http-1", Name: "http-only", ImplementationID: "noop",
		Enabled: true, Stage: "request", Priority: 20,
		FailBehavior: "fail-closed", TimeoutMs: int(budget / time.Millisecond),
		// This traffic is AI; the hook is scoped to plain HTTP.
		ApplicableTrafficKinds: []string{"http"},
	}

	p := NewPipeline(
		[]boundHook{
			{hook: &hangingHook{dwell: 400 * time.Millisecond, entered: make(chan struct{}, 1)}, config: hanging},
			{hook: inapplicableHook{}, config: httpOnly},
		},
		budget, 30*time.Second, false, slog.Default(),
	)

	input := &core.HookInput{
		Normalized: &normalize.NormalizedPayload{
			Kind:             normalize.KindAIChat,
			NormalizeVersion: normalize.SchemaVersion,
		},
	}
	res := p.Execute(context.Background(), input)
	if res == nil {
		t.Fatal("no result")
	}

	for _, hr := range res.HookResults {
		if hr.HookID == "http-1" {
			t.Errorf("the abandonment path answered for %q, a hook scoped to http traffic that "+
				"this AI request would never have consulted (decision=%v reason=%q).\n"+
				"runSequential skips it precisely so the audit shows no phantom execution; "+
				"under fail-closed the synthesised verdict is a refusal, so a hook that was "+
				"never going to look at this request blocks it and is named as the blocker.",
				hr.HookID, hr.Decision, hr.Reason)
		}
	}

	// The control: the hook that WAS applicable and did not report must still be
	// answered for, or abandonment would have become a silent approval.
	var sawHanging bool
	for _, hr := range res.HookResults {
		if hr.HookID == "hang-1" {
			sawHanging = true
		}
	}
	if !sawHanging {
		t.Error("the abandoned applicable hook produced no result at all; a fail-closed hook " +
			"that never ran must not leave the pipeline silent about it")
	}
}
