package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// --- fakes ---------------------------------------------------------------

type scriptModel struct {
	responder  func(req agent.ModelRequest) (*agent.ModelResponse, error)
	onGenerate func(ctx context.Context) // optional: observe the (decorated) run context
	mu         sync.Mutex
	calls      int
}

func (m *scriptModel) Generate(ctx context.Context, req agent.ModelRequest, _, _ func(string)) (*agent.ModelResponse, error) {
	if m.onGenerate != nil {
		m.onGenerate(ctx)
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return m.responder(req)
}

func finalModel(text string, tokens int) *scriptModel {
	return &scriptModel{responder: func(agent.ModelRequest) (*agent.ModelResponse, error) {
		return finalResp(text, tokens), nil
	}}
}

func toolUseResp(id, tool, input string) *agent.ModelResponse {
	return &agent.ModelResponse{Message: agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{
		{Type: agent.BlockToolUse, ID: id, ToolName: tool, Input: json.RawMessage(input)},
	}}, StopReason: agent.StopToolUse}
}

func finalResp(text string, tokens int) *agent.ModelResponse {
	return &agent.ModelResponse{Message: agent.TextMessage(agent.RoleAssistant, text), StopReason: agent.StopEndTurn, Usage: &agent.Usage{TotalTokens: tokens}}
}

type fakeAutoTool struct {
	name string
	ran  *int32
}

func (t fakeAutoTool) Name() string          { return t.name }
func (t fakeAutoTool) Description() string   { return "read " + t.name }
func (fakeAutoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeAutoTool) Tier() agent.Tier        { return agent.TierAuto }
func (t fakeAutoTool) Run(context.Context, json.RawMessage) (agent.Result, error) {
	if t.ran != nil {
		atomic.AddInt32(t.ran, 1)
	}
	return agent.Result{Content: "read ok"}, nil
}

type fakeMitigateTool struct{ name string }

func (t fakeMitigateTool) Name() string          { return t.name }
func (t fakeMitigateTool) Description() string   { return "mutate " + t.name }
func (fakeMitigateTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeMitigateTool) Tier() agent.Tier        { return agent.TierConfirm }
func (fakeMitigateTool) Run(context.Context, json.RawMessage) (agent.Result, error) {
	return agent.Result{Content: "mutated"}, nil
}

func newDispatchTool(model agent.Model, confirm agent.ConfirmFunc, tools ...agent.Tool) SubagentDispatchTool {
	parent := agent.NewRegistry()
	for _, t := range tools {
		parent.Register(t)
	}
	dt := NewSubagentDispatchTool(SubagentDispatchConfig{
		Parent: parent, Model: model, Gate: agent.NewGate(nil, nil, false), Confirm: confirm,
		FanOutMax: 4, MaxTurns: 4, MaxTokens: 100_000, Deadline: time.Minute,
	})
	parent.Register(dt) // mirror the builder: the dispatch tool is in the parent registry
	return dt
}

func mustDispatch(t *testing.T, dt SubagentDispatchTool, input string) dispatchOutput {
	t.Helper()
	res, err := dt.Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Run returned a Go error (should be in-band): %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch unexpectedly errored: %s", res.Content)
	}
	var out dispatchOutput
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("decode dispatch output %q: %v", res.Content, err)
	}
	return out
}

func dispatchToolError(t *testing.T, dt SubagentDispatchTool, input string) string {
	t.Helper()
	res, err := dt.Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Run returned a Go error (should be in-band): %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected an in-band tool error; got %s", res.Content)
	}
	return res.Content
}

// --- tests ---------------------------------------------------------------

