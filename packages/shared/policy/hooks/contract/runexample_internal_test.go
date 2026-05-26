package contract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// fakeHook is a deterministic Hook used to drive each failure branch of
// runExample without depending on a real built-in implementation.
type fakeHook struct {
	core.AnyEndpointAnyModality
	result *core.HookResult
	err    error
}

func (f *fakeHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return f.result, f.err
}

// newRegistry builds an unfrozen registry seeded with the given factory
// for testing. Using Clone() preserves the global Registry's contents
// (so the global stays usable in parallel) and Register() (not Replace())
// panics on duplicate IDs — surfacing test-setup bugs early.
func newRegistry(implID string, factory core.HookFactory) *core.HookRegistry {
	r := core.NewHookRegistry()
	if factory != nil {
		r.Register(implID, factory)
	}
	return r
}

// TestRunExample_HappyPath pins that a fixture whose factory + Execute
// + Decision all align with ExpectedDecision returns nil. The four
// negative tests below each flip one of those facets — together they
// cover every branch in runExample.
func TestRunExample_HappyPath(t *testing.T) {
	ex := Example{
		Name: "happy",
		Config: core.HookConfig{
			ID: "x-1", ImplementationID: "fake-ok",
			Name: "ok", Stage: "request", Priority: 1, Enabled: true,
		},
		Input:            &core.HookInput{Stage: "request", IngressType: "AI_GATEWAY"},
		ExpectedDecision: core.Approve,
	}
	reg := newRegistry("fake-ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &fakeHook{result: &core.HookResult{Decision: core.Approve}}, nil
	})
	if err := runExample(reg, ex); err != nil {
		t.Fatalf("happy path should not error: %v", err)
	}
}

// TestRunExample_MissingFactory pins the first failure branch: when a
// fixture names an ImplementationID that the consumer's registry never
// registered, runExample must return an error mentioning the impl ID.
// This is the most common drift mode — a service forgets to register a
// new hook but still pulls the shared contract Examples().
func TestRunExample_MissingFactory(t *testing.T) {
	ex := Example{
		Name: "no-factory",
		Config: core.HookConfig{
			ID: "x-2", ImplementationID: "not-registered",
		},
	}
	reg := newRegistry("something-else", func(_ *core.HookConfig) (core.Hook, error) {
		return &fakeHook{result: &core.HookResult{Decision: core.Approve}}, nil
	})
	err := runExample(reg, ex)
	if err == nil {
		t.Fatal("missing factory should return error")
	}
	if !strings.Contains(err.Error(), `"not-registered"`) {
		t.Errorf("error should name the missing impl id; got: %v", err)
	}
	if !strings.Contains(err.Error(), "no factory registered") {
		t.Errorf("error should mention 'no factory registered'; got: %v", err)
	}
}

// TestRunExample_FactoryError pins the second failure branch: when a
// factory rejects its config (typo'd field, missing required parameter,
// invalid regex compile, …), runExample must wrap that error and tag it
// with the fixture name so the operator can locate the broken Example.
func TestRunExample_FactoryError(t *testing.T) {
	factoryErr := errors.New("bad config: missing patterns")
	ex := Example{
		Name: "factory-bad-config",
		Config: core.HookConfig{
			ID: "x-3", ImplementationID: "fake-rejects",
		},
	}
	reg := newRegistry("fake-rejects", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, factoryErr
	})
	err := runExample(reg, ex)
	if err == nil {
		t.Fatal("factory error should propagate")
	}
	if !errors.Is(err, factoryErr) {
		t.Errorf("error should wrap the underlying factory error; got: %v", err)
	}
	if !strings.Contains(err.Error(), "factory-bad-config") {
		t.Errorf("error should carry fixture name; got: %v", err)
	}
}

// TestRunExample_ExecuteError pins the third failure branch: when the
// hook's Execute returns an error (transient downstream issue,
// programming bug, etc.), runExample must wrap that error so the
// fail-open vs fail-closed gate in production is observable from the
// suite output.
func TestRunExample_ExecuteError(t *testing.T) {
	execErr := errors.New("downstream service unavailable")
	ex := Example{
		Name: "execute-fails",
		Config: core.HookConfig{
			ID: "x-4", ImplementationID: "fake-exec-err",
		},
		Input:            &core.HookInput{Stage: "request"},
		ExpectedDecision: core.Approve,
	}
	reg := newRegistry("fake-exec-err", func(_ *core.HookConfig) (core.Hook, error) {
		return &fakeHook{err: execErr}, nil
	})
	err := runExample(reg, ex)
	if err == nil {
		t.Fatal("execute error should propagate")
	}
	if !errors.Is(err, execErr) {
		t.Errorf("error should wrap the underlying execute error; got: %v", err)
	}
	if !strings.Contains(err.Error(), "Execute error") {
		t.Errorf("error should mention 'Execute error' so operators can locate the branch; got: %v", err)
	}
}

// TestRunExample_DecisionMismatch pins the fourth and most important
// failure branch: the hook ran cleanly but emitted a Decision that
// disagrees with the fixture's ExpectedDecision. This is the canonical
// "schema drift" signal — a refactor renamed/repurposed a Decision
// constant or a hook silently flipped from RejectHard to Approve. The
// error message must include both the actual and expected values so
// the operator can diagnose without re-running.
func TestRunExample_DecisionMismatch(t *testing.T) {
	ex := Example{
		Name: "decision-drift",
		Config: core.HookConfig{
			ID: "x-5", ImplementationID: "fake-wrong-decision",
		},
		Input:            &core.HookInput{Stage: "request"},
		ExpectedDecision: core.RejectHard,
	}
	reg := newRegistry("fake-wrong-decision", func(_ *core.HookConfig) (core.Hook, error) {
		return &fakeHook{result: &core.HookResult{Decision: core.Approve}}, nil
	})
	err := runExample(reg, ex)
	if err == nil {
		t.Fatal("decision mismatch should error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "decision-drift") {
		t.Errorf("error should include fixture name; got: %v", err)
	}
	if !strings.Contains(msg, string(core.Approve)) {
		t.Errorf("error should include actual decision %q; got: %v", core.Approve, err)
	}
	if !strings.Contains(msg, string(core.RejectHard)) {
		t.Errorf("error should include expected decision %q; got: %v", core.RejectHard, err)
	}
}
