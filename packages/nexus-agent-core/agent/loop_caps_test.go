package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// toolUseBlock is one tool_use block for a multi-call assistant response.
type toolUseBlock struct{ id, name, args string }

// asstToolUses builds an assistant ModelResponse that calls several tools in ONE
// response — the shape that drives per-round fan-out (F-0291) and the panic
// barrier (F-0292).
func asstToolUses(blocks ...toolUseBlock) *ModelResponse {
	bs := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		bs = append(bs, Block{Type: BlockToolUse, ID: b.id, ToolName: b.name, Input: json.RawMessage(b.args)})
	}
	return &ModelResponse{Message: Message{Role: RoleAssistant, Blocks: bs}, StopReason: StopToolUse}
}

// TestLoopMaxToolCallsPerRound proves the per-round semaphore caps CONCURRENCY: a
// response that emits 10 auto-tier tool calls with MaxToolCallsPerRound=3 must
// never have more than 3 running at once, yet all 10 still complete.
func TestLoopMaxToolCallsPerRound(t *testing.T) {
	const total = 10
	const cap3 = 3
	var inflight int32
	var maxObserved int32
	gate := make(chan struct{}) // released once, after we've let concurrency build

	tool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		cur := atomic.AddInt32(&inflight, 1)
		for {
			prev := atomic.LoadInt32(&maxObserved)
			if cur <= prev || atomic.CompareAndSwapInt32(&maxObserved, prev, cur) {
				break
			}
		}
		<-gate // hold the slot so concurrent calls pile up against the semaphore
		atomic.AddInt32(&inflight, -1)
		return Result{Content: "ok"}, nil
	}}
	reg := NewRegistry()
	reg.Register(tool)

	blocks := make([]toolUseBlock, total)
	for i := range blocks {
		blocks[i] = toolUseBlock{id: fmt.Sprintf("u%d", i), name: "observe_cost", args: `{}`}
	}
	fm := newFakeModel(asstToolUses(blocks...), asstText("done"))
	l := &Loop{Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), StepCap: 8, MaxToolCallsPerRound: cap3}

	done := make(chan struct{})
	go func() {
		_, _, _ = l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
		close(done)
	}()

	// Give the loop time to saturate the semaphore, then release everything.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&maxObserved) < cap3 {
		select {
		case <-deadline:
			t.Fatalf("never reached the concurrency cap; maxObserved=%d", atomic.LoadInt32(&maxObserved))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(gate)
	<-done

	if got := atomic.LoadInt32(&maxObserved); got > cap3 {
		t.Fatalf("peak concurrency %d exceeded MaxToolCallsPerRound=%d", got, cap3)
	}
	if got := atomic.LoadInt32(&tool.calls); got != total {
		t.Fatalf("all %d tools must still run; ran %d", total, got)
	}
}

// TestLoopMaxToolCallsPerTurn proves the per-turn ceiling stops issuing tool calls
// once the cumulative count is reached and returns the structured limit signal for
// the dropped blocks — across MULTIPLE rounds (the counter spans the turn).
func TestLoopMaxToolCallsPerTurn(t *testing.T) {
	var ran int32
	tool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		atomic.AddInt32(&ran, 1)
		return Result{Content: "ok"}, nil
	}}
	reg := NewRegistry()
	reg.Register(tool)

	// Round 1: 3 calls. Round 2: 3 calls. With MaxToolCallsPerTurn=4, exactly 4 run
	// (3 in round 1, 1 in round 2) and the rest are limit-signalled.
	r1 := asstToolUses(
		toolUseBlock{"a", "observe_cost", `{}`},
		toolUseBlock{"b", "observe_cost", `{}`},
		toolUseBlock{"c", "observe_cost", `{}`},
	)
	r2 := asstToolUses(
		toolUseBlock{"d", "observe_cost", `{}`},
		toolUseBlock{"e", "observe_cost", `{}`},
		toolUseBlock{"f", "observe_cost", `{}`},
	)
	fm := newFakeModel(r1, r2, asstText("done"))
	l := &Loop{Model: fm, Registry: reg, Gate: NewGate(nil, nil, false), StepCap: 8, MaxToolCallsPerTurn: 4}

	produced, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&ran); got != 4 {
		t.Fatalf("MaxToolCallsPerTurn=4 must run exactly 4 tools, ran %d", got)
	}
	// The dropped blocks must carry the structured limit signal as an error result.
	var sawLimit int
	for _, m := range produced {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && strings.Contains(b.Text, "tool call limit exceeded") {
				if !b.IsError {
					t.Fatalf("limit result must be IsError")
				}
				sawLimit++
			}
		}
	}
	if sawLimit != 2 {
		t.Fatalf("want 2 limit-exceeded results, got %d", sawLimit)
	}
}