func TestDispatch_ParallelThreeTasks(t *testing.T) {
	dt := newDispatchTool(finalModel("child summary", 100), nil, fakeAutoTool{name: "read"})
	out := mustDispatch(t, dt, `{"parallel":true,"tasks":[{"task":"a"},{"task":"b"},{"task":"c"}]}`)
	if !out.Parallel || out.Dispatched != 3 || len(out.Results) != 3 {
		t.Fatalf("dispatched=%d results=%d parallel=%v; want 3/3/true", out.Dispatched, len(out.Results), out.Parallel)
	}
	if out.TotalUsage.TotalTokens != 300 {
		t.Errorf("total usage = %d; want 300 (3×100)", out.TotalUsage.TotalTokens)
	}
	for i, r := range out.Results {
		if r.Index != i+1 || r.Status != "completed" || r.Summary != "child summary" || r.Usage.TotalTokens != 100 {
			t.Errorf("result[%d] = %+v; want completed child summary 100 tokens", i, r)
		}
	}
}

func TestDispatch_ParallelRejectsConfirmTier(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read"}, fakeMitigateTool{name: "kill_switch"})
	msg := dispatchToolError(t, dt, `{"parallel":true,"tasks":[{"task":"do it","tools":["kill_switch"]}]}`)
	if !strings.Contains(msg, "kill_switch") || !strings.Contains(msg, "confirm-tier") {
		t.Errorf("error = %q; want it to name the confirm-tier tool", msg)
	}
}

func TestDispatch_InheritExcludesConfirmInParallel(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read"}, fakeMitigateTool{name: "kill_switch"})

	par, err := dt.childRegistry(dispatchTask{Task: "t"}, true)
	if err != nil {
		t.Fatalf("childRegistry(parallel): %v", err)
	}
	if _, ok := par.Get("kill_switch"); ok {
		t.Error("parallel child inherited a confirm-tier tool; want it excluded")
	}
	if _, ok := par.Get("read"); !ok {
		t.Error("parallel child missing the auto-tier read tool")
	}
	if _, ok := par.Get(agent.DispatchToolName); ok {
		t.Error("child registry contains the dispatch tool (depth-1 violated)")
	}

	seq, err := dt.childRegistry(dispatchTask{Task: "t"}, false)
	if err != nil {
		t.Fatalf("childRegistry(sequential): %v", err)
	}
	if _, ok := seq.Get("kill_switch"); !ok {
		t.Error("sequential child should inherit the confirm-tier tool")
	}
}

func TestDispatch_DepthOnePrunedFromExplicitList(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read"})
	msg := dispatchToolError(t, dt, `{"tasks":[{"task":"t","tools":["`+agent.DispatchToolName+`"]}]}`)
	if !strings.Contains(msg, "cannot dispatch further") {
		t.Errorf("error = %q; want a depth-1 rejection", msg)
	}
}

func TestDispatch_UnknownToolRejected(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read"})
	msg := dispatchToolError(t, dt, `{"tasks":[{"task":"t","tools":["ghost"]}]}`)
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error = %q; want it to name the unknown tool", msg)
	}
}

func TestDispatch_TooManyTasks(t *testing.T) {
	var ran int32
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read", ran: &ran})
	dt.cfg.FanOutMax = 2
	msg := dispatchToolError(t, dt, `{"tasks":[{"task":"a"},{"task":"b"},{"task":"c"}]}`)
	if !strings.Contains(msg, "at most 2") {
		t.Errorf("error = %q; want it to name the cap of 2", msg)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Error("a child ran despite the over-cap rejection")
	}
}

func TestDispatch_EmptyAndBlankTasks(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read"})
	if msg := dispatchToolError(t, dt, `{"tasks":[]}`); !strings.Contains(msg, "at least one task") {
		t.Errorf("empty tasks error = %q", msg)
	}
	if msg := dispatchToolError(t, dt, `{"tasks":[{"task":"   "}]}`); !strings.Contains(msg, "empty task") {
		t.Errorf("blank task error = %q", msg)
	}
	if msg := dispatchToolError(t, dt, `not json`); !strings.Contains(msg, "invalid input") {
		t.Errorf("bad json error = %q", msg)
	}
}

