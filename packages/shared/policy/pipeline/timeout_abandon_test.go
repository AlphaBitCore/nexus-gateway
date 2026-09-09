package pipeline

// A hook that does not honour its context must not be able to hold the request.
//
// safeHookExecute calls Execute synchronously, so a hook that ignores ctx runs
// to completion whatever its deadline says — and none of the built-in scanning
// hooks check ctx.Done() (pii-detector, keyword-filter, content-safety,
// rulepack-engine all ignore it; only webhook-forward honours it, via
// http.NewRequestWithContext). The per-hook timeout therefore classified
// nothing on the path where it matters most: the caller waited, and the
// fail-closed decision it was supposed to reach never happened, because
// reaching it required the hook to return first.
//
// For a compliance gateway that is the worst of the three outcomes. A refusal
// is recoverable and visible; unscanned traffic is neither; but a request that
// simply hangs delivers no decision, no audit row saying we refused, and a
// client that retries into the same wall.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// hangingHook ignores its context entirely, the way every built-in scanning
// hook does, and takes far longer than its configured timeout.
type hangingHook struct {
	dwell   time.Duration
	entered chan struct{}
}

func (h *hangingHook) Name() string { return "hanging" }
func (h *hangingHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	time.Sleep(h.dwell) // deliberately not ctx-aware
	return &core.HookResult{Decision: core.Approve}, nil
}
func (h *hangingHook) SupportsEndpoint(core.EndpointType) bool { return true }
func (h *hangingHook) SupportsModality(core.Modality) bool     { return true }

func hangingPipeline(t *testing.T, dwell, hookTimeout time.Duration, failBehavior string) *Pipeline {
	t.Helper()
	cfg := &core.HookConfig{
		ID: "hang-1", Name: "hanging", ImplementationID: "noop",
		Enabled: true, Stage: "request", Priority: 10,
		FailBehavior: failBehavior,
		TimeoutMs:    int(hookTimeout / time.Millisecond),
	}
	return NewPipeline(
		[]boundHook{{hook: &hangingHook{dwell: dwell, entered: make(chan struct{}, 1)}, config: cfg}},
		hookTimeout,
		30*time.Second, // the production totalTimeout default
		false,
		slog.Default(),
	)
}

// The deadline the hook was given must bound how long Execute takes, not just
// what it reports afterwards. 50ms budget, 400ms hook: Execute has to come back
// long before the hook does.
func TestExecute_AbandonsAHookThatOutlivesItsTimeout(t *testing.T) {
	p := hangingPipeline(t, 400*time.Millisecond, 50*time.Millisecond, "fail-closed")

	start := time.Now()
	res := p.Execute(context.Background(), &core.HookInput{})
	elapsed := time.Since(start)

	if elapsed > 250*time.Millisecond {
		t.Fatalf("Execute waited %v for a hook whose timeout was 50ms — the deadline "+
			"is advisory, so a hook that ignores its context holds the request", elapsed)
	}
	if res == nil {
		t.Fatal("Execute returned nil after abandoning the hook; the caller has no decision to act on")
	}
}

// Abandoning is not the same as approving. A fail-closed hook that never
// produced a verdict must not leave the pipeline saying the content is fine —
// that is the silent compliance gap fail-closed exists to prevent.
func TestExecute_AbandonedFailClosedHookDoesNotApprove(t *testing.T) {
	p := hangingPipeline(t, 400*time.Millisecond, 50*time.Millisecond, "fail-closed")

	res := p.Execute(context.Background(), &core.HookInput{})
	if res == nil {
		t.Fatal("no result")
	}
	if res.Decision == core.Approve {
		t.Fatalf("a fail-closed hook was abandoned mid-flight and the pipeline still "+
			"approved: decision=%v — nothing scanned this content", res.Decision)
	}
}

// The mirror case: a fail-open hook that is abandoned must not turn a healthy
// request into a refusal. Availability is the declared posture for those.
func TestExecute_AbandonedFailOpenHookStillApproves(t *testing.T) {
	p := hangingPipeline(t, 400*time.Millisecond, 50*time.Millisecond, "fail-open")

	res := p.Execute(context.Background(), &core.HookInput{})
	if res == nil {
		t.Fatal("no result")
	}
	if res.Decision != core.Approve {
		t.Fatalf("a fail-open hook was abandoned and the pipeline refused the request: "+
			"decision=%v — fail-open means availability wins", res.Decision)
	}
}
