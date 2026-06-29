package codecs

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestOpenAIChat_RequestSimple(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}],"temperature":0.7}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIChat {
		t.Errorf("kind = %v, want %v", got.Kind, core.KindAIChat)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Role != core.RoleSystem || got.Messages[0].Content[0].Text != "You are helpful." {
		t.Errorf("system message wrong: %+v", got.Messages[0])
	}
	if got.Messages[1].Role != core.RoleUser || got.Messages[1].Content[0].Text != "Hi" {
		t.Errorf("user message wrong: %+v", got.Messages[1])
	}
	if got.Params == nil || got.Params.Temperature == nil || *got.Params.Temperature != 0.7 {
		t.Errorf("temperature missing: %+v", got.Params)
	}
}

func TestOpenAIChat_RequestWithTools(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.Tools[0].ParametersJSONSchema["type"] != "object" {
		t.Errorf("parameters schema not preserved: %+v", got.Tools[0].ParametersJSONSchema)
	}
}

func TestOpenAIChat_RequestWithMultimodal(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(blocks))
	}
	if blocks[0].Type != core.ContentText || blocks[0].Text != "describe" {
		t.Errorf("first block: %+v", blocks[0])
	}
	if blocks[1].Type != core.ContentImageRef || blocks[1].ImageRef == nil || blocks[1].ImageRef.SpillKey == "" {
		t.Errorf("image block: %+v", blocks[1])
	}
}

func TestOpenAIChat_RequestEmptyMessagesYieldsErrUnsupported(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[]}`
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestOpenAIChat_RequestMalformedYieldsError(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte("not json"), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if errors.Is(err, core.ErrUnsupported) {
		t.Errorf("malformed JSON should be a parse error, not core.ErrUnsupported: %v", err)
	}
}

func TestOpenAIChat_NonStreamResponseSimple(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello!"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "Hello!" {
		t.Errorf("response message wrong: %+v", got.Messages)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Usage == nil || got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 {
		t.Errorf("usage prompt tokens wrong: %+v", got.Usage)
	}
}

func TestOpenAIChat_NonStreamResponseWithToolUse(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolUse {
		t.Fatalf("expected one tool_use block, got %+v", blocks)
	}
	if blocks[0].ToolUse.Name != "get_weather" || blocks[0].ToolUse.Input["city"] != "SF" {
		t.Errorf("tool_use wrong: %+v", blocks[0].ToolUse)
	}
}

func TestOpenAIChat_StreamResponseSimple(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
		``,
		`data: {"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	if got.Messages[0].Content[0].Text != "Hi there" {
		t.Errorf("assembled text = %q", got.Messages[0].Content[0].Text)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Usage == nil || *got.Usage.PromptTokens != 5 || *got.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if !got.Stream {
		t.Errorf("Stream flag not set")
	}
}

// Stream confidence is frame coverage: a stream interleaved with frames
// that parse as JSON but carry no chat-completion structure folds the
// recognized frames with proportionally lower confidence — the operator
// sees an honest "3 of 4 frames decoded" signal instead of a field-shape
// score read off the first frame.
func TestOpenAIChat_StreamConfidenceIsFrameCoverage(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
		``,
		`data: {"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: {"ping":"keepalive"}`, // parses, but no chat structure: total only
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence != 0.75 {
		t.Errorf("confidence = %v, want 0.75 (3 of 4 frames recognized)", got.Confidence)
	}
	if got.Messages[0].Content[0].Text != "Hi there" {
		t.Errorf("recognized frames not folded: %q", got.Messages[0].Content[0].Text)
	}
}

