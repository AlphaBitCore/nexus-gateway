package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cancelOnGenModel cancels the turn from inside Generate (simulating an esc-interrupt
// that lands the instant the model finishes answering) and returns a tool-use turn,
// so the loop reaches its post-answer ctx.Err() guard before any tool runs.
type cancelOnGenModel struct {
	cancel context.CancelFunc
	resp   *ModelResponse
}

func (m *cancelOnGenModel) Generate(_ context.Context, _ ModelRequest, _, _ func(string)) (*ModelResponse, error) {
	m.cancel()
	return m.resp, nil
}

// TestLoopCancelBetweenAnswerAndTools covers the mid-turn cancel branch: when the
// turn is cancelled after the model proposes tool calls but before they execute, the
// loop must return the cancellation and run NO tool (the esc-interrupt must take
// effect immediately, not after a full tool round).
func TestLoopCancelBetweenAnswerAndTools(t *testing.T) {
	var ran int32
	tool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		atomic.AddInt32(&ran, 1)
		return Result{Content: "x"}, nil
	}}
	reg := NewRegistry()
	reg.Register(tool)
	ctx, cancel := context.WithCancel(context.Background())
	m := &cancelOnGenModel{cancel: cancel, resp: asstToolUse("u1", "observe_cost", `{}`)}
	l := newLoop(m, reg, NewGate(nil, nil, false), nil)

	_, _, err := l.Run(ctx, "SYS", nil, TextMessage(RoleUser, "go"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a turn cancelled after the model proposed tools must return context.Canceled, got %v", err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatalf("no tool may run once the turn is cancelled before tool execution, ran=%d", ran)
	}
}

func newLoop(model Model, reg *Registry, gate *Gate, confirm ConfirmFunc) *Loop {
	return &Loop{Model: model, Registry: reg, Gate: gate, StepCap: 8, Confirm: confirm}
}

// 1. A turn that ends with text (no tool_use) returns that text and never loops.
func TestLoopPlainAnswer(t *testing.T) {
	fm := newFakeModel(asstText("p95 is 90ms"))
	l := newLoop(fm, NewRegistry(), NewGate(nil, nil, false), nil)
	hist, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "what is p95?"))
	if err != nil {
		t.Fatal(err)
	}
	if fm.calls != 1 {
		t.Fatalf("plain answer = 1 model call, got %d", fm.calls)
	}
	last := hist[len(hist)-1]
	if last.Role != RoleAssistant || !strings.Contains(last.Text(), "90ms") {
		t.Fatalf("final message should be the assistant answer, got %+v", last)
	}
}

// 2. tool_use → run tool → feed tool_result → model answers.
func TestLoopRunsToolAndFeedsResult(t *testing.T) {
	tool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		return Result{Content: "anthropic $3/hr"}, nil
	}}
	reg := NewRegistry()
	reg.Register(tool)
	fm := newFakeModel(
		asstToolUse("u1", "observe_cost", `{"window":"1h"}`),
		asstText("Your top cost is anthropic at $3/hr."),
	)
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)
	hist, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "cost?"))
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&tool.calls) != 1 {
		t.Fatalf("tool should run once, got %d", tool.calls)
	}
	var found *Block
	for _, m := range hist {
		for i := range m.Blocks {
			if m.Blocks[i].Type == BlockToolResult {
				found = &m.Blocks[i]
			}
		}
	}
	if found == nil || found.ID != "u1" || !strings.Contains(found.Text, "$3/hr") {
		t.Fatalf("loop must feed a tool_result with the tool_use id, got %+v", found)
	}
	if len(fm.gotReqs) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(fm.gotReqs))
	}
	// The second model call's history includes both the assistant tool_use and
	// the tool_result we fed back.
	if len(fm.gotReqs[1].Messages) < 3 {
		t.Fatalf("second call must carry the tool_use + tool_result turns, got %d msgs", len(fm.gotReqs[1].Messages))
	}
}