func TestDispatch_SingleConfirmBubblesWithSubagentNamed(t *testing.T) {
	model := &scriptModel{}
	var n int32
	model.responder = func(agent.ModelRequest) (*agent.ModelResponse, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return toolUseResp("u1", "kill_switch", `{}`), nil
		}
		return finalResp("done after denial", 5), nil
	}
	var sawReason, sawTool string
	confirm := func(_ context.Context, tool agent.Tool, _ json.RawMessage, reason string) (bool, error) {
		sawTool = tool.Name()
		sawReason = reason
		return false, nil
	}
	dt := newDispatchTool(model, confirm, fakeMitigateTool{name: "kill_switch"})
	out := mustDispatch(t, dt, `{"tasks":[{"task":"flip the kill switch"}]}`)

	if sawTool != "kill_switch" {
		t.Errorf("parent confirm saw tool %q; want kill_switch", sawTool)
	}
	if !strings.Contains(sawReason, "subagent 1") {
		t.Errorf("confirm reason = %q; want the sub-agent named", sawReason)
	}
	r := out.Results[0]
	if len(r.Denials) != 1 || r.Denials[0].Tool != "kill_switch" || r.Denials[0].Kind != "user" {
		t.Errorf("denials = %+v; want one user denial on kill_switch", r.Denials)
	}
	if r.Status != "completed" || r.Summary != "done after denial" {
		t.Errorf("result = %+v; want the child to have adapted to the denial", r)
	}
}

// TestDispatch_DecorateChildStampsEachChild pins the per-surface hook: every child's
// run context is derived via DecorateChild with the sub-agent label (the
// control-plane uses this for the audit marker, T4). The decorated context reaches
// the child's model call, so we observe it there.
func TestDispatch_DecorateChildStampsEachChild(t *testing.T) {
	type ctxKey struct{}
	var seen sync.Map
	parent := agent.NewRegistry()
	parent.Register(fakeAutoTool{name: "read"})
	model := finalModel("done", 1)
	model.onGenerate = func(ctx context.Context) {
		if v, ok := ctx.Value(ctxKey{}).(string); ok {
			seen.Store(v, true)
		}
	}
	dt := NewSubagentDispatchTool(SubagentDispatchConfig{
		Parent: parent, Model: model, Gate: agent.NewGate(nil, nil, false),
		DecorateChild: func(ctx context.Context, label string) context.Context {
			return context.WithValue(ctx, ctxKey{}, label)
		},
	})
	parent.Register(dt)
	mustDispatch(t, dt, `{"parallel":true,"tasks":[{"task":"a"},{"task":"b"}]}`)
	for _, label := range []string{"subagent 1", "subagent 2"} {
		if _, ok := seen.Load(label); !ok {
			t.Errorf("child model never saw decorated context for %q", label)
		}
	}
}

// TestDispatch_ExplicitValidToolSubset pins that an explicit tools list grants
// exactly that subset (and nothing else) to the child.
func TestDispatch_ExplicitValidToolSubset(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil, fakeAutoTool{name: "read"}, fakeAutoTool{name: "analyze"})
	reg, err := dt.childRegistry(dispatchTask{Task: "t", Tools: []string{"read"}}, false)
	if err != nil {
		t.Fatalf("childRegistry: %v", err)
	}
	if _, ok := reg.Get("read"); !ok {
		t.Error("explicit subset missing the granted tool")
	}
	if _, ok := reg.Get("analyze"); ok {
		t.Error("explicit subset leaked a tool that was not requested")
	}
}