// Envelope-key recognition: frames that carry the chunk envelope with
// empty values — a content-filter prologue ({"id":"","model":"",
// "created":0,"choices":[],...}) or an empty-choices heartbeat — are
// recognized, so a real provider stream cannot be dragged below the
// registry claim threshold and demoted to the generic-http fallback by
// its own protocol chatter.
func TestOpenAIChat_StreamFilterPrologueAndHeartbeatRecognized(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"","model":"","created":0,"choices":[],"prompt_filter_results":[{"prompt_index":0}]}`,
		``,
		`data: {"choices":[]}`, // heartbeat: choices key present, empty
		``,
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (envelope keys recognize prologue + heartbeat frames)", got.Confidence)
	}
	if got.Kind != core.KindAIChat || got.Messages[0].Content[0].Text != "hi" {
		t.Errorf("stream content not folded: %+v", got.Messages)
	}
}

// A frame whose choices value is not an array (provider bug / foreign
// shape) keeps its envelope recognition but folds nothing from it — the
// rest of the stream still folds.
func TestOpenAIChat_StreamMalformedChoicesTreeFoldsRest(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":{"not":"an array"}}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		``,
	}, "\n")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Messages[0].Content[0].Text != "ok" {
		t.Errorf("remaining stream not folded: %+v", got.Messages)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (envelope key counts the malformed-choices frame)", got.Confidence)
	}
}

// An oversized line aborts the scanner mid-capture: the unread tail
// weighs on coverage like one undecodable frame, and the decodable
// prefix still folds — same contract as the anthropic fold.
func TestOpenAIChat_StreamOversizedLineWeighsOnCoverage(t *testing.T) {
	huge := strings.Repeat("x", 9*1024*1024) // > the scanner's 8 MiB line cap
	raw := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: {"pad":"` + huge + `"}`,
		``,
	}, "\n")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Messages[0].Content[0].Text != "hi" {
		t.Errorf("prefix not folded: %+v", got.Messages)
	}
	if want := 0.5; got.Confidence != want {
		t.Errorf("confidence = %v, want %v (1 recognized + 1 lost tail)", got.Confidence, want)
	}
}

// A clean full stream pins coverage 1.0 — and the Normalize wrapper must
// NOT overwrite the fold's frame coverage with the single-document
// field-shape score (which would read 0.9889 off the first frame).
func TestOpenAIChat_StreamConfidenceFullCoverage(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (every frame recognized; [DONE] is a sentinel)", got.Confidence)
	}
}

func TestOpenAIChat_StreamResponseEmptyYieldsErrUnsupported(t *testing.T) {
	// Empty stream — only the [DONE] marker.
	raw := "data: [DONE]\n\n"
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported on empty stream, got %v", err)
	}
}

// assertContentIsJSONArray marshals msg the same way the audit bridge
// persists response_normalized and asserts content is a JSON array (never
// null / missing / string). The prod smoke's P6b check is exactly this
// assertion (jsonb_typeof(messages[0].content) == 'array').
func assertContentIsJSONArray(t *testing.T, msg core.Message, wantLen int) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if len(probe.Content) == 0 || string(probe.Content) == "null" {
		t.Fatalf("content is null/missing on the wire: %s", string(b))
	}
	var arr []any
	if err := json.Unmarshal(probe.Content, &arr); err != nil {
		t.Fatalf("content is not a JSON array (%q): %v", string(probe.Content), err)
	}
	if len(arr) != wantLen {
		t.Fatalf("content array len = %d, want %d (%s)", len(arr), wantLen, string(probe.Content))
	}
}