// 3. A tool error becomes an error tool_result (loop continues; model adapts).
func TestLoopToolErrorBecomesResult(t *testing.T) {
	tool := &stubTool{name: "observe_cost", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		return Result{}, errBackend()
	}}
	reg := NewRegistry()
	reg.Register(tool)
	fm := newFakeModel(asstToolUse("u1", "observe_cost", `{}`), asstText("could not read cost"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)
	hist, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "cost?"))
	if err != nil {
		t.Fatal("a tool error must not abort the loop")
	}
	if !hasErrorToolResult(hist) {
		t.Fatal("tool error must surface as an error tool_result")
	}
}

// 4. An unknown tool name → recoverable error result, not a crash.
func TestLoopUnknownTool(t *testing.T) {
	fm := newFakeModel(asstToolUse("u1", "ghost_tool", `{}`), asstText("ok"))
	l := newLoop(fm, NewRegistry(), NewGate(nil, nil, false), nil)
	hist, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasErrorToolResult(hist) {
		t.Fatal("unknown tool must yield an error tool_result")
	}
}

// 5. Confirm-tier tool: declined → "user declined" result; approved → runs.
func TestLoopConfirmGate(t *testing.T) {
	mit := &stubTool{name: "mitigate_kill", tier: TierConfirm}
	reg := NewRegistry()
	reg.Register(mit)

	fm := newFakeModel(asstToolUse("u1", "mitigate_kill", `{}`), asstText("understood, leaving it on"))
	declined := newLoop(fm, reg, NewGate(nil, nil, false), func(context.Context, Tool, json.RawMessage, string) (bool, error) { return false, nil })
	hist, _, _ := declined.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "kill it"))
	if atomic.LoadInt32(&mit.calls) != 0 {
		t.Fatal("declined mitigation must NOT run")
	}
	if !hasToolResultContaining(hist, "declined") {
		t.Fatal("decline must feed a 'user declined' result so the model adapts")
	}

	mit2 := &stubTool{name: "mitigate_kill", tier: TierConfirm, run: func(json.RawMessage) (Result, error) { return Result{Content: "engaged"}, nil }}
	reg2 := NewRegistry()
	reg2.Register(mit2)
	fm2 := newFakeModel(asstToolUse("u1", "mitigate_kill", `{}`), asstText("done"))
	approved := newLoop(fm2, reg2, NewGate(nil, nil, false), func(context.Context, Tool, json.RawMessage, string) (bool, error) { return true, nil })
	approved.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "kill it"))
	if atomic.LoadInt32(&mit2.calls) != 1 {
		t.Fatal("approved mitigation must run")
	}
}

// 6. Step cap: a model that always calls a tool stops at StepCap with a flag.
func TestLoopStepCap(t *testing.T) {
	tool := &stubTool{name: "loop_tool", tier: TierAuto}
	reg := NewRegistry()
	reg.Register(tool)
	resps := make([]*ModelResponse, 20)
	for i := range resps {
		resps[i] = asstToolUse("u", "loop_tool", `{}`)
	}
	fm := &fakeModel{resps: resps}
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)
	l.StepCap = 3
	_, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
	if err == nil || !strings.Contains(err.Error(), "step cap") {
		t.Fatalf("hitting the step cap must return a step-cap signal, got %v", err)
	}
	if fm.calls > 3 {
		t.Fatalf("must not exceed the step cap, calls=%d", fm.calls)
	}
}

// 7. Parallel reads: two independent auto tool_use blocks run concurrently.
func TestLoopParallelReads(t *testing.T) {
	var inflight, maxInflight int32
	mk := func(name string) *stubTool {
		return &stubTool{name: name, tier: TierAuto, run: func(json.RawMessage) (Result, error) {
			n := atomic.AddInt32(&inflight, 1)
			for {
				m := atomic.LoadInt32(&maxInflight)
				if n <= m || atomic.CompareAndSwapInt32(&maxInflight, m, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			return Result{Content: name}, nil
		}}
	}
	reg := NewRegistry()
	reg.Register(mk("a"))
	reg.Register(mk("b"))
	twoCalls := &ModelResponse{Message: Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockToolUse, ID: "1", ToolName: "a", Input: json.RawMessage(`{}`)},
		{Type: BlockToolUse, ID: "2", ToolName: "b", Input: json.RawMessage(`{}`)},
	}}, StopReason: StopToolUse}
	fm := newFakeModel(twoCalls, asstText("done"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)
	l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
	if atomic.LoadInt32(&maxInflight) < 2 {
		t.Fatalf("independent reads must run in parallel, max inflight=%d", maxInflight)
	}
}

