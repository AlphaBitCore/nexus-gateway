package codecs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// readCorpusWire and wantIntPtr are shared with the sibling codec parity
// tests (openai_chat_parity_test.go): the conformance corpus is the
// single source of truth for the bytes asserted here.

// anthropicSSEMeta mirrors the meta.json of the keyed anthropic SSE
// corpus cases (adapterType anthropic, response direction, stream flag
// set, /v1/messages path).
func anthropicSSEMeta() core.Meta {
	return core.Meta{
		AdapterType:  "anthropic",
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1/messages",
		Stream:       true,
	}
}

func TestLooksLikeAnthropicSSE(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"corpus anthropic stream", readCorpusWire(t, "anthropic-sse-text"), true},
		{"event line no space", []byte("event:message_start\ndata: {}\n"), true},
		{"leading whitespace", []byte("\r\n  event: message_start\n"), true},
		{"data-first message_start", []byte(`data: {"type":"message_start","message":{}}`), true},
		{"openai SSE", readCorpusWire(t, "openai-sse-text"), false},
		{"mid-stream event first", []byte("event: content_block_delta\ndata: {}\n"), false},
		{"plain anthropic JSON", []byte(`{"type":"message","model":"claude","content":[]}`), false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeAnthropicSSE(tc.raw); got != tc.want {
				t.Errorf("LooksLikeAnthropicSSE = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnthropicSSE_ToolUseOnlyCorpus folds the prod-captured tool_use-only
// stream: a single tool_use block whose input arrives purely as
// input_json_delta fragments, with a heavily cached prompt.
func TestAnthropicSSE_ToolUseOnlyCorpus(t *testing.T) {
	raw := readCorpusWire(t, "anthropic-sse-tooluse-only")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw, anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIChat || !got.Stream {
		t.Errorf("kind/stream wrong: kind=%q stream=%v", got.Kind, got.Stream)
	}
	if got.Protocol != "anthropic-messages" || got.DetectedSpec != "anthropic-messages" {
		t.Errorf("protocol=%q detectedSpec=%q, want anthropic-messages", got.Protocol, got.DetectedSpec)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("model = %q, want claude-opus-4-7", got.Model)
	}
	if got.FinishReason != "tool_use" {
		t.Errorf("finishReason = %q, want tool_use", got.FinishReason)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (every frame recognized)", got.Confidence)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	msg := got.Messages[0]
	if msg.Role != core.RoleAssistant || len(msg.Content) != 1 || msg.Content[0].Type != core.ContentToolUse {
		t.Fatalf("expected single assistant tool_use block, got %+v", msg)
	}
	tu := msg.Content[0].ToolUse
	if tu.Name != "TaskUpdate" || tu.CallID != "toolu_01ZyXwVuTsRqPoNmLkJiHgFe" {
		t.Errorf("tool identity wrong: name=%q callId=%q", tu.Name, tu.CallID)
	}
	if tu.Input["taskId"] != "91" || tu.Input["status"] != "completed" {
		t.Errorf("accumulated tool input wrong: %+v", tu.Input)
	}
	// Wire usage: input_tokens=1, cache_creation=1758, cache_read=968742,
	// output=91. Canonical PromptTokens = 1+1758+968742.
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	wantIntPtr(t, "promptTokens", got.Usage.PromptTokens, 970501)
	wantIntPtr(t, "completionTokens", got.Usage.CompletionTokens, 91)
	wantIntPtr(t, "totalTokens", got.Usage.TotalTokens, 970592)
	wantIntPtr(t, "cacheReadTokens", got.Usage.CacheReadTokens, 968742)
	wantIntPtr(t, "cacheCreationTokens", got.Usage.CacheCreationTokens, 1758)
}

// TestAnthropicSSE_ToolUseBashCorpus folds the prod-captured Bash tool
// stream: 12 input_json_delta fragments carrying a multi-line shell
// command that must reassemble into valid JSON input.
func TestAnthropicSSE_ToolUseBashCorpus(t *testing.T) {
	raw := readCorpusWire(t, "anthropic-sse-tooluse-bash")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw, anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "claude-opus-4-7" || got.FinishReason != "tool_use" {
		t.Errorf("model=%q finishReason=%q", got.Model, got.FinishReason)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", got.Confidence)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 {
		t.Fatalf("expected 1 message with 1 block, got %+v", got.Messages)
	}
	tu := got.Messages[0].Content[0].ToolUse
	if tu == nil || tu.Name != "Bash" || tu.CallID != "toolu_01MnBvCxZaSdFgHjKlQwErTy" {
		t.Fatalf("tool identity wrong: %+v", tu)
	}
	cmd, _ := tu.Input["command"].(string)
	if !strings.Contains(cmd, "go test -race -count=1 -timeout 60s") || !strings.Contains(cmd, "check-go-coverage.sh --staged") {
		t.Errorf("multi-fragment command not reassembled: %q", cmd)
	}
	if desc, _ := tu.Input["description"].(string); desc != "test + coverage gate" {
		t.Errorf("description = %q", desc)
	}
	if timeout, _ := tu.Input["timeout"].(float64); timeout != 180000 {
		t.Errorf("timeout = %v, want 180000", tu.Input["timeout"])
	}
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	// input_tokens=1 + cache_creation=3673 + cache_read=964615 = 968289.
	wantIntPtr(t, "promptTokens", got.Usage.PromptTokens, 968289)
	wantIntPtr(t, "completionTokens", got.Usage.CompletionTokens, 354)
	wantIntPtr(t, "totalTokens", got.Usage.TotalTokens, 968643)
	wantIntPtr(t, "cacheReadTokens", got.Usage.CacheReadTokens, 964615)
	wantIntPtr(t, "cacheCreationTokens", got.Usage.CacheCreationTokens, 3673)
}

// TestAnthropicSSE_TextCorpus folds the pure text-delta control case:
// two text_delta frames concatenate into the assistant sentence, the
// model is captured from message_start and the stop reason from
// message_delta.
func TestAnthropicSSE_TextCorpus(t *testing.T) {
	raw := readCorpusWire(t, "anthropic-sse-text")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw, anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q, want claude-haiku-4-5-20251001", got.Model)
	}
	if got.FinishReason != "end_turn" {
		t.Errorf("finishReason = %q, want end_turn", got.FinishReason)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", got.Confidence)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 {
		t.Fatalf("expected 1 message with 1 text block, got %+v", got.Messages)
	}
	blk := got.Messages[0].Content[0]
	wantText := "A gateway is a device or software that connects two different networks and controls the flow of data between them."
	if blk.Type != core.ContentText || blk.Text != wantText {
		t.Errorf("text block = %+v, want concatenated deltas %q", blk, wantText)
	}
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	wantIntPtr(t, "promptTokens", got.Usage.PromptTokens, 17)
	wantIntPtr(t, "completionTokens", got.Usage.CompletionTokens, 24)
	wantIntPtr(t, "totalTokens", got.Usage.TotalTokens, 41)
}

// TestAnthropicSSE_ThinkingCorpus folds the extended-thinking capture:
// thinking_delta frames become a reasoning block (signature_delta is
// recognized but contributes no content), the visible answer follows as
// a text block, and the wire's explicit thinking-token count lands in
// Usage.ReasoningTokens.
func TestAnthropicSSE_ThinkingCorpus(t *testing.T) {
	raw := readCorpusWire(t, "anthropic-sse-thinking")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw, anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "claude-sonnet-4-6" || got.FinishReason != "end_turn" {
		t.Errorf("model=%q finishReason=%q", got.Model, got.FinishReason)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (signature_delta counts as recognized)", got.Confidence)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 {
		t.Fatalf("expected reasoning + text blocks, got %+v", got.Messages)
	}
	reasoning := got.Messages[0].Content[0]
	wantThinking := "27 * 43\n\n= 27 * 40 + 27 * 3\n= 1080 + 81\n= 1161"
	if reasoning.Type != core.ContentReasoning || reasoning.Text == "" {
		t.Fatalf("first block is not a populated reasoning block: %+v", reasoning)
	}
	if reasoning.Text != wantThinking {
		t.Errorf("reasoning text = %q, want concatenated thinking deltas %q", reasoning.Text, wantThinking)
	}
	text := got.Messages[0].Content[1]
	if text.Type != core.ContentText || !strings.HasPrefix(text.Text, "## Solving 27 × 43 Step by Step") ||
		!strings.HasSuffix(text.Text, "**Answer: 27 × 43 = 1,161**") {
		t.Errorf("text block wrong: %+v", text)
	}
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	wantIntPtr(t, "promptTokens", got.Usage.PromptTokens, 48)
	wantIntPtr(t, "completionTokens", got.Usage.CompletionTokens, 158)
	wantIntPtr(t, "totalTokens", got.Usage.TotalTokens, 206)
	// message_delta carries output_tokens_details.thinking_tokens=41; the
	// explicit wire count must win over the char-length heuristic (which
	// would give len(thinking)*2/7 = 13).
	wantIntPtr(t, "reasoningTokens", got.Usage.ReasoningTokens, 41)
}

// TestAnthropicSSE_TruncatedMidFrame cuts the text corpus stream inside
// the message_delta frame: the folded prefix must survive (full text,
// message_start usage) with NO error, the lost stop_reason stays empty,
// and confidence drops to the recognized/total coverage ratio.
func TestAnthropicSSE_TruncatedMidFrame(t *testing.T) {
	raw := readCorpusWire(t, "anthropic-sse-text")
	cut := bytes.LastIndex(raw, []byte(`"stop_reason"`))
	if cut < 0 {
		t.Fatal("corpus wire lost its message_delta frame")
	}
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw[:cut], anthropicSSEMeta())
	if err != nil {
		t.Fatalf("truncated stream must fold without error, got %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 ||
		got.Messages[0].Content[0].Text != "A gateway is a device or software that connects two different networks and controls the flow of data between them." {
		t.Errorf("folded prefix lost the text content: %+v", got.Messages)
	}
	if got.FinishReason != "" {
		t.Errorf("finishReason = %q, want empty (message_delta was cut)", got.FinishReason)
	}
	// 6 intact frames (message_start, content_block_start, ping, 2
	// text_delta, content_block_stop) + 1 unparseable cut frame.
	if want := 6.0 / 7.0; got.Confidence != want {
		t.Errorf("confidence = %v, want %v (6 of 7 frames recognized)", got.Confidence, want)
	}
	if got.Usage == nil {
		t.Fatal("usage missing (message_start side must survive)")
	}
	// Only the message_start usage was seen: input=17, output=7.
	wantIntPtr(t, "promptTokens", got.Usage.PromptTokens, 17)
	wantIntPtr(t, "completionTokens", got.Usage.CompletionTokens, 7)
}

// TestAnthropicSSE_SniffWithoutStreamFlag drops the stream flag and the
// adapter hint from meta: the byte sniff alone must route the SSE body
// to the fold instead of failing the JSON response path.
func TestAnthropicSSE_SniffWithoutStreamFlag(t *testing.T) {
	raw := readCorpusWire(t, "anthropic-sse-text")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw,
		core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("sniffed SSE body must fold, got error: %v", err)
	}
	if got.Kind != core.KindAIChat || !got.Stream || got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("sniffed fold wrong: kind=%q stream=%v model=%q", got.Kind, got.Stream, got.Model)
	}
	if got.FinishReason != "end_turn" {
		t.Errorf("finishReason = %q, want end_turn", got.FinishReason)
	}
}

// TestAnthropicSSE_ForeignStreamRejected feeds an OpenAI-shaped SSE body
// through the anthropic codec with the stream flag set: no frame matches
// the Anthropic vocabulary, so the codec must decline with
// ErrUnsupported (never claim with zero recognized frames, which the
// registry would read as full confidence).
func TestAnthropicSSE_ForeignStreamRejected(t *testing.T) {
	raw := readCorpusWire(t, "openai-sse-text")
	_, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), raw, anthropicSSEMeta())
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported for openai-shaped stream, got %v", err)
	}
	// A JSON body with the stream flag set must decline the same way.
	_, err = NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(`{"model":"claude","content":[{"type":"text","text":"hi"}]}`), anthropicSSEMeta())
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported for non-SSE body on stream path, got %v", err)
	}
}

