package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// R2 confirm seam (AC-4). The kernel propagates ctx to Confirm and treats both a
// ctx-cancel and a Confirm error as a fail-safe DENY. Confirm-timeout policy lives
// in the Confirm IMPLEMENTATION (web: a pending-confirm deadline; CLI: the turn
// ctx) — the kernel's contract is only "honor ctx, error == deny". These tests are
// the missing AC-4 evidence: out-of-band async approval runs; ctx timeout denies
// without leaking the turn goroutine; a Confirm error denies.

// A Confirm that errors must be treated as deny — a confirm-backend failure must
// never be mistaken for an approval.
func TestLoopConfirmErrorDenies(t *testing.T) {
	mit := &stubTool{name: "mitigate_kill", tier: TierConfirm}
	reg := NewRegistry()
	reg.Register(mit)
	fm := newFakeModel(asstToolUse("u1", "mitigate_kill", `{}`), asstText("ok"))
	l := newLoop(fm, reg, NewGate(nil, nil, false),
		func(context.Context, Tool, json.RawMessage, string) (bool, error) {
			return true, errors.New("confirm backend failed")
		})
	hist, _, _ := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "kill it"))
	if atomic.LoadInt32(&mit.calls) != 0 {
		t.Fatal("a Confirm that errors must be treated as deny (fail-safe); tool must NOT run")
	}
	if !hasToolResultContaining(hist, "declined") {
		t.Fatal("confirm error must feed a 'declined' result so the model adapts")
	}
}

// A Confirm parked awaiting an out-of-band decision must deny when the turn ctx
// times out, and the turn (Run) must return promptly — no goroutine wedged on the
// reply channel forever.
func TestLoopConfirmCtxTimeoutDeniesNoLeak(t *testing.T) {
	mit := &stubTool{name: "mitigate_kill", tier: TierConfirm}
	reg := NewRegistry()
	reg.Register(mit)
	fm := newFakeModel(asstToolUse("u1", "mitigate_kill", `{}`), asstText("ok"))
	// The web Confirm's shape: park awaiting an out-of-band POST; give up on ctx.
	confirm := func(ctx context.Context, _ Tool, _ json.RawMessage, _ string) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	l := newLoop(fm, reg, NewGate(nil, nil, false), confirm)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _, _ = l.Run(ctx, "SYS", nil, TextMessage(RoleUser, "kill it"))
		close(done)
	}()
	select {
	case <-done:
		// Run returned after the confirm's ctx timed out — no leak.
	case <-time.After(2 * time.Second):
		t.Fatal("Run leaked: it did not return after the parked confirm's ctx timed out")
	}
	if atomic.LoadInt32(&mit.calls) != 0 {
		t.Fatal("a confirm that times out must deny; the write tool must NOT run")
	}
}

// An out-of-band approval (the web POST /confirm shape) that arrives while Confirm
// is parked must let the tool run.
func TestLoopConfirmAsyncResolveRuns(t *testing.T) {
	ran := make(chan struct{}, 1)
	mit := &stubTool{name: "mitigate_kill", tier: TierConfirm, run: func(json.RawMessage) (Result, error) {
		ran <- struct{}{}
		return Result{Content: "engaged"}, nil
	}}
	reg := NewRegistry()
	reg.Register(mit)
	fm := newFakeModel(asstToolUse("u1", "mitigate_kill", `{}`), asstText("done"))
	reply := make(chan bool)
	confirm := func(ctx context.Context, _ Tool, _ json.RawMessage, _ string) (bool, error) {
		select {
		case ok := <-reply:
			return ok, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	l := newLoop(fm, reg, NewGate(nil, nil, false), confirm)
	go func() { time.Sleep(15 * time.Millisecond); reply <- true }() // out-of-band authorize
	l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "kill it"))
	select {
	case <-ran:
	default:
		t.Fatal("an out-of-band approval must let the confirm-tier tool run")
	}
}