// TestLoopPanicBarrier proves a tool that panics is contained: the panic is
// recovered, converted to an IsError tool_result, and the turn survives (the
// shared CP process must NOT crash).
func TestLoopPanicBarrier(t *testing.T) {
	panicTool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		panic("boom in tool")
	}}
	reg := NewRegistry()
	reg.Register(panicTool)
	fm := newFakeModel(asstToolUse("u1", "observe_cost", `{}`), asstText("recovered"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)

	produced, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
	if err != nil {
		t.Fatalf("a panicking tool must NOT fail the turn, got %v", err)
	}
	var sawPanic bool
	for _, m := range produced {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && strings.Contains(b.Text, "tool panicked: boom in tool") {
				if !b.IsError {
					t.Fatalf("panic result must be IsError")
				}
				sawPanic = true
			}
		}
	}
	if !sawPanic {
		t.Fatalf("expected a recovered panic tool_result, produced=%+v", produced)
	}
}

// TestLoopPanicBarrierConfirmTier proves the barrier also covers the synchronous
// confirm-tier path (a panic there runs on the loop goroutine and would otherwise
// crash the process).
func TestLoopPanicBarrierConfirmTier(t *testing.T) {
	panicTool := &stubTool{name: "mitigate_x", tier: TierConfirm, run: func(json.RawMessage) (Result, error) {
		panic("boom in confirm tool")
	}}
	reg := NewRegistry()
	reg.Register(panicTool)
	fm := newFakeModel(asstToolUse("u1", "mitigate_x", `{}`), asstText("recovered"))
	confirm := func(context.Context, Tool, json.RawMessage, string) (bool, error) { return true, nil }
	l := newLoop(fm, reg, NewGate(nil, nil, false), confirm)

	produced, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
	if err != nil {
		t.Fatalf("a panicking confirm-tier tool must NOT fail the turn, got %v", err)
	}
	var sawPanic bool
	for _, m := range produced {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && strings.Contains(b.Text, "tool panicked: boom in confirm tool") {
				sawPanic = true
			}
		}
	}
	if !sawPanic {
		t.Fatalf("expected the confirm-tier panic to be contained, produced=%+v", produced)
	}
}

// TestLoopRoundCapDefaultsApplied proves the <=0 fallbacks engage the package
// defaults (so a Loop built without the caps still runs every tool, bounded by the
// defaults rather than unbounded).
func TestLoopRoundCapDefaultsApplied(t *testing.T) {
	var ran int32
	tool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		atomic.AddInt32(&ran, 1)
		return Result{Content: "ok"}, nil
	}}
	reg := NewRegistry()
	reg.Register(tool)
	blocks := []toolUseBlock{
		{"a", "observe_cost", `{}`}, {"b", "observe_cost", `{}`}, {"c", "observe_cost", `{}`},
	}
	fm := newFakeModel(asstToolUses(blocks...), asstText("done"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil) // no caps set → defaults
	if _, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go")); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&ran); got != 3 {
		t.Fatalf("with defaults all 3 tools must run, ran %d", got)
	}
}