// TestDispatch_AuthoringOffloadToolExposure pins the FR-53 carrier contract on
// the dispatch chassis, using the production build-tool names and tiers: an
// authoring-offload child can reach the auto-tier iteration tools
// (workflow_draft_save, workflow_freeze — draft→freeze→repair runs inside the
// subagent), while the human-moment tools (workflow_approve, workflow_run_start
// — both confirm-tier) and skill_delete NEVER enter a parallel child's registry,
// and an explicit authoring subset never carries them either.
func TestDispatch_AuthoringOffloadToolExposure(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil,
		fakeAutoTool{name: "workflow_draft_save"},
		fakeAutoTool{name: "workflow_freeze"},
		fakeMitigateTool{name: "workflow_approve"},
		fakeMitigateTool{name: "workflow_run_start"},
		fakeMitigateTool{name: "skill_delete"},
	)

	// Parallel inherit-all: the iteration tools are present, the human-moment
	// tools are stripped with the rest of the confirm tier.
	par, err := dt.childRegistry(dispatchTask{Task: "draft and freeze until green"}, true)
	if err != nil {
		t.Fatalf("childRegistry(parallel): %v", err)
	}
	for _, name := range []string{"workflow_draft_save", "workflow_freeze"} {
		if _, ok := par.Get(name); !ok {
			t.Errorf("offload child missing the auto-tier iteration tool %q", name)
		}
	}
	for _, name := range []string{"workflow_approve", "workflow_run_start", "skill_delete"} {
		if _, ok := par.Get(name); ok {
			t.Errorf("parallel child registry must never include %q (human moment / destructive)", name)
		}
	}

	// The authoring-offload subset the knowledge skill teaches grants exactly the
	// iteration pair and nothing else.
	sub, err := dt.childRegistry(dispatchTask{Task: "t", Tools: []string{"workflow_draft_save", "workflow_freeze"}}, false)
	if err != nil {
		t.Fatalf("childRegistry(offload subset): %v", err)
	}
	for _, name := range []string{"workflow_approve", "workflow_run_start", "skill_delete"} {
		if _, ok := sub.Get(name); ok {
			t.Errorf("the offload subset leaked %q", name)
		}
	}
	if _, ok := sub.Get("workflow_freeze"); !ok {
		t.Error("the offload subset must carry workflow_freeze")
	}

	// Explicitly requesting the approve tool in a parallel dispatch fails fast
	// with the tier named — the approval cannot be smuggled into a fan-out.
	msg := dispatchToolError(t, dt, `{"parallel":true,"tasks":[{"task":"t","tools":["workflow_approve"]}]}`)
	if !strings.Contains(msg, "workflow_approve") || !strings.Contains(msg, "confirm-tier") {
		t.Errorf("error = %q; want it to name workflow_approve as confirm-tier", msg)
	}
}

// TestDispatch_TimeoutDenialTyped pins that a bubbled confirm that times out is
// recorded as a "timeout" denial (distinct from a user "no"), so the summary can
// say the operator was away (ruling #5).
func TestDispatch_TimeoutDenialTyped(t *testing.T) {
	model := &scriptModel{}
	var n int32
	model.responder = func(agent.ModelRequest) (*agent.ModelResponse, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return toolUseResp("u1", "kill_switch", `{}`), nil
		}
		return finalResp("ok", 1), nil
	}
	confirm := func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) {
		return false, context.DeadlineExceeded
	}
	dt := newDispatchTool(model, confirm, fakeMitigateTool{name: "kill_switch"})
	out := mustDispatch(t, dt, `{"tasks":[{"task":"flip it"}]}`)
	r := out.Results[0]
	if len(r.Denials) != 1 || r.Denials[0].Kind != "timeout" {
		t.Errorf("denials = %+v; want one timeout denial", r.Denials)
	}
}

// TestDispatch_LongTaskTruncatedInConfirm pins that a long task line is truncated
// in the bubbled confirm prefix (the "…" ellipsis).
func TestDispatch_LongTaskTruncatedInConfirm(t *testing.T) {
	longTask := strings.Repeat("x", 200)
	model := &scriptModel{}
	var n int32
	model.responder = func(agent.ModelRequest) (*agent.ModelResponse, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return toolUseResp("u1", "kill_switch", `{}`), nil
		}
		return finalResp("ok", 1), nil
	}
	var sawReason string
	confirm := func(_ context.Context, _ agent.Tool, _ json.RawMessage, reason string) (bool, error) {
		sawReason = reason
		return true, nil
	}
	dt := newDispatchTool(model, confirm, fakeMitigateTool{name: "kill_switch"})
	mustDispatch(t, dt, `{"tasks":[{"task":"`+longTask+`"}]}`)
	if !strings.Contains(sawReason, "…") {
		t.Errorf("confirm reason = %q; want the long task truncated with an ellipsis", sawReason)
	}
}

