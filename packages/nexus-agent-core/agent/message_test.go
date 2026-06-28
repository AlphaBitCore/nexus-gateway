package agent

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestMessageHelpers(t *testing.T) {
	m := Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: "checking "},
		{Type: BlockText, Text: "cost"},
		{Type: BlockToolUse, ID: "t1", ToolName: "observe_cost", Input: json.RawMessage(`{"window":"1h"}`)},
	}}
	if got := m.Text(); got != "checking cost" {
		t.Fatalf("Text() concatenates text blocks, got %q", got)
	}
	uses := m.ToolUses()
	if len(uses) != 1 || uses[0].ToolName != "observe_cost" || uses[0].ID != "t1" {
		t.Fatalf("ToolUses() returns only tool_use blocks, got %+v", uses)
	}

	// A text-only message has no tool_use blocks — len(ToolUses())==0 is the loop's
	// final-answer signal.
	textOnly := TextMessage(RoleAssistant, "all healthy")
	if len(textOnly.ToolUses()) != 0 {
		t.Fatalf("a text-only message has no tool_use blocks, got %+v", textOnly.ToolUses())
	}
}

func TestTextMessageAndToolResult(t *testing.T) {
	u := TextMessage(RoleUser, "what is my cost?")
	if u.Role != RoleUser || len(u.Blocks) != 1 || u.Blocks[0].Text != "what is my cost?" {
		t.Fatalf("TextMessage shape wrong: %+v", u)
	}
	r := ToolResult("t1", "cost is $4/hr", false)
	if r.Type != BlockToolResult || r.ID != "t1" || r.Text != "cost is $4/hr" || r.IsError {
		t.Fatalf("ToolResult shape wrong: %+v", r)
	}
	e := ToolResult("t2", "boom", true)
	if !e.IsError {
		t.Fatal("error tool_result must set IsError")
	}
}
