package codecs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"

	"github.com/tidwall/gjson"
)

// helper: build a core.NormalizedPayload with one assistant message + blocks.
func payloadWithAssistant(blocks ...core.ContentBlock) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind: core.KindAIChat,
		Messages: []core.Message{{
			Role:    core.RoleAssistant,
			Content: blocks,
		}},
	}
}

func intPtr(v int) *int { return &v }

// TestProject_TextOnly is the simplest happy path — one text block
// becomes `choices[0].message.content` verbatim.
func TestProject_TextOnly(t *testing.T) {
	p := payloadWithAssistant(core.ContentBlock{Type: core.ContentText, Text: "Hello, world."})
	body, err := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{
		ID:           "msg-abc",
		Model:        "claude-3-7-sonnet",
		Created:      1716908532,
		FinishReason: "stop",
	})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	r := gjson.ParseBytes(body)
	if r.Get("id").String() != "msg-abc" {
		t.Errorf("id=%q want msg-abc", r.Get("id").String())
	}
	if r.Get("object").String() != "chat.completion" {
		t.Errorf("object=%q want chat.completion", r.Get("object").String())
	}
	if r.Get("model").String() != "claude-3-7-sonnet" {
		t.Errorf("model=%q want claude-3-7-sonnet", r.Get("model").String())
	}
	if r.Get("created").Int() != 1716908532 {
		t.Errorf("created=%d want 1716908532", r.Get("created").Int())
	}
	if got := r.Get("choices.0.message.content").String(); got != "Hello, world." {
		t.Errorf("content=%q want %q", got, "Hello, world.")
	}
	if got := r.Get("choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("finish_reason=%q want stop", got)
	}
	if r.Get("choices.0.message.reasoning_content").Exists() {
		t.Errorf("reasoning_content should not exist for text-only response")
	}
	if r.Get("choices.0.message.tool_calls").Exists() {
		t.Errorf("tool_calls should not exist for text-only response")
	}
}

// TestProject_TextPlusReasoning verifies OpenAI o-series + Anthropic
// extended-thinking convention: reasoning rides in `reasoning_content`,
// not stripped, not merged into content.
func TestProject_TextPlusReasoning(t *testing.T) {
	p := payloadWithAssistant(
		core.ContentBlock{Type: core.ContentReasoning, Text: "The user asked X. I should answer Y because…"},
		core.ContentBlock{Type: core.ContentText, Text: "Y."},
	)
	body, err := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	r := gjson.ParseBytes(body)
	if got := r.Get("choices.0.message.content").String(); got != "Y." {
		t.Errorf("content=%q want Y.", got)
	}
	if got := r.Get("choices.0.message.reasoning_content").String(); got != "The user asked X. I should answer Y because…" {
		t.Errorf("reasoning_content=%q want canonical reasoning prefix", got)
	}
}

// TestProject_ToolCalls_AssignedID verifies the upstream tool_use id is
// preserved verbatim (Anthropic supplies one).
func TestProject_ToolCalls_AssignedID(t *testing.T) {
	p := payloadWithAssistant(
		core.ContentBlock{Type: core.ContentToolUse, ToolUse: &core.ToolUse{
			CallID: "toolu_01abc",
			Name:   "get_weather",
			Input:  map[string]any{"city": "NYC"},
		}},
	)
	body, err := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{
		ID:           "msg-tool",
		Model:        "claude-3-7-sonnet",
		FinishReason: "tool_calls",
	})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	r := gjson.ParseBytes(body)
	tc := r.Get("choices.0.message.tool_calls.0")
	if !tc.Exists() {
		t.Fatalf("tool_calls missing; body=%s", string(body))
	}
	if got := tc.Get("id").String(); got != "toolu_01abc" {
		t.Errorf("tool_call id=%q want toolu_01abc", got)
	}
	if got := tc.Get("type").String(); got != "function" {
		t.Errorf("tool_call type=%q want function", got)
	}
	if got := tc.Get("function.name").String(); got != "get_weather" {
		t.Errorf("function.name=%q want get_weather", got)
	}
	// arguments must be a JSON STRING (OpenAI contract), not nested JSON.
	argsRaw := tc.Get("function.arguments").String()
	if argsRaw == "" {
		t.Fatal("function.arguments empty")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsRaw), &args); err != nil {
		t.Fatalf("function.arguments not JSON-parseable string: %v (raw=%q)", err, argsRaw)
	}
	if args["city"] != "NYC" {
		t.Errorf("args.city=%v want NYC", args["city"])
	}
	// Tool-only response: content is JSON null (not "" or "null"-string).
	contentField := r.Get("choices.0.message.content")
	if contentField.Type != gjson.Null {
		t.Errorf("content should be JSON null when tool_calls present + no text; got type=%v value=%q", contentField.Type, contentField.String())
	}
}