// TestLoopToolStartCallbackIsSerial guards the OnToolStart contract: even when
// independent auto-tier tools execute concurrently, the progress callback must fire
// on the loop goroutine in block order, never from a parallel worker — the real TUI
// sink appends a transcript line, so a concurrent call would race. Run under -race:
// before the fix the two workers called OnToolStart concurrently and the detector
// flagged the unguarded append below.
func TestLoopToolStartCallbackIsSerial(t *testing.T) {
	mk := func(name string) *stubTool {
		return &stubTool{name: name, tier: TierAuto, run: func(json.RawMessage) (Result, error) {
			time.Sleep(10 * time.Millisecond) // keep both tools' execution windows overlapping
			return Result{Content: name}, nil
		}}
	}
	reg := NewRegistry()
	reg.Register(mk("a"))
	reg.Register(mk("b"))
	twoCalls := &ModelResponse{Message: Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockToolUse, ID: "1", ToolName: "a", Input: json.RawMessage(`{}`)},
		{Type: BlockToolUse, ID: "2", ToolName: "b", Input: json.RawMessage(`{}`)},
	}}, StopReason: StopToolUse}
	fm := newFakeModel(twoCalls, asstText("done"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)
	var started []string // intentionally unguarded: the contract guarantees serial calls
	l.OnToolStart = func(name string, _ json.RawMessage) { started = append(started, name) }
	l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))
	if len(started) != 2 {
		t.Fatalf("both parallel tools must be announced exactly once, got %v", started)
	}
}

// 9. Progress callbacks: OnText streams assistant text; OnToolStart announces tools.
func TestLoopStreamsProgress(t *testing.T) {
	tool := &stubTool{name: "observe_cost", tier: TierAuto}
	reg := NewRegistry()
	reg.Register(tool)
	fm := newFakeModel(asstToolUse("u1", "observe_cost", `{}`), asstText("answer text"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)
	var texts []string
	var startedTools []string
	l.OnText = func(s string) { texts = append(texts, s) }
	l.OnToolStart = func(name string, _ json.RawMessage) { startedTools = append(startedTools, name) }

	l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "cost?"))

	if len(startedTools) != 1 || startedTools[0] != "observe_cost" {
		t.Fatalf("OnToolStart must announce the tool, got %v", startedTools)
	}
	joined := strings.Join(texts, "")
	if !strings.Contains(joined, "answer text") {
		t.Fatalf("OnText must stream the assistant text, got %q", joined)
	}
}

// 9a. The reasoning channel is forwarded to OnReasoning, in order, and is NOT
// folded into the assistant text stream (display-only, never persisted).
func TestLoopStreamsReasoning(t *testing.T) {
	fm := newFakeModel(asstText("the answer"))
	fm.reasonings = []string{"weighing the options"}
	l := newLoop(fm, NewRegistry(), NewGate(nil, nil, false), nil)
	var reasoning, text []string
	l.OnReasoning = func(s string) { reasoning = append(reasoning, s) }
	l.OnText = func(s string) { text = append(text, s) }

	hist, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "q?"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reasoning, "") != "weighing the options" {
		t.Fatalf("OnReasoning must receive the thinking channel, got %v", reasoning)
	}
	if strings.Contains(strings.Join(text, ""), "weighing") {
		t.Fatalf("reasoning must not leak into the assistant text stream, got %v", text)
	}
	// Reasoning is display-only: it must not appear as a block in the transcript.
	for _, m := range hist {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "weighing") {
				t.Fatalf("reasoning must never be persisted to the transcript, found in %+v", m)
			}
		}
	}
}

