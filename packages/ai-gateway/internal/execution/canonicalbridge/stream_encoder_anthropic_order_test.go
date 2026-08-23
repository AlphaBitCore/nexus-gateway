package canonicalbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

type anthropicOrderEvent struct {
	name string
	data map[string]any
}

func parseAnthropicOrderEvents(t *testing.T, stream []byte) []anthropicOrderEvent {
	t.Helper()
	var events []anthropicOrderEvent
	for _, frame := range strings.Split(strings.TrimSpace(string(stream)), "\n\n") {
		if frame == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed Anthropic SSE frame %q", frame)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("decode Anthropic SSE frame %q: %v", frame, err)
		}
		events = append(events, anthropicOrderEvent{
			name: strings.TrimPrefix(lines[0], "event: "),
			data: data,
		})
	}
	return events
}

func anthropicOrderLabel(ev anthropicOrderEvent) string {
	if ev.name != "content_block_start" && ev.name != "content_block_delta" && ev.name != "content_block_stop" {
		return ev.name
	}
	idx := int(ev.data["index"].(float64))
	if ev.name == "content_block_start" {
		block := ev.data["content_block"].(map[string]any)
		return fmt.Sprintf("%s[%d]:%s", ev.name, idx, block["type"])
	}
	if ev.name == "content_block_delta" {
		delta := ev.data["delta"].(map[string]any)
		return fmt.Sprintf("%s[%d]:%s", ev.name, idx, delta["type"])
	}
	return fmt.Sprintf("%s[%d]", ev.name, idx)
}

func TestAnthropicStreamEncoder_SyntheticThinkingAndTextOrder(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	ctx := context.Background()

	body, err := enc.Write(ctx, provcore.Chunk{
		NexusThinking: []provcore.NexusThinkingBlock{{Index: 7, Thinking: "plan", Signature: "sig"}},
		Delta:         "answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := enc.Write(ctx, provcore.Chunk{Done: true})
	if err != nil {
		t.Fatal(err)
	}
	events := parseAnthropicOrderEvents(t, append(body, terminal...))
	var got []string
	for _, ev := range events {
		got = append(got, anthropicOrderLabel(ev))
	}
	want := []string{
		"message_start",
		"ping",
		"content_block_start[0]:thinking",
		"content_block_delta[0]:thinking_delta",
		"content_block_delta[0]:signature_delta",
		"content_block_stop[0]",
		"content_block_start[1]:text",
		"content_block_delta[1]:text_delta",
		"content_block_stop[1]",
		"message_delta",
		"message_stop",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

func TestAnthropicStreamEncoder_SyntheticThinkingAndToolOrder(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	ctx := context.Background()

	body, err := enc.Write(ctx, provcore.Chunk{
		NexusThinking:  []provcore.NexusThinkingBlock{{Index: 7, Thinking: "plan", Signature: "sig"}},
		ToolCallDeltas: []provcore.ToolCallDelta{{Index: 3, ID: "call", Name: "lookup", Arguments: `{"q":"x"}`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := enc.Write(ctx, provcore.Chunk{Done: true, FinishReason: "tool_calls"})
	if err != nil {
		t.Fatal(err)
	}
	events := parseAnthropicOrderEvents(t, append(body, terminal...))
	var got []string
	for _, ev := range events {
		got = append(got, anthropicOrderLabel(ev))
	}
	want := []string{
		"message_start",
		"ping",
		"content_block_start[0]:thinking",
		"content_block_delta[0]:thinking_delta",
		"content_block_delta[0]:signature_delta",
		"content_block_stop[0]",
		"content_block_start[1]:tool_use",
		"content_block_delta[1]:input_json_delta",
		"content_block_stop[1]",
		"message_delta",
		"message_stop",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}
