package core

import "github.com/goccy/go-json"

// Chat wire structs: the OpenAI-shape chat-completions request/response the Chat
// Playground + agent use, plus the CP simulator-forward request.

// ChatMessage is one turn of an OpenAI-shape chat conversation. For an assistant
// turn that calls tools, ToolCalls is set and Content may be empty; for a tool
// result, Role is "tool", ToolCallID references the call, and Content is the
// result text.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ChatTool advertises a callable function to the model (OpenAI tools[] shape).
type ChatTool struct {
	Type     string           `json:"type"` // always "function"
	Function ChatToolFunction `json:"function"`
}

// ChatToolFunction is the function descriptor inside a ChatTool.
type ChatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is one function call the model emitted (assistant tool_calls[]) or a
// fully accumulated call from a stream.
type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the name + JSON-string arguments of a ToolCall.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest is the subset of the OpenAI chat-completions request the Chat
// Playground + agent send. ChatStream/ChatToolStream force streaming + usage on
// regardless.
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Tools         []ChatTool     `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions toggles the trailing usage frame on a streamed completion.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatUsage is the token accounting returned in the final stream chunk (and in
// a non-streamed completion). The Playground shows it per turn.
type ChatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// ChatResult is the assembled outcome of a (possibly tool-calling) completion:
// the assistant text, any tool calls, the finish reason, and token usage.
type ChatResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        *ChatUsage
}

// SimulatorForwardRequest is the body POSTed to the CP simulator-forward
// endpoint. The endpoint is admin-authed; the VK is the upstream credential it
// forwards under. An empty TargetURL lets the server default to its gateway.
type SimulatorForwardRequest struct {
	TargetURL string          `json:"targetUrl,omitempty"`
	Path      string          `json:"path"`
	Method    string          `json:"method"`
	VK        string          `json:"vk"`
	Body      json.RawMessage `json:"body,omitempty"`
}