// TestAnthropicSSE_UnknownFramesLowerCoverage pins the coverage scoring
// frame by frame: unknown event types, unknown delta types, and deltas
// missing their payload field count toward the total but are not
// recognized, while empty data lines are not frames at all.
func TestAnthropicSSE_UnknownFramesLowerCoverage(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude","usage":{"input_tokens":5,"output_tokens":0}}}`,
		``,
		`data:`, // empty payload — skipped, not a frame
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"wat_delta","wat":"?"}}`, // unknown delta type
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta"}}`, // text_delta without text
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0}`, // delta object missing
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error"}}`, // unknown event type
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 8 frames total, 4 recognized (message_start, content_block_start,
	// first text_delta, message_delta).
	if want := 4.0 / 8.0; got.Confidence != want {
		t.Errorf("confidence = %v, want %v", got.Confidence, want)
	}
	if got.Messages[0].Content[0].Text != "Hi" {
		t.Errorf("recognized deltas must still fold: %+v", got.Messages[0].Content)
	}
	if got.FinishReason != "end_turn" {
		t.Errorf("finishReason = %q, want end_turn", got.FinishReason)
	}
}

// TestAnthropicSSE_TruncatedToolInputKeepsIdentity: when the stream cuts
// before the tool input JSON is complete, the unparseable fragment is
// dropped but the tool_use block keeps its name and call id, so audit
// readers still see WHICH tool was invoked.
func TestAnthropicSSE_TruncatedToolInputKeepsIdentity(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude","usage":{"input_tokens":4,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cut","name":"get_weather"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"S"}}`,
		``,
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages[0].Content) != 1 || got.Messages[0].Content[0].Type != core.ContentToolUse {
		t.Fatalf("expected tool_use block, got %+v", got.Messages[0].Content)
	}
	tu := got.Messages[0].Content[0].ToolUse
	if tu.Name != "get_weather" || tu.CallID != "toolu_cut" {
		t.Errorf("tool identity lost: %+v", tu)
	}
	if tu.Input != nil {
		t.Errorf("incomplete input JSON must not decode: %+v", tu.Input)
	}
}

// TestAnthropicSSE_HeadTruncatedToolInputOpensImplicitBlock: when the
// capture lost the content_block_start frame (head truncation), the
// surviving input_json_delta run must still surface its accumulated
// tool input as a tool_use block — with empty identity, since the
// CallID/Name rode on the lost frame. Counting the deltas as
// recognized while emitting no content would overstate fidelity.
func TestAnthropicSSE_HeadTruncatedToolInputOpensImplicitBlock(t *testing.T) {
	raw := strings.Join([]string{
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\": "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"rm -rf /tmp/x\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 {
		t.Fatalf("expected exactly one content block, got %+v", got.Messages)
	}
	block := got.Messages[0].Content[0]
	if block.Type != core.ContentToolUse {
		t.Fatalf("expected implicit tool_use block, got %q", block.Type)
	}
	tu := block.ToolUse
	if tu.Name != "" || tu.CallID != "" {
		t.Errorf("identity must stay empty (start frame lost), got %+v", tu)
	}
	if cmd, ok := tu.Input["command"].(string); !ok || cmd != "rm -rf /tmp/x" {
		t.Errorf("accumulated tool input lost: %+v", tu.Input)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (all 3 frames recognized)", got.Confidence)
	}
}

// TestAnthropicSSE_OversizedLineCountsAsLostFrame: a data line beyond the
// 8 MiB scanner cap aborts the walk; the unread tail must weigh on the
// coverage as one unrecognized frame so the registry sees the fold as
// partial rather than fully confident.
func TestAnthropicSSE_OversizedLineCountsAsLostFrame(t *testing.T) {
	raw := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n" +
		"data: {\"pad\":\"" + strings.Repeat("a", 9<<20) + "\"}\n"
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "claude" {
		t.Errorf("model = %q, want claude (frame before the oversized line)", got.Model)
	}
	if want := 1.0 / 2.0; got.Confidence != want {
		t.Errorf("confidence = %v, want %v (1 recognized + 1 lost tail)", got.Confidence, want)
	}
}

// TestAnthropicSSE_ThinkingDeltaTextKeyFallback accepts the SDK capture
// variant that ships reasoning text under "text" instead of "thinking",
// including when no content_block_start announced the block.
func TestAnthropicSSE_ThinkingDeltaTextKeyFallback(t *testing.T) {
	raw := strings.Join([]string{
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","text":"pondering"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blk := got.Messages[0].Content[0]
	if blk.Type != core.ContentReasoning || blk.Text != "pondering" {
		t.Fatalf("reasoning fallback block wrong: %+v", blk)
	}
	// No explicit thinking-token count on the wire → char heuristic:
	// len("pondering")=9 chars × 2/7 = 2.
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	wantIntPtr(t, "reasoningTokens", got.Usage.ReasoningTokens, 2)
}

// TestAnthropicSSE_ReasoningTokenHeuristicFloor: reasoning text shorter
// than the chars/3.5 granularity still reports 1 token, never 0 — a
// zero would make the row indistinguishable from "no reasoning".
func TestAnthropicSSE_ReasoningTokenHeuristicFloor(t *testing.T) {
	raw := strings.Join([]string{
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ok"}}`,
		``,
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Usage == nil {
		t.Fatal("usage missing")
	}
	wantIntPtr(t, "reasoningTokens", got.Usage.ReasoningTokens, 1)
}

// TestAnthropicSSE_ContentBlockStartWithoutBody: a content_block_start
// missing its content_block object is structurally recognized (it opens
// an untyped slot) and a degenerate tool_use stop with no name still
// emits an empty tool block rather than dropping the turn.
func TestAnthropicSSE_ContentBlockStartWithoutBody(t *testing.T) {
	raw := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_empty","name":"noop"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	got, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(raw), anthropicSSEMeta())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (all frames recognized)", got.Confidence)
	}
	if len(got.Messages[0].Content) != 1 || got.Messages[0].Content[0].ToolUse.Name != "noop" {
		t.Errorf("expected only the named empty tool block, got %+v", got.Messages[0].Content)
	}
	if got.Messages[0].Content[0].ToolUse.Input != nil {
		t.Errorf("no input deltas arrived — Input must stay nil: %+v", got.Messages[0].Content[0].ToolUse.Input)
	}
}
