package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func TestNewAgentRegistryIsSuperset(t *testing.T) {
	reg := NewAgentRegistry(&fakeGateway{}, &fakeCanvas{}, AgentOptions{EnableMitigate: true, EnableCanvas: true, EnableSystem: true})
	for _, want := range []string{"observe_health", "navigate", "show_event", "highlight", "run_command", "read_file", "write_file", "mitigate_kill_switch"} {
		if _, ok := reg.Get(want); !ok {
			t.Fatalf("agent registry must expose %q", want)
		}
	}
}

func TestBuildAgentRunsToolThenAnswers(t *testing.T) {
	// Scripted model: round 1 calls observe_health, round 2 answers.
	steps := []*core.ChatResult{
		{FinishReason: "tool_calls", ToolCalls: []core.ToolCall{{ID: "c1", Function: core.ToolCallFunction{Name: "observe_health", Arguments: "{}"}}}},
		{Content: "All healthy: 27 nodes.", FinishReason: "stop"},
	}
	sm := &scriptedStreamer{steps: steps}
	gw := &fakeGateway{instances: &core.InstancesResult{Count: 27}, sparkline: &core.SparklineResult{}}
	canvas := &fakeCanvas{}
	dir := t.TempDir()

	var toolStarts []string
	ag, err := BuildAgent(context.Background(), AgentDeps{
		Streamer: sm, Gateway: gw, Canvas: canvas,
		VKSecret: "vk", Model: "gpt-4o", Env: "local",
		MemoryDir: dir, SessionDir: dir,
		Confirm:     func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) { return true, nil },
		OnToolStart: func(name string, _ []byte) { toolStarts = append(toolStarts, name) },
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Turn(context.Background(), "is the gateway healthy?", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "27 nodes") {
		t.Fatalf("agent should answer after running the health tool, got %q", out)
	}
	if sm.calls != 2 {
		t.Fatalf("agent should call the model twice (tool round + answer), got %d", sm.calls)
	}
	if len(toolStarts) != 1 || toolStarts[0] != "observe_health" {
		t.Fatalf("OnToolStart must report the executed tool, got %v", toolStarts)
	}
	// The session was persisted under the session dir.
	if metas, _ := ag.Store.List(); len(metas) != 1 {
		t.Fatalf("the turn must persist exactly one session, got %d", len(metas))
	}
}

// TestBuildAgentResumesInjectedSession pins the resume seam (the TUI's /sessions
// pick): a non-nil AgentDeps.Session binds the built agent to that conversation,
// so the next turn APPENDS to the same persisted session id instead of starting
// a fresh one.
func TestBuildAgentResumesInjectedSession(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	past := agent.NewSession("local")
	past.Messages = []agent.Message{
		agent.TextMessage(agent.RoleUser, "what changed yesterday?"),
		agent.TextMessage(agent.RoleAssistant, "two routing rules were toggled"),
	}
	if err := agent.OpenStoreAt(sessDir).Save(past); err != nil {
		t.Fatal(err)
	}

	sm := &scriptedStreamer{steps: []*core.ChatResult{{Content: "still those two.", FinishReason: "stop"}}}
	ag, err := BuildAgent(context.Background(), AgentDeps{
		Streamer: sm, Gateway: &fakeGateway{}, Canvas: &fakeCanvas{},
		VKSecret: "vk", Model: "m", Env: "local",
		MemoryDir: dir, SessionDir: sessDir,
		Session: past,
		Confirm: func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag.Session != past {
		t.Fatal("BuildAgent must bind the injected session, not start a fresh one")
	}
	if _, err := ag.Turn(context.Background(), "and today?", ""); err != nil {
		t.Fatal(err)
	}
	// The turn appended to the SAME persisted session: still one file, same id,
	// history kept, the new exchange appended after it.
	st := agent.OpenStoreAt(sessDir)
	metas, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != past.ID {
		t.Fatalf("the resumed turn must persist under session %s, got %+v", past.ID, metas)
	}
	got, err := st.Load(past.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 || got.Messages[2].Text() != "and today?" || got.Messages[3].Text() != "still those two." {
		t.Fatalf("resumed session must keep its history and append the new turn, got %d messages: %+v", len(got.Messages), got.Messages)
	}
}

func TestBuildAgentConfirmGatesMitigation(t *testing.T) {
	// The model proposes a kill-switch engage; a declining Confirm must block the
	// write and the model adapts (sees "user declined").
	steps := []*core.ChatResult{
		{FinishReason: "tool_calls", ToolCalls: []core.ToolCall{{ID: "c1", Function: core.ToolCallFunction{Name: "mitigate_kill_switch", Arguments: `{"engage":true}`}}}},
		{Content: "Understood, leaving it off.", FinishReason: "stop"},
	}
	gw := &fakeGateway{}
	dir := t.TempDir()
	ag, err := BuildAgent(context.Background(), AgentDeps{
		Streamer: &scriptedStreamer{steps: steps}, Gateway: gw, Canvas: &fakeCanvas{},
		VKSecret: "vk", Model: "m", Env: "local", EnableMitigate: true,
		MemoryDir: dir, SessionDir: dir,
		Confirm: func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) { return false, nil }, // decline
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Turn(context.Background(), "engage the kill switch", ""); err != nil {
		t.Fatal(err)
	}
	if gw.killCalls != 0 {
		t.Fatalf("a declined confirmation must NOT issue the write, got %d kill calls", gw.killCalls)
	}
}