// TestDispatch_NilConfirmAutoDeniesConfirmTier pins fail-safe in a single dispatch
// with no host ConfirmFunc: a confirm-tier child tool is auto-denied, not run.
func TestDispatch_NilConfirmAutoDeniesConfirmTier(t *testing.T) {
	model := &scriptModel{}
	var n int32
	model.responder = func(agent.ModelRequest) (*agent.ModelResponse, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return toolUseResp("u1", "kill_switch", `{}`), nil
		}
		return finalResp("adapted", 1), nil
	}
	dt := newDispatchTool(model, nil, fakeMitigateTool{name: "kill_switch"})
	out := mustDispatch(t, dt, `{"tasks":[{"task":"flip it"}]}`)
	if len(out.Results[0].Denials) != 1 {
		t.Errorf("denials = %+v; want one auto-denial with a nil host confirm", out.Results[0].Denials)
	}
}

func TestDispatch_ChildModelErrorSurfacesPerTask(t *testing.T) {
	model := &scriptModel{responder: func(req agent.ModelRequest) (*agent.ModelResponse, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Text(), "boom") {
				return nil, errors.New("gateway down")
			}
		}
		return finalResp("ok", 10), nil
	}}
	dt := newDispatchTool(model, nil, fakeAutoTool{name: "read"})
	out := mustDispatch(t, dt, `{"parallel":true,"tasks":[{"task":"fine"},{"task":"boom"}]}`)

	var okCount, errCount int
	for _, r := range out.Results {
		switch {
		case r.Status == "completed":
			okCount++
		case strings.HasPrefix(r.Status, "error:"):
			errCount++
			if !strings.Contains(r.Summary, "gateway down") {
				t.Errorf("error result summary = %q; want the cause", r.Summary)
			}
		}
	}
	if okCount != 1 || errCount != 1 {
		t.Errorf("ok=%d err=%d; want 1 completed + 1 errored", okCount, errCount)
	}
}

func TestDispatch_ParallelRunsConcurrently(t *testing.T) {
	const n = 3
	var inflight int32
	barrier := make(chan struct{})
	model := &scriptModel{responder: func(agent.ModelRequest) (*agent.ModelResponse, error) {
		if atomic.AddInt32(&inflight, 1) == n {
			close(barrier)
		}
		<-barrier
		return finalResp("done", 1), nil
	}}
	dt := newDispatchTool(model, nil, fakeAutoTool{name: "read"})
	out := mustDispatch(t, dt, `{"parallel":true,"tasks":[{"task":"a"},{"task":"b"},{"task":"c"}]}`)
	if out.Dispatched != n || len(out.Results) != n {
		t.Fatalf("dispatched=%d; want %d", out.Dispatched, n)
	}
}

func TestDispatch_ToolMetadata(t *testing.T) {
	dt := newDispatchTool(finalModel("x", 1), nil)
	if dt.Name() != agent.DispatchToolName {
		t.Errorf("name = %q; want %q", dt.Name(), agent.DispatchToolName)
	}
	if dt.Tier() != agent.TierAuto {
		t.Errorf("tier = %v; want auto", dt.Tier())
	}
	if !json.Valid(dt.Schema()) {
		t.Error("schema is not valid JSON")
	}
	if dt.Description() == "" {
		t.Error("description is empty")
	}
}

// TestDispatch_DefaultCapsApplied pins that an unset cap falls back to the package
// default (so production callers need not pass caps).
func TestDispatch_DefaultCapsApplied(t *testing.T) {
	parent := agent.NewRegistry()
	dt := NewSubagentDispatchTool(SubagentDispatchConfig{Parent: parent, Model: finalModel("x", 1), Gate: agent.NewGate(nil, nil, false)})
	if dt.cfg.FanOutMax != SubagentFanOutMax || dt.cfg.MaxTurns != SubagentMaxTurns ||
		dt.cfg.MaxTokens != SubagentMaxTokens || dt.cfg.Deadline != SubagentDeadline {
		t.Errorf("defaults not applied: %+v", dt.cfg)
	}
}