// TestOpenAIChatStream_EmptyAssistantTurnNormalizesToContentArray pins the
// prod-smoke P6b regression: reasoning models (o3 / gpt-5.5 / kimi-k2.x)
// can return a structurally valid chat-completion SSE stream whose
// assistant turn carries ONLY a role delta + a finish_reason — no visible
// text, no echoed reasoning_content, no tool calls (the model spent the
// whole budget on hidden reasoning the provider did not stream back).
//
// Such a stream must still normalize to an ai-chat payload with exactly
// one assistant message whose content serializes as an empty JSON array
// `[]` (never null/missing). Before the fix, sawAny stayed false for these
// turns, the normalizer returned ErrUnsupported ("no chunks decoded"), and
// the Coordinator fell through to the Tier-3 generic-http verbatim
// fallback — producing a payload with NO messages[] at all, so
// response_normalized.messages[0].content was absent.
func TestOpenAIChatStream_EmptyAssistantTurnNormalizesToContentArray(t *testing.T) {
	n := NewOpenAIChatNormalizer()

	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "role_delta_plus_finish_reason",
			raw: strings.Join([]string{
				`data: {"model":"o3","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				``,
				`data: {"model":"o3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				``,
				`data: {"model":"o3","choices":[],"usage":{"prompt_tokens":5,"completion_tokens_details":{"reasoning_tokens":50}}}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
		},
		{
			name: "finish_reason_only_no_role_delta",
			raw: strings.Join([]string{
				`data: {"model":"gpt-5.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
		},
		{
			name: "role_then_explicit_null_content_then_finish",
			raw: strings.Join([]string{
				`data: {"model":"kimi-k2.6","choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`,
				``,
				`data: {"model":"kimi-k2.6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := n.Normalize(context.Background(), []byte(tc.raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != core.KindAIChat {
				t.Fatalf("kind = %q, want ai-chat", got.Kind)
			}
			if len(got.Messages) != 1 {
				t.Fatalf("messages = %d, want 1 (empty assistant turn must still produce a message)", len(got.Messages))
			}
			if got.Messages[0].Role != core.RoleAssistant {
				t.Errorf("role = %q, want assistant", got.Messages[0].Role)
			}
			if got.FinishReason != "stop" {
				t.Errorf("finish_reason = %q, want stop", got.FinishReason)
			}
			// The core assertion the prod smoke makes: content is an empty
			// JSON array on the wire, not null.
			assertContentIsJSONArray(t, got.Messages[0], 0)
		})
	}
}

// TestOpenAIChatStream_ReasoningAndTextStillProduceArrays guards the
// passing rows: reasoning+text and text-only streams must keep producing
// non-empty content arrays (the fix must not regress them).
func TestOpenAIChatStream_ReasoningAndTextStillProduceArrays(t *testing.T) {
	n := NewOpenAIChatNormalizer()

	reasoningPlusText := strings.Join([]string{
		`data: {"model":"kimi-k2.5","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"let me think"}}]}`,
		``,
		`data: {"model":"kimi-k2.5","choices":[{"index":0,"delta":{"content":"Answer"}}]}`,
		``,
		`data: {"model":"kimi-k2.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := n.Normalize(context.Background(), []byte(reasoningPlusText), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("reasoning+text: unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("reasoning+text: messages = %d, want 1", len(got.Messages))
	}
	// reasoning block + text block = 2 content parts.
	assertContentIsJSONArray(t, got.Messages[0], 2)
	if got.Messages[0].Content[0].Type != core.ContentReasoning || got.Messages[0].Content[1].Type != core.ContentText {
		t.Errorf("blocks = %+v, want [reasoning, text]", got.Messages[0].Content)
	}
}

func TestOpenAIChat_EmptyBodyYieldsErrUnsupported(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	for _, dir := range []core.Direction{core.DirectionRequest, core.DirectionResponse} {
		_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: dir})
		if !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("direction=%v: expected core.ErrUnsupported on empty body, got %v", dir, err)
		}
	}
}

func TestOpenAIChat_UnknownDirectionYieldsErrUnsupported(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("ufo")})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestOpenAIChat_ID(t *testing.T) {
	if id := NewOpenAIChatNormalizer().ID(); id != "openai-chat" {
		t.Errorf("ID = %q, want openai-chat", id)
	}
}

// TestOpenAIChatStream_MultiToolCall_NoIndexFallsBackToZero verifies that a
// single streaming tool-call delta with no explicit `index` field is correctly
// placed at aggregation-map slot 0.  This is the baseline single-tool-call
// path; the fix must not regress it.
func TestOpenAIChatStream_MultiToolCall_NoIndexFallsBackToZero(t *testing.T) {
	// No "index" field present in either delta — falls back to pos=0 for both.
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_A","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(
		context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolUse {
		t.Fatalf("expected exactly one ContentToolUse block, got %+v", blocks)
	}
	tu := blocks[0].ToolUse
	if tu.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", tu.Name)
	}
	if tu.CallID != "call_A" {
		t.Errorf("call id = %q, want call_A", tu.CallID)
	}
	if tu.Input["city"] != "Paris" {
		t.Errorf("tool input city = %v, want Paris", tu.Input["city"])
	}
}

// TestOpenAIChatStream_MultiToolCall_TwoInterleavedExplicitIndex verifies that
// two interleaved tool-call deltas with explicit index=0 and index=1 are
// preserved as two separate ContentToolUse blocks in the final aggregated
// response.  Before the fix indexOfToolCall always returned 0, causing all
// tool-call deltas to be merged into the single slot 0 — silently discarding
// every second tool call.
func TestOpenAIChatStream_MultiToolCall_TwoInterleavedExplicitIndex(t *testing.T) {
	// OpenAI sends parallel tool calls with interleaved deltas keyed by index.
	// Chunk 1: both tool calls start (name fragments).
	// Chunk 2: both tool calls receive argument fragments.
	// Chunk 3: finish.
	raw := strings.Join([]string{
		// First chunk — two tool calls announced in the same delta.
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"get_weather","arguments":""}},{"index":1,"id":"call_1","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		``,
		// Second chunk — arguments for tool call 0.
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}}]}`,
		``,
		// Third chunk — arguments for tool call 1.
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"tz\":\"Europe/Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(
		context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	// Must have exactly two ContentToolUse blocks — one per tool call.
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2 (one per tool call); got %+v", len(blocks), blocks)
	}
	if blocks[0].Type != core.ContentToolUse || blocks[1].Type != core.ContentToolUse {
		t.Fatalf("expected both blocks to be ContentToolUse; got types %v, %v", blocks[0].Type, blocks[1].Type)
	}
	tu0 := blocks[0].ToolUse
	tu1 := blocks[1].ToolUse
	if tu0.Name != "get_weather" {
		t.Errorf("tool[0] name = %q, want get_weather", tu0.Name)
	}
	if tu0.CallID != "call_0" {
		t.Errorf("tool[0] call_id = %q, want call_0", tu0.CallID)
	}
	if tu0.Input["city"] != "Paris" {
		t.Errorf("tool[0] input city = %v, want Paris", tu0.Input["city"])
	}
	if tu1.Name != "get_time" {
		t.Errorf("tool[1] name = %q, want get_time", tu1.Name)
	}
	if tu1.CallID != "call_1" {
		t.Errorf("tool[1] call_id = %q, want call_1", tu1.CallID)
	}
	if tu1.Input["tz"] != "Europe/Paris" {
		t.Errorf("tool[1] input tz = %v, want Europe/Paris", tu1.Input["tz"])
	}
}

// TestOpenAIChatStream_MultiToolCall_NonZeroBaseIndex verifies that a stream
// containing only tool-call deltas at index=2 (with no index=0 or index=1
// deltas first) is NOT merged into slot 0.  Before the fix indexOfToolCall
// always returned 0 regardless of the wire index, so this tool call would be
// incorrectly placed at slot 0 and its distinct identity (e.g. a third
// parallel tool call continuing from a prior stream segment) would be lost.
func TestOpenAIChatStream_MultiToolCall_NonZeroBaseIndex(t *testing.T) {
	// Only index=2 deltas — simulates a stream segment whose first two tool
	// calls (index=0 and index=1) were already flushed or belong to a prior
	// context window. The normalizer must honour the wire index rather than
	// forcing the slot to 0.
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":2,"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"nexus\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(
		context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolUse {
		t.Fatalf("expected exactly one ContentToolUse block at wire index 2; got %+v", blocks)
	}
	tu := blocks[0].ToolUse
	if tu.Name != "lookup" {
		t.Errorf("tool name = %q, want lookup", tu.Name)
	}
	if tu.CallID != "call_2" {
		t.Errorf("call_id = %q, want call_2", tu.CallID)
	}
	if tu.Input["q"] != "nexus" {
		t.Errorf("input q = %v, want nexus", tu.Input["q"])
	}
}
