package agent

import "github.com/goccy/go-json"

// Role is the author of a message. The kernel models conversations as Claude
// Code does: a role plus an ordered list of typed content blocks.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType discriminates the content-block union.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Block is one content block. Field use by Type:
//   - BlockText:       Text
//   - BlockToolUse:    ID (call id), ToolName, Input (JSON args)
//   - BlockToolResult: ID (the tool_use id it answers), Text, IsError
type Block struct {
	Type     BlockType       `json:"type"`
	Text     string          `json:"text,omitempty"`
	ID       string          `json:"id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
}

// Message is a role plus its ordered content blocks.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// TextMessage builds a single-text-block message (the common user turn).
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// ToolResult builds a tool_result block answering a tool_use id.
func ToolResult(toolUseID, text string, isErr bool) Block {
	return Block{Type: BlockToolResult, ID: toolUseID, Text: text, IsError: isErr}
}

// Text returns the concatenation of all text blocks.
func (m Message) Text() string {
	var s string
	for _, b := range m.Blocks {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// ToolUses returns the tool_use blocks in order.
func (m Message) ToolUses() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}