// 9c. A cancelled turn bails before calling the model — the loop's ctx check is
// what makes the TUI's esc-interrupt take immediately instead of running another
// model + tool round first.
func TestLoopCancelledContextBails(t *testing.T) {
	fm := newFakeModel(asstText("must not run"))
	l := newLoop(fm, NewRegistry(), NewGate(nil, nil, false), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := l.Run(ctx, "S", nil, TextMessage(RoleUser, "hi"))
	if err == nil {
		t.Fatal("a cancelled turn must return an error")
	}
	if fm.calls != 0 {
		t.Fatalf("a cancelled turn must not call the model, got %d calls", fm.calls)
	}
}

// 9b. A zero StepCap defaults to the built-in cap and still answers.
func TestLoopDefaultStepCap(t *testing.T) {
	fm := newFakeModel(asstText("ok"))
	l := &Loop{Model: fm, Registry: NewRegistry(), Gate: NewGate(nil, nil, false)}
	hist, _, err := l.Run(context.Background(), "S", nil, TextMessage(RoleUser, "hi"))
	if err != nil || len(hist) == 0 {
		t.Fatalf("zero StepCap must default and still answer, err=%v hist=%d", err, len(hist))
	}
}

// 10. A model error mid-turn propagates out of Run (the turn cannot continue).
func TestLoopModelError(t *testing.T) {
	fm := &fakeModel{errs: []error{errBackend()}}
	l := newLoop(fm, NewRegistry(), NewGate(nil, nil, false), nil)
	_, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "x"))
	if err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Fatalf("a model error must propagate, got %v", err)
	}
}

// test helpers
func errBackend() error { return &stubErr{"backend down"} }

type stubErr struct{ s string }

func (e *stubErr) Error() string { return e.s }

func hasErrorToolResult(hist []Message) bool {
	for _, m := range hist {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && b.IsError {
				return true
			}
		}
	}
	return false
}

func hasToolResultContaining(hist []Message, sub string) bool {
	for _, m := range hist {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && strings.Contains(b.Text, sub) {
				return true
			}
		}
	}
	return false
}

// TestLoopOnToolEndReportsResultsInOrder guards the OnToolEnd contract: each tool's
// result (output + error flag) is reported once, in block order, after the parallel
// batch — so a stateful UI sink (the transcript peek) never sees it out of order.
func TestLoopOnToolEndReportsResultsInOrder(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubTool{name: "a", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		time.Sleep(8 * time.Millisecond)
		return Result{Content: `{"ok":1}`}, nil
	}})
	reg.Register(&stubTool{name: "b", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		return Result{Content: "boom", IsError: true}, nil
	}})
	twoCalls := &ModelResponse{Message: Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockToolUse, ID: "1", ToolName: "a", Input: json.RawMessage(`{}`)},
		{Type: BlockToolUse, ID: "2", ToolName: "b", Input: json.RawMessage(`{}`)},
	}}, StopReason: StopToolUse}
	fm := newFakeModel(twoCalls, asstText("done"))
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)

	type ev struct {
		name, out string
		isErr     bool
	}
	var ends []ev // serial by contract — no guard needed
	l.OnToolEnd = func(name string, out json.RawMessage, isErr bool) {
		ends = append(ends, ev{name, string(out), isErr})
	}
	l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "go"))

	if len(ends) != 2 {
		t.Fatalf("OnToolEnd must fire once per tool, got %v", ends)
	}
	if ends[0] != (ev{"a", `{"ok":1}`, false}) {
		t.Fatalf("first result wrong (order/output/err): %+v", ends[0])
	}
	if ends[1].name != "b" || !ends[1].isErr {
		t.Fatalf("second result should be b's error: %+v", ends[1])
	}
}

// redactFuncStub adapts a func to the Redactor interface for tests.
type redactFuncStub func(toolName, text string) string