// TestProject_ToolCalls_SynthID verifies the sha1-based id synthesis
// when the upstream omitted one (Gemini's functionCall doesn't carry
// one natively). Stable: same name + same args → same id.
func TestProject_ToolCalls_SynthID(t *testing.T) {
	p := payloadWithAssistant(
		core.ContentBlock{Type: core.ContentToolUse, ToolUse: &core.ToolUse{
			Name:  "get_weather",
			Input: map[string]any{"city": "NYC"},
		}},
	)
	body1, _ := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{Model: "gemini-2.5-pro"})
	body2, _ := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{Model: "gemini-2.5-pro"})
	id1 := gjson.GetBytes(body1, "choices.0.message.tool_calls.0.id").String()
	id2 := gjson.GetBytes(body2, "choices.0.message.tool_calls.0.id").String()
	if id1 == "" || id2 == "" {
		t.Fatalf("synthesised tool_call id should not be empty (got id1=%q id2=%q)", id1, id2)
	}
	if id1 != id2 {
		t.Errorf("synthesised id should be stable across calls; got %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "call_") {
		t.Errorf("synthesised id should start with call_; got %q", id1)
	}
}

// TestProject_EmptyAssistant covers the dry-run / refused / abstained
// case: no assistant message in payload → empty choice with finish_reason
// fallback.
func TestProject_EmptyAssistant(t *testing.T) {
	p := core.NormalizedPayload{Kind: core.KindAIChat} // no Messages
	body, err := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	r := gjson.ParseBytes(body)
	if got := r.Get("choices.0.message.content").String(); got != "" {
		t.Errorf("content=%q want empty for no-assistant payload", got)
	}
	if got := r.Get("choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("finish_reason=%q want stop fallback", got)
	}
}

// TestProject_UsageBlockPresent verifies the usage block is projected
// when meta.Usage is non-nil. The shape must match what
// openai_chat.go's extractCanonicalUsage produces on the inbound side
// so a project→extract round-trip is lossless.
func TestProject_UsageBlockPresent(t *testing.T) {
	p := payloadWithAssistant(core.ContentBlock{Type: core.ContentText, Text: "ok"})
	usage := &core.Usage{
		PromptTokens:     intPtr(120),
		CompletionTokens: intPtr(50),
		TotalTokens:      intPtr(170),
		CacheReadTokens:  intPtr(80),
		ReasoningTokens:  intPtr(20),
	}
	body, err := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{Model: "gpt-5", Usage: usage})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	r := gjson.ParseBytes(body)
	if got := r.Get("usage.prompt_tokens").Int(); got != 120 {
		t.Errorf("prompt_tokens=%d want 120", got)
	}
	if got := r.Get("usage.completion_tokens").Int(); got != 50 {
		t.Errorf("completion_tokens=%d want 50", got)
	}
	if got := r.Get("usage.total_tokens").Int(); got != 170 {
		t.Errorf("total_tokens=%d want 170", got)
	}
	if got := r.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 80 {
		t.Errorf("prompt_tokens_details.cached_tokens=%d want 80", got)
	}
	if got := r.Get("usage.completion_tokens_details.reasoning_tokens").Int(); got != 20 {
		t.Errorf("completion_tokens_details.reasoning_tokens=%d want 20", got)
	}
}

// TestProject_NoUsage verifies that omitting core.Usage omits the wire
// "usage" key entirely (matches OpenAI's streaming-without-usage-opt-in
// behaviour).
func TestProject_NoUsage(t *testing.T) {
	p := payloadWithAssistant(core.ContentBlock{Type: core.ContentText, Text: "ok"})
	body, _ := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{Model: "gpt-4o", Usage: nil})
	if gjson.GetBytes(body, "usage").Exists() {
		t.Errorf("usage key should not exist when core.Usage is nil; body=%s", string(body))
	}
}

// TestProject_AssistantFinishReasonFallback verifies the fallback chain:
// meta.FinishReason wins, then assistant.FinishReason, then "stop".
func TestProject_AssistantFinishReasonFallback(t *testing.T) {
	cases := []struct {
		name            string
		metaFinish      string
		assistantFinish string
		want            string
	}{
		{"meta wins", "tool_calls", "end_turn", "tool_calls"},
		{"assistant fallback", "", "length", "length"},
		{"stop default", "", "", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := core.NormalizedPayload{
				Kind:     core.KindAIChat,
				Messages: []core.Message{{Role: core.RoleAssistant, Content: []core.ContentBlock{{Type: core.ContentText, Text: "x"}}, FinishReason: tc.assistantFinish}},
			}
			body, _ := ProjectToOpenAIChatCompletion(p, ProjectionWireMetadata{FinishReason: tc.metaFinish})
			got := gjson.GetBytes(body, "choices.0.finish_reason").String()
			if got != tc.want {
				t.Errorf("finish_reason=%q want %q", got, tc.want)
			}
		})
	}
}
