package agent

import (
	"context"
	"github.com/goccy/go-json"
)

// ToolSchema is a tool advertised to the model: name, human description, and a
// JSON Schema for its arguments. The loop builds these from the Registry.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ModelRequest is one model call: a system prompt, the conversation so far, and
// the tools currently exposed (narrowed when a skill is active).
type ModelRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSchema
}

// StopReason is why a model turn ended.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
)

// Usage is token accounting for a turn (the kernel's own type so the package
// stays decoupled from core; Layer 2 translates core.ChatUsage into this).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
}

// ModelResponse is the assistant turn: the assembled assistant message (text +
// any tool_use blocks), the stop reason, and usage.
type ModelResponse struct {
	Message    Message
	StopReason StopReason
	Usage      *Usage
}

// Model is the kernel's only outbound dependency for inference. Generate runs
// one assistant turn: it streams assistant text to onText and the model's
// reasoning/thinking channel to onReasoning (both for live display) and returns
// the full assistant message plus stop reason and usage. Either callback may be
// nil. Implementations must populate Message.Blocks with the text and tool_use
// blocks the model produced; reasoning is display-only and is not persisted to
// the conversation, so it never enters Message.Blocks.
type Model interface {
	Generate(ctx context.Context, req ModelRequest, onText, onReasoning func(string)) (*ModelResponse, error)
}