func (f redactFuncStub) RedactToolOutput(toolName, text string) string { return f(toolName, text) }

// TestLoopRedactsToolOutput proves the Redactor seam scrubs a tool result BEFORE
// it reaches the model's conversation and the OnToolEnd peek — the data-governance
// guarantee the web assistant relies on (raw traffic bodies must not enter the
// prompt unredacted). A nil Redactor (the CLI default) is covered by every other
// loop test, which pass tool output through verbatim.
func TestLoopRedactsToolOutput(t *testing.T) {
	tool := &stubTool{name: "observe_traffic_event", tier: TierAuto, run: func(json.RawMessage) (Result, error) {
		return Result{Content: `{"requestBody":"contact alice@example.com"}`}, nil
	}}
	reg := NewRegistry()
	reg.Register(tool)
	fm := newFakeModel(
		asstToolUse("u1", "observe_traffic_event", `{}`),
		asstText("done"),
	)
	l := newLoop(fm, reg, NewGate(nil, nil, false), nil)

	var seen string
	l.OnToolEnd = func(_ string, output json.RawMessage, _ bool) { seen = string(output) }
	var gotTool string
	l.Redactor = redactFuncStub(func(toolName, text string) string {
		gotTool = toolName
		return strings.ReplaceAll(text, "alice@example.com", "[REDACTED]")
	})

	conv, _, err := l.Run(context.Background(), "SYS", nil, TextMessage(RoleUser, "show the event"))
	if err != nil {
		t.Fatal(err)
	}
	if gotTool != "observe_traffic_event" {
		t.Errorf("Redactor must receive the tool name, got %q", gotTool)
	}
	// The OnToolEnd peek (same results slice that enters the conversation) is redacted.
	if strings.Contains(seen, "alice@example.com") {
		t.Fatalf("tool output reached the UI/model unredacted: %q", seen)
	}
	if !strings.Contains(seen, "[REDACTED]") {
		t.Fatalf("expected the redaction marker in the tool peek, got %q", seen)
	}
	// And the conversation the model continued from carries the redacted result,
	// never the raw PII.
	var convDump strings.Builder
	for _, m := range conv {
		for _, b := range m.Blocks {
			convDump.WriteString(b.Text)
		}
	}
	if strings.Contains(convDump.String(), "alice@example.com") {
		t.Fatalf("the conversation passed to the model still contains raw PII: %q", convDump.String())
	}
}

// TestLoopSurvivesPoisonedEmptyMessage pins the V16 double guard: a historic
// assistant message with NO blocks (an interrupted turn persisted one) is
// dropped from the model REQUEST (providers 400 on empty content), and a
// contentless model reply is never appended to the transcript in the first
// place.
func TestLoopSurvivesPoisonedEmptyMessage(t *testing.T) {
	poisoned := []Message{
		TextMessage(RoleUser, "first"),
		{Role: RoleAssistant, Blocks: []Block{}},                              // the poison
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "  "}}}, // whitespace-only
	}
	sent := sendableMessages(append(poisoned, TextMessage(RoleUser, "继续")))
	if len(sent) != 2 {
		t.Fatalf("empty/whitespace messages must be dropped from the request, got %d: %+v", len(sent), sent)
	}
	for _, m := range sent {
		if len(m.Blocks) == 0 {
			t.Fatalf("no empty message may reach the provider: %+v", m)
		}
	}

	// A model reply with zero blocks must not enter the produced transcript.
	fm := newFakeModel(&ModelResponse{Message: Message{Role: RoleAssistant}})
	l := &Loop{Model: fm, Registry: NewRegistry(), Gate: NewGate(nil, nil, false)}
	produced, _, err := l.Run(context.Background(), "sys", nil, TextMessage(RoleUser, "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, m := range produced {
		if m.Role == RoleAssistant && len(m.Blocks) == 0 {
			t.Fatalf("a contentless reply must not be persisted: %+v", produced)
		}
	}
}
