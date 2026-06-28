package runtime

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func TestModelTranslatesRequestToOpenAIWire(t *testing.T) {
	fs := &fakeChatStreamer{res: &core.ChatResult{Content: "done", FinishReason: "stop",
		Usage: &core.ChatUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}}
	m := NewModel(fs, "vk_secret", "gpt-4o")

	req := agent.ModelRequest{
		System: "you are an SRE",
		Messages: []agent.Message{
			agent.TextMessage(agent.RoleUser, "what is my cost?"),
			{Role: agent.RoleAssistant, Blocks: []agent.Block{
				{Type: agent.BlockText, Text: "checking"},
				{Type: agent.BlockToolUse, ID: "call_1", ToolName: "observe_cost", Input: json.RawMessage(`{"groupBy":"provider"}`)},
			}},
			{Role: agent.RoleUser, Blocks: []agent.Block{
				agent.ToolResult("call_1", "cost is $4/hr", false),
			}},
		},
		Tools: []agent.ToolSchema{{Name: "observe_cost", Description: "cost", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	var streamed string
	resp, err := m.Generate(context.Background(), req, func(s string) { streamed += s }, nil)
	if err != nil {
		t.Fatal(err)
	}

	if fs.gotVK != "vk_secret" || fs.gotReq.Model != "gpt-4o" {
		t.Fatalf("vk/model not carried: %q %q", fs.gotVK, fs.gotReq.Model)
	}
	wire := fs.gotReq.Messages
	if len(wire) != 4 || wire[0].Role != "system" || wire[0].Content != "you are an SRE" {
		t.Fatalf("system message must be prepended, got %+v", wire)
	}
	if wire[1].Role != "user" || wire[1].Content != "what is my cost?" {
		t.Fatalf("user message wrong: %+v", wire[1])
	}
	if wire[2].Role != "assistant" || len(wire[2].ToolCalls) != 1 ||
		wire[2].ToolCalls[0].ID != "call_1" || wire[2].ToolCalls[0].Function.Name != "observe_cost" ||
		wire[2].ToolCalls[0].Function.Arguments != `{"groupBy":"provider"}` {
		t.Fatalf("assistant tool_calls wrong: %+v", wire[2])
	}
	if wire[3].Role != "tool" || wire[3].ToolCallID != "call_1" || wire[3].Content != "cost is $4/hr" {
		t.Fatalf("tool result message wrong: %+v", wire[3])
	}
	if len(fs.gotReq.Tools) != 1 || fs.gotReq.Tools[0].Function.Name != "observe_cost" || fs.gotReq.ToolChoice != "auto" {
		t.Fatalf("tools must be forwarded with tool_choice=auto, got %+v choice=%q", fs.gotReq.Tools, fs.gotReq.ToolChoice)
	}
	if resp.StopReason != agent.StopEndTurn || resp.Message.Text() != "done" || streamed != "" {
		t.Fatalf("end-turn response wrong: %+v streamed=%q", resp, streamed)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 12 {
		t.Fatalf("usage must translate, got %+v", resp.Usage)
	}
}

func TestModelTranslatesToolCallResponse(t *testing.T) {
	fs := &fakeChatStreamer{res: &core.ChatResult{
		Content:      "let me look",
		FinishReason: "tool_calls",
		ToolCalls:    []core.ToolCall{{ID: "call_9", Function: core.ToolCallFunction{Name: "observe_alerts", Arguments: `{}`}}},
	}}
	m := NewModel(fs, "vk", "m")
	resp, err := m.Generate(context.Background(), agent.ModelRequest{Messages: []agent.Message{agent.TextMessage(agent.RoleUser, "any alerts?")}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != agent.StopToolUse {
		t.Fatalf("finish_reason tool_calls -> StopToolUse, got %v", resp.StopReason)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 1 || uses[0].ID != "call_9" || uses[0].ToolName != "observe_alerts" || string(uses[0].Input) != `{}` {
		t.Fatalf("tool_use block translation wrong: %+v", uses)
	}
	if resp.Message.Text() != "let me look" {
		t.Fatalf("preamble text must be preserved alongside tool_use, got %q", resp.Message.Text())
	}
	// No tools requested -> tool_choice/tools left empty.
	if fs.gotReq.ToolChoice != "" || len(fs.gotReq.Tools) != 0 {
		t.Fatalf("no tools requested must not set tool_choice, got %+v choice=%q", fs.gotReq.Tools, fs.gotReq.ToolChoice)
	}
}

func TestModelEmptyToolArgsBecomesObject(t *testing.T) {
	fs := &fakeChatStreamer{res: &core.ChatResult{FinishReason: "tool_calls",
		ToolCalls: []core.ToolCall{{ID: "c", Function: core.ToolCallFunction{Name: "observe_nodes", Arguments: ""}}}}}
	m := NewModel(fs, "vk", "m")
	resp, _ := m.Generate(context.Background(), agent.ModelRequest{}, nil, nil)
	if string(resp.Message.ToolUses()[0].Input) != `{}` {
		t.Fatalf("empty arguments must normalize to {}, got %s", resp.Message.ToolUses()[0].Input)
	}
}

func TestModelLengthStopMapsMaxTokens(t *testing.T) {
	fs := &fakeChatStreamer{res: &core.ChatResult{Content: "truncated", FinishReason: "length"}}
	resp, _ := NewModel(fs, "vk", "m").Generate(context.Background(), agent.ModelRequest{}, nil, nil)
	if resp.StopReason != agent.StopMaxTokens {
		t.Fatalf("finish_reason length -> StopMaxTokens, got %v", resp.StopReason)
	}
}

func TestModelAssistantToolCallOnlyOmitsEmptyText(t *testing.T) {
	// An assistant turn that is purely tool_calls (no text) translates to a wire
	// row whose Content is empty but whose tool_calls are present.
	fs := &fakeChatStreamer{res: &core.ChatResult{FinishReason: "stop"}}
	m := NewModel(fs, "vk", "m")
	_, _ = m.Generate(context.Background(), agent.ModelRequest{Messages: []agent.Message{
		{Role: agent.RoleAssistant, Blocks: []agent.Block{
			{Type: agent.BlockToolUse, ID: "x", ToolName: "observe_cost", Input: json.RawMessage(``)},
		}},
	}}, nil, nil)
	row := fs.gotReq.Messages[0]
	if row.Role != "assistant" || row.Content != "" || len(row.ToolCalls) != 1 || row.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("tool-call-only assistant row wrong: %+v", row)
	}
}

func TestModelForwardsReasoningCallback(t *testing.T) {
	fs := &fakeChatStreamer{res: &core.ChatResult{Content: "ok", FinishReason: "stop"}, reasoning: "internal monologue"}
	m := NewModel(fs, "vk", "m")
	var reasoning, text string
	_, err := m.Generate(context.Background(), agent.ModelRequest{},
		func(s string) { text += s },
		func(s string) { reasoning += s })
	if err != nil {
		t.Fatal(err)
	}
	if reasoning != "internal monologue" {
		t.Fatalf("Generate must forward the reasoning callback to the streamer, got %q", reasoning)
	}
	if strings.Contains(text, "monologue") {
		t.Fatalf("reasoning must not arrive on the text callback, got %q", text)
	}
}

func TestModelPropagatesStreamerError(t *testing.T) {
	m := NewModel(&fakeChatStreamer{err: errors.New("403 PII detected")}, "vk", "m")
	_, err := m.Generate(context.Background(), agent.ModelRequest{}, nil, nil)
	if err == nil || err.Error() != "403 PII detected" {
		t.Fatalf("streamer error must propagate verbatim (PII block surfaced), got %v", err)
	}
}
