// Package pipeline — the two ways a configured hook can stop guarding traffic
// while the gateway keeps serving, and the per-request evidence for each.
//
// Named failure modes:
//   - EVERY configured hook fails to build, so there is no pipeline to carry the
//     unbuildable tag and the request's audit row reads exactly like a tenant
//     who configured no hooks at all
//   - a hook that builds and runs but errors on most of the traffic around a
//     request leaves that request indistinguishable from one a healthy hook
//     approved
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// alwaysErrHook builds fine and then fails on every execution — the shape that
// produces a fail-open approve while scanning nothing.
type alwaysErrHook struct{ approveEverythingHook }

func (h *alwaysErrHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return nil, errors.New("upstream scanner unreachable")
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// When every configured hook fails to build, BuildPipeline has no pipeline to
// hang the tag on — so the list must travel out on its own, or the caller cannot
// tell "nothing configured" from "everything broken".
func TestBuildPipelineReportsUnbuildableEvenWithNoPipeline(t *testing.T) {
	reg := core.NewHookRegistry()
	configs := []core.HookConfig{
		{
			ID: "h1", Name: "a", ImplementationID: "detector-that-does-not-exist",
			Stage: "request", Enabled: true, ApplicableIngress: []string{"ALL"},
			FailBehavior: "fail-open",
		},
		{
			ID: "h2", Name: "b", ImplementationID: "another-missing-detector",
			Stage: "request", Enabled: true, ApplicableIngress: []string{"ALL"},
			FailBehavior: "fail-open",
		},
	}
	r := NewPolicyResolver(configs, reg, discard())
	p, unbuildable, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
		time.Second, 5*time.Second, false, true, discard())
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	if p != nil {
		t.Fatal("a pipeline was built from two unbuildable hooks — this test is asserting " +
			"nothing about the no-pipeline path")
	}
	if len(unbuildable) != 2 {
		t.Fatalf("unbuildable = %v, want both implementations — with no pipeline this list "+
			"is the ONLY way the caller learns the hooks were configured and broken", unbuildable)
	}
	tags := UnbuildableTags(unbuildable)
	if !slices.Contains(tags, "hook-unbuildable:detector-that-does-not-exist") {
		t.Errorf("tags = %v, want one naming each broken implementation", tags)
	}
}

// A pipeline that DID build still reports its unbuildable list to the caller, so
// the two producers of the tag agree on the same set.
func TestBuildPipelineReportsUnbuildableAlongsideALivePipeline(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("noop-ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &approveEverythingHook{}, nil
	})
	configs := []core.HookConfig{
		{ID: "h-ok", Name: "ok", ImplementationID: "noop-ok", Stage: "request",
			Enabled: true, ApplicableIngress: []string{"ALL"}, FailBehavior: "fail-open"},
		{ID: "h-bad", Name: "bad", ImplementationID: "missing-impl", Stage: "request",
			Enabled: true, ApplicableIngress: []string{"ALL"}, FailBehavior: "fail-open"},
	}
	r := NewPolicyResolver(configs, reg, discard())
	p, unbuildable, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
		time.Second, 5*time.Second, false, true, discard())
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	if p == nil {
		t.Fatal("setup: the buildable hook should have kept the pipeline alive")
	}
	if !slices.Contains(unbuildable, "missing-impl") {
		t.Errorf("unbuildable = %v, want it to name missing-impl", unbuildable)
	}
	// And the tag the merge stamps must be the same string the caller would
	// build from the returned list — one helper, so they cannot drift.
	res := p.Execute(context.Background(), &core.HookInput{Stage: "request"})
	if !slices.Contains(res.Tags, UnbuildableTags(unbuildable)[0]) {
		t.Errorf("result tags %v do not contain %q — the two producers of this tag have drifted",
			res.Tags, UnbuildableTags(unbuildable)[0])
	}
}

// A hook that keeps erroring is fail-open by policy, so every request it fails
// on is APPROVED and looks clean. The tag is the only per-request trace, and an
// audit query on it is how "what went out while the scanner was down" gets
// answered.
func TestDegradedHookTagsTheRequestsItWasFailingOn(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("flaky", func(_ *core.HookConfig) (core.Hook, error) {
		return &alwaysErrHook{}, nil
	})
	configs := []core.HookConfig{{
		ID: "h-flaky", Name: "flaky", ImplementationID: "flaky", Stage: "request",
		Enabled: true, ApplicableIngress: []string{"ALL"}, FailBehavior: "fail-open",
	}}
	r := NewPolicyResolver(configs, reg, discard())
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r.health.now = clock.now

	build := func() *Pipeline {
		p, _, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
			time.Second, 5*time.Second, false, false, discard())
		if err != nil || p == nil {
			t.Fatalf("BuildPipeline: %v (nil=%v)", err, p == nil)
		}
		return p
	}

	// The FIRST request must not be tagged: below the sample floor nothing is
	// known yet, and a detector that accuses on one failure is worse than none.
	first := build().Execute(context.Background(), &core.HookInput{Stage: "request"})
	if slices.ContainsFunc(first.Tags, func(s string) bool { return s == degradedTagPrefix+"flaky" }) {
		t.Error("the first failing request was tagged degraded — that is the sample floor " +
			"not holding, and every deploy would light it")
	}

	// Drive past the floor. Each request rebuilds the pipeline, exactly as the
	// gateway does, which is why the window has to live on the resolver.
	for range degradeMinSamples + 5 {
		clock.add(10 * time.Millisecond)
		build().Execute(context.Background(), &core.HookInput{Stage: "request"})
	}

	clock.add(10 * time.Millisecond)
	res := build().Execute(context.Background(), &core.HookInput{Stage: "request"})
	if res.Decision != core.Approve {
		t.Fatalf("decision = %s, want APPROVE — fail-open is the posture that makes this gap "+
			"invisible, and without it the tag is not the only signal", res.Decision)
	}
	if !slices.Contains(res.Tags, degradedTagPrefix+"flaky") {
		t.Errorf("tags = %v, want %q — a request approved by a hook that is failing on "+
			"everything is indistinguishable from a clean one without it",
			res.Tags, degradedTagPrefix+"flaky")
	}
}

// The control. A hook that works must never be tagged, or the tag means nothing
// and an audit query on it returns the whole table.
func TestHealthyHookIsNeverTaggedDegraded(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("noop-ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &approveEverythingHook{}, nil
	})
	configs := []core.HookConfig{{
		ID: "h-ok", Name: "ok", ImplementationID: "noop-ok", Stage: "request",
		Enabled: true, ApplicableIngress: []string{"ALL"}, FailBehavior: "fail-open",
	}}
	r := NewPolicyResolver(configs, reg, discard())
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r.health.now = clock.now

	var res *core.CompliancePipelineResult
	for range degradeMinSamples + 10 {
		clock.add(10 * time.Millisecond)
		p, _, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
			time.Second, 5*time.Second, false, false, discard())
		if err != nil || p == nil {
			t.Fatalf("BuildPipeline: %v", err)
		}
		res = p.Execute(context.Background(), &core.HookInput{Stage: "request"})
	}
	for _, tag := range res.Tags {
		if tag == degradedTagPrefix+"noop-ok" {
			t.Errorf("a hook that succeeded on every execution was tagged degraded; tags=%v", res.Tags)
		}
	}
}
