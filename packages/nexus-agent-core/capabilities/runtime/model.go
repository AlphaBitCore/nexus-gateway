package runtime

import (
	"context"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// chatStreamer is the slice of *core.Client the Model needs. A seam so the Model
// is unit-testable with a fake instead of a live gateway.
type chatStreamer interface {
	ChatToolStream(ctx context.Context, vkSecret string, req core.ChatRequest, onDelta, onReasoning func(string)) (*core.ChatResult, error)
}

// Model is the kernel's agent.Model implemented over the AI Gateway's VK
// chat-completions SSE with native tools/tool_calls. It translates the kernel's
// content-block messages to the OpenAI wire and back.
type Model struct {
	streamer chatStreamer
	vkSecret string
	model    string
}

// NewModel builds the gateway-backed model. vkSecret is the Virtual Key the
// agent's inference accrues to; model is the slug to route.
func NewModel(s chatStreamer, vkSecret, model string) *Model {
	return &Model{streamer: s, vkSecret: vkSecret, model: model}
}

var _ agent.Model = (*Model)(nil)

// Generate runs one assistant turn: translate -> stream -> translate back.
func (m *Model) Generate(ctx context.Context, req agent.ModelRequest, onText, onReasoning func(string)) (*agent.ModelResponse, error) {
	creq := core.ChatRequest{Model: m.model, Messages: toWireMessages(req.System, req.Messages)}
	if len(req.Tools) > 0 {
		creq.Tools = toWireTools(req.Tools)
		creq.ToolChoice = "auto"
	}
	res, err := m.streamer.ChatToolStream(ctx, m.vkSecret, creq, onText, onReasoning)
	if err != nil {
		return nil, err
	}
	return toModelResponse(res), nil
}

// toWireMessages flattens the kernel's content-block messages into OpenAI rows.
// A user message's tool_result blocks each become a {role:"tool"} row; its text
// blocks become a {role:"user"} row. An assistant message becomes one row with
// text content + tool_calls.
func toWireMessages(system string, msgs []agent.Message) []core.ChatMessage {
	out := make([]core.ChatMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, core.ChatMessage{Role: "system", Content: system})
	}
	for _, msg := range msgs {
		switch msg.Role {
		case agent.RoleAssistant:
			row := core.ChatMessage{Role: "assistant", Content: msg.Text()}
			for _, b := range msg.Blocks {
				if b.Type == agent.BlockToolUse {
					args := string(b.Input)
					if args == "" {
						args = "{}"
					}
					row.ToolCalls = append(row.ToolCalls, core.ToolCall{
						ID: b.ID, Type: "function",
						Function: core.ToolCallFunction{Name: b.ToolName, Arguments: args},
					})
				}
			}
			out = append(out, row)
		default: // user
			var text string
			for _, b := range msg.Blocks {
				switch b.Type {
				case agent.BlockToolResult:
					out = append(out, core.ChatMessage{Role: "tool", ToolCallID: b.ID, Content: b.Text})
				case agent.BlockText:
					text += b.Text
				}
			}
			if text != "" {
				out = append(out, core.ChatMessage{Role: "user", Content: text})
			}
		}
	}
	return out
}

// toWireTools maps kernel tool schemas to the OpenAI tools[] shape.
func toWireTools(schemas []agent.ToolSchema) []core.ChatTool {
	out := make([]core.ChatTool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, core.ChatTool{Type: "function", Function: core.ChatToolFunction{
			Name: s.Name, Description: s.Description, Parameters: s.Parameters,
		}})
	}
	return out
}

// toModelResponse builds the assistant content-block message from the wire result.
func toModelResponse(res *core.ChatResult) *agent.ModelResponse {
	msg := agent.Message{Role: agent.RoleAssistant}
	if res.Content != "" {
		msg.Blocks = append(msg.Blocks, agent.Block{Type: agent.BlockText, Text: res.Content})
	}
	for _, tc := range res.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		msg.Blocks = append(msg.Blocks, agent.Block{
			Type: agent.BlockToolUse, ID: tc.ID, ToolName: tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}
	resp := &agent.ModelResponse{Message: msg, StopReason: stopReason(res.FinishReason)}
	if res.Usage != nil {
		resp.Usage = &agent.Usage{
			PromptTokens:     res.Usage.PromptTokens,
			CompletionTokens: res.Usage.CompletionTokens,
			TotalTokens:      res.Usage.TotalTokens,
			CachedTokens:     res.Usage.PromptTokensDetails.CachedTokens,
		}
	}
	return resp
}

// stopReason maps an OpenAI finish_reason to the kernel StopReason.
func stopReason(fr string) agent.StopReason {
	switch fr {
	case "tool_calls":
		return agent.StopToolUse
	case "length":
		return agent.StopMaxTokens
	default:
		return agent.StopEndTurn
	}
}
