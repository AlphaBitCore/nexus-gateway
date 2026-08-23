package codec

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// stagingReasoningCarryoverPayload is a trimmed copy of a real request that
// AlphaBitCore staging rejected on 2026-08-13. The shape is the load-bearing
// part: an assistant turn that carries `reasoning_content` (foreign reasoning
// text, produced by a non-Anthropic model on an earlier `auto` hop) together
// with tool_calls, followed by its tool result.
const stagingReasoningCarryoverPayload = `{
  "model": "claude-opus-4-6",
  "messages": [
    {"role": "system", "content": "You are executing a skill runbook."},
    {"role": "user", "content": "[skill_invoke caller context]"},
    {"role": "assistant", "content": "",
     "reasoning_content": "Let me understand the task. The user invoked skill report-template-assembly",
     "tool_calls": [{"id": "call_00_QcAN2fG2SfZ2TfJwfNMr4482", "type": "function",
                     "function": {"name": "bash", "arguments": "{\"command\":\"ls -la\"}"}}]},
    {"role": "tool", "content": "{\"ok\":false}", "tool_call_id": "call_00_QcAN2fG2SfZ2TfJwfNMr4482"}
  ]
}`

// TestReasoningContentNeverBecomesUnsignedThinking pins the rule that a
// `thinking` block may only exist when it carries the signature Anthropic
// itself minted.
//
// Provenance: staging rejected nine requests with
// "messages.N.content.0.thinking.signature: Field required". In every one, N
// mapped — after Anthropic hoists `system` out of the array — to the first
// assistant turn carrying `reasoning_content`. Assistant turns with tool_calls
// but no reasoning_content passed unharmed, which is what identified the
// promotion of that field as the cause. Foreign reasoning text can never carry
// a valid Anthropic signature, so it must not become a thinking block at all.
func TestReasoningContentNeverBecomesUnsignedThinking(t *testing.T) {
	_, msgs, err := splitMessages(gjson.Get(stagingReasoningCarryoverPayload, "messages"))
	if err != nil {
		t.Fatalf("splitMessages: %v", err)
	}

	sawAssistant := false
	for i, m := range msgs {
		if m["role"] == "assistant" {
			sawAssistant = true
		}
		parts, ok := m["content"].([]map[string]any)
		if !ok {
			continue
		}
		for j, p := range parts {
			if p["type"] != "thinking" {
				continue
			}
			if sig, _ := p["signature"].(string); sig == "" {
				enc, _ := json.Marshal(msgs)
				t.Fatalf("messages[%d].content[%d] is an unsigned thinking block — Anthropic answers "+
					"this with \"messages.%d.content.%d.thinking.signature: Field required\".\nGot: %s",
					i, j, i, j, enc)
			}
		}
	}
	if !sawAssistant {
		t.Fatal("test setup is wrong: the payload must survive into an assistant turn")
	}
}
