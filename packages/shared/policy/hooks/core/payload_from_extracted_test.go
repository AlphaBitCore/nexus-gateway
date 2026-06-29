package core

import (
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestPayloadFromExtracted_BuildsTextAndToolUseBlocks(t *testing.T) {
	segments := []string{"hello", "world"}
	toolCalls := []string{
		`{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"user@example.com\"}"}}`,
		`{"id":"call_2","type":"function","function":{"name":"noop","arguments":"{\"x\":\"y\"}"}}`,
	}
	p := PayloadFromExtracted(segments, toolCalls)
	if len(p.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(p.Messages))
	}
	content := p.Messages[0].Content
	// 2 text + 2 tool_use, text first.
	if len(content) != 4 {
		t.Fatalf("content blocks=%d want 4", len(content))
	}
	if content[0].Type != normalize.ContentText || content[0].Text != "hello" {
		t.Fatalf("block0=%+v", content[0])
	}
	if content[2].Type != normalize.ContentToolUse || content[2].ToolUse.CallID != "call_1" {
		t.Fatalf("block2=%+v", content[2])
	}
	if content[2].ToolUse.Input["q"].(string) != "user@example.com" {
		t.Fatalf("tool input not parsed: %v", content[2].ToolUse.Input)
	}
	// The tool argument leaf must be projected for detection.
	proj := p.TextProjection()
	var sawArg bool
	for _, s := range proj {
		if s == "user@example.com" {
			sawArg = true
		}
	}
	if !sawArg {
		t.Fatalf("tool arg leaf not in projection: %v", proj)
	}
}

func TestPayloadFromExtracted_SkipsNonFunctionAndLegacy(t *testing.T) {
	toolCalls := []string{
		`{"id":"r","type":"retrieval"}`,                                         // non-function → skip
		`{"name":"legacy","arguments":"{\"a\":\"b\"}"}`,                         // legacy function_call (no type) → skip
		`{"id":"f","type":"function","function":{"name":"f","arguments":"{}"}}`, // function → keep
		`not json`, // invalid → skip
	}
	p := PayloadFromExtracted(nil, toolCalls)
	var toolUse int
	for _, b := range p.Messages[0].Content {
		if b.Type == normalize.ContentToolUse {
			toolUse++
		}
	}
	if toolUse != 1 {
		t.Fatalf("tool_use blocks=%d want 1 (only the function-type call)", toolUse)
	}
}

func TestPayloadFromExtracted_UnparseableArgsFailOpen(t *testing.T) {
	// arguments is a JSON string but not an object → Input stays nil, no crash.
	toolCalls := []string{
		`{"id":"f","type":"function","function":{"name":"f","arguments":"\"just a string\""}}`,
	}
	p := PayloadFromExtracted(nil, toolCalls)
	b := p.Messages[0].Content[0]
	if b.Type != normalize.ContentToolUse {
		t.Fatalf("expected tool_use block")
	}
	if b.ToolUse.Input != nil {
		t.Fatalf("non-object arguments should leave Input nil, got %v", b.ToolUse.Input)
	}
	// Projection over a nil Input yields nothing — fail-open detection gap.
	if got := p.TextProjection(); len(got) != 0 {
		t.Fatalf("expected empty projection, got %v", got)
	}
}

func TestPayloadFromExtracted_EmptyInputs(t *testing.T) {
	p := PayloadFromExtracted(nil, nil)
	if len(p.Messages) != 0 || p.Kind != normalize.KindAIChat {
		t.Fatalf("empty inputs should yield an empty ai-chat payload, got %+v", p)
	}
}
