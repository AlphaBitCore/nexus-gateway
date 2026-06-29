package streaming

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// ---- fake per-host wire codecs (no traffic.Adapter dependency) -------------

// openAICodec decodes choices.0.delta.content — the OpenAI/Mistral/… wire.
type openAICodec struct{}

func (openAICodec) ChunkText(data string) (string, bool) {
	var c struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil || len(c.Choices) == 0 {
		return "", false
	}
	t := c.Choices[0].Delta.Content
	return t, t != ""
}

// anthropicCodec decodes content_block_delta.delta.text — a DIFFERENT wire
// shape, proving the splice logic is not OpenAI-only.
type anthropicCodec struct{}

func (anthropicCodec) ChunkText(data string) (string, bool) {
	var f struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(data), &f) != nil {
		return "", false
	}
	if f.Type == "content_block_delta" && f.Delta.Type == "text_delta" {
		return f.Delta.Text, f.Delta.Text != ""
	}
	return "", false
}

// constTextCodec always reports the same text regardless of the frame body, so
// a re-encoded frame never round-trips to the intended masked text — exercises
// the reencode verify fail-open path.
type constTextCodec struct{ text string }

func (c constTextCodec) ChunkText(string) (string, bool) { return c.text, c.text != "" }

// ---- helpers ---------------------------------------------------------------

func oaText(content string) *SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
	})
	return &SSEEvent{Event: "message", Data: string(b), Retry: -1}
}

func oaRole() *SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant"}}},
	})
	return &SSEEvent{Event: "message", Data: string(b), Retry: -1}
}

func oaToolCall() *SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "{\"q\":\"x\"}"}}},
		}}},
	})
	return &SSEEvent{Event: "message", Data: string(b), Retry: -1}
}

func anthText(text string) *SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return &SSEEvent{Event: "content_block_delta", Data: string(b), Retry: -1}
}

func doneEvt() *SSEEvent { return &SSEEvent{Event: "message", Data: "[DONE]", Retry: -1, Done: true} }

func serialize(evt *SSEEvent) string {
	var b bytes.Buffer
	_ = WriteSSEEvent(&b, evt)
	return b.String()
}

// reconstruct re-extracts each frame's text via the codec and concatenates it —
// the client-visible transcript.
func reconstruct(events []*SSEEvent, codec WireTextCodec) string {
	var b strings.Builder
	for _, e := range events {
		if t, ok := codec.ChunkText(e.Data); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

func modifyResult(masked string, spans ...normalize.TransformSpan) *core.CompliancePipelineResult {
	return &core.CompliancePipelineResult{
		Decision:        core.Modify,
		ModifiedContent: []core.ContentBlock{{Type: "text", Text: masked}},
		TransformSpans:  spans,
	}
}

func span(addr string, start, end int, repl string) normalize.TransformSpan {
	return normalize.TransformSpan{ContentAddress: addr, Start: start, End: end, Replacement: repl}
}

// ---- tests -----------------------------------------------------------------

// Buffered OpenAI SSE Modify: the spanned PII is masked across the text frames,
// the original PII never appears in the replayed timeline, and the role +
// [DONE] frames are byte-identical.
func TestSpliceTextFrames_OpenAI_MasksTextAcrossFrames(t *testing.T) {
	role := oaRole()
	done := doneEvt()
	// transcript: "My SSN is 555-12-3456." (offsets: 555-12-3456 = [10,21))
	events := []*SSEEvent{
		role,
		oaText("My SSN is "),
		oaText("555-12"),
		oaText("-3456"),
		oaText("."),
		done,
	}
	res := modifyResult("My SSN is [REDACTED].", span("messages.0.content.0", 10, 21, "[REDACTED]"))

	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if !ok {
		t.Fatalf("expected supported splice, got fail-open")
	}
	got := reconstruct(out, openAICodec{})
	if got != "My SSN is [REDACTED]." {
		t.Fatalf("masked transcript = %q, want %q", got, "My SSN is [REDACTED].")
	}
	for i, e := range out {
		if strings.Contains(e.Data, "555-12") || strings.Contains(e.Data, "3456") {
			t.Fatalf("frame %d still carries original PII: %s", i, e.Data)
		}
	}
	// role + [DONE] frames pass byte-verbatim.
	if serialize(out[0]) != serialize(role) {
		t.Errorf("role frame not byte-identical:\n got %q\nwant %q", serialize(out[0]), serialize(role))
	}
	if serialize(out[5]) != serialize(done) {
		t.Errorf("[DONE] frame not byte-identical")
	}
}

// A tool_call delta frame in the same timeline passes BYTE-IDENTICAL while the
// text is redacted around it.
func TestSpliceTextFrames_OpenAI_ToolCallFrameByteIdentical(t *testing.T) {
	tool := oaToolCall()
	events := []*SSEEvent{
		oaText("call me at "),
		tool,
		oaText("415-555-0000"),
		doneEvt(),
	}
	res := modifyResult("call me at [PHONE]", span("messages.0.content.0", 11, 23, "[PHONE]"))

	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if !ok {
		t.Fatalf("expected supported splice")
	}
	if serialize(out[1]) != serialize(tool) {
		t.Fatalf("tool_call frame not byte-identical:\n got %q\nwant %q", serialize(out[1]), serialize(tool))
	}
	if got := reconstruct(out, openAICodec{}); got != "call me at [PHONE]" {
		t.Fatalf("transcript = %q", got)
	}
	if strings.Contains(reconstruct(out, openAICodec{}), "415-555-0000") {
		t.Fatalf("PII leaked")
	}
}

// Non-OpenAI per-host wire (Anthropic-shaped frames) redacts correctly.
func TestSpliceTextFrames_Anthropic_Masks(t *testing.T) {
	events := []*SSEEvent{
		anthText("email "),
		anthText("a@b.com"),
		anthText(" now"),
		doneEvt(),
	}
	// transcript: "email a@b.com now"; a@b.com = [6,13)
	res := modifyResult("email [EMAIL] now", span("messages.0.content.0", 6, 13, "[EMAIL]"))

	out, ok := SpliceTextFrames(events, res, anthropicCodec{})
	if !ok {
		t.Fatalf("expected supported splice on Anthropic wire")
	}
	if got := reconstruct(out, anthropicCodec{}); got != "email [EMAIL] now" {
		t.Fatalf("Anthropic masked transcript = %q", got)
	}
	for _, e := range out {
		if strings.Contains(e.Data, "a@b.com") {
			t.Fatalf("Anthropic PII leaked: %s", e.Data)
		}
	}
}

// Multiple disjoint spans over multiple frames each apply exactly once.
func TestSpliceTextFrames_MultipleSpansMultipleFrames(t *testing.T) {
	events := []*SSEEvent{
		oaText("A 111 B "), // [0,8)
		oaText("middle "),  // [8,15)
		oaText("C 222 D"),  // [15,22)
		doneEvt(),
	}
	// "A 111 B middle C 222 D"; 111=[2,5), 222=[17,20)
	res := modifyResult("A <1> B middle C <2> D",
		span("messages.0.content.0", 2, 5, "<1>"),
		span("messages.0.content.0", 17, 20, "<2>"),
	)
	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if !ok {
		t.Fatalf("expected supported splice")
	}
	got := reconstruct(out, openAICodec{})
	if got != "A <1> B middle C <2> D" {
		t.Fatalf("transcript = %q", got)
	}
	if strings.Count(got, "<1>") != 1 || strings.Count(got, "<2>") != 1 {
		t.Fatalf("replacement count wrong: %q", got)
	}
}

// A span fully covering one frame and spilling into the next: replacement once,
// the fully-covered tail frame becomes empty content (no leak, still a frame).
func TestSpliceTextFrames_SpanCoversWholeFrame(t *testing.T) {
	events := []*SSEEvent{
		oaText("x"),      // [0,1)
		oaText("SECRET"), // [1,7)
		oaText("y"),      // [7,8)
		doneEvt(),
	}
	res := modifyResult("x[R]y", span("messages.0.content.0", 1, 7, "[R]"))
	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if !ok {
		t.Fatalf("expected supported splice")
	}
	if got := reconstruct(out, openAICodec{}); got != "x[R]y" {
		t.Fatalf("transcript = %q", got)
	}
	for _, e := range out {
		if strings.Contains(e.Data, "SECRET") {
			t.Fatalf("leak: %s", e.Data)
		}
	}
}

// Fail-open: a span addressing a tool-call argument leaf cannot be delivered on
// the wire → original events returned, not modified.
func TestSpliceTextFrames_ToolArgSpanFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("hi"), doneEvt()}
	res := modifyResult("hi", span("messages.0.content.0.toolUse.input.0", 0, 2, "x"))
	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if ok {
		t.Fatalf("expected fail-open for tool-arg span")
	}
	if len(out) == 0 || serialize(out[0]) != serialize(events[0]) {
		t.Fatalf("fail-open must return original events unchanged")
	}
}

// Fail-open: spans addressing more than one content block.
func TestSpliceTextFrames_MultiAddressFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("ab"), doneEvt()}
	res := modifyResult("xy",
		span("messages.0.content.0", 0, 1, "x"),
		span("messages.0.content.1", 1, 2, "y"),
	)
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open for multi-address spans")
	}
}

// Fail-open: a non-text content address (e.g. http body view).
func TestSpliceTextFrames_NonTextAddressFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("ab"), doneEvt()}
	res := modifyResult("xy", span("http.bodyView", 0, 1, "x"))
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open for non-text address")
	}
}

// Fail-open: the divergence fence — spliced transcript ≠ ModifiedContent.
func TestSpliceTextFrames_FenceMismatchFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("hello world"), doneEvt()}
	// span masks [0,5) "hello" → "BYE", spliced = "BYE world", but
	// ModifiedContent claims a different masked text.
	res := modifyResult("DIFFERENT", span("messages.0.content.0", 0, 5, "BYE"))
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open on fence mismatch")
	}
}

// Fail-open: empty ModifiedContent (no authoritative masked text to verify).
func TestSpliceTextFrames_EmptyModifiedContentFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("hello"), doneEvt()}
	res := &core.CompliancePipelineResult{
		Decision:       core.Modify,
		TransformSpans: []normalize.TransformSpan{span("messages.0.content.0", 0, 5, "X")},
	}
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open when ModifiedContent is empty")
	}
}

// Fail-open: a span offset outside the reconstructed transcript.
func TestSpliceTextFrames_OutOfRangeSpanFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("hi"), doneEvt()}
	res := modifyResult("hi", span("messages.0.content.0", 0, 99, "X"))
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open for out-of-range span")
	}
}

// Fail-open: overlapping spans.
func TestSpliceTextFrames_OverlappingSpansFailOpen(t *testing.T) {
	events := []*SSEEvent{oaText("abcdef"), doneEvt()}
	res := modifyResult("ZZ",
		span("messages.0.content.0", 0, 3, "Z"),
		span("messages.0.content.0", 2, 5, "Z"),
	)
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open for overlapping spans")
	}
}

// Fail-open: no text frames present though a span exists.
func TestSpliceTextFrames_NoTextFramesFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaRole(), oaToolCall(), doneEvt()}
	res := modifyResult("x", span("messages.0.content.0", 0, 1, "x"))
	if _, ok := SpliceTextFrames(events, res, openAICodec{}); ok {
		t.Fatalf("expected fail-open with no text frames")
	}
}

// Fail-open: re-encode cannot be verified (codec never round-trips).
func TestSpliceTextFrames_ReencodeVerifyFailsOpen(t *testing.T) {
	events := []*SSEEvent{oaText("hello"), doneEvt()}
	res := modifyResult("BYE", span("messages.0.content.0", 0, 5, "BYE"))
	// const codec: every frame "reads" as "hello" so wireText builds, fence
	// passes (masked "BYE" == ModifiedContent "BYE"? no — reconstruct uses
	// "hello"). Use a codec that reports "hello" for the original AND the
	// rewritten frame, so the masked aggregate ("BYE") matches ModifiedContent
	// but the re-encode verify (re-extract must equal "BYE") fails.
	if _, ok := SpliceTextFrames(events, res, constTextCodec{text: "hello"}); ok {
		t.Fatalf("expected fail-open when re-encode cannot be verified")
	}
}

// nil result / nil codec / no spans → fail-open.
func TestSpliceTextFrames_GuardInputs(t *testing.T) {
	ev := []*SSEEvent{oaText("a")}
	if _, ok := SpliceTextFrames(ev, nil, openAICodec{}); ok {
		t.Errorf("nil result should fail-open")
	}
	if _, ok := SpliceTextFrames(ev, modifyResult("a", span("messages.0.content.0", 0, 1, "x")), nil); ok {
		t.Errorf("nil codec should fail-open")
	}
	if _, ok := SpliceTextFrames(ev, &core.CompliancePipelineResult{Decision: core.Modify}, openAICodec{}); ok {
		t.Errorf("no spans should fail-open")
	}
}

// Concurrency: two parallel splices produce distinct, correct output with no
// shared state (run under -race).
func TestSpliceTextFrames_Concurrent(t *testing.T) {
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			secret := fmt.Sprintf("S%05d", n)
			events := []*SSEEvent{
				oaText("id "),
				oaText(secret),
				oaText(" end"),
				doneEvt(),
			}
			masked := "id [ID] end"
			res := modifyResult(masked, span("messages.0.content.0", 3, 3+len(secret), "[ID]"))
			out, ok := SpliceTextFrames(events, res, openAICodec{})
			if !ok {
				t.Errorf("goroutine %d: unexpected fail-open", n)
				return
			}
			got := reconstruct(out, openAICodec{})
			results[n] = got
			if strings.Contains(got, secret) {
				t.Errorf("goroutine %d leaked %s", n, secret)
			}
		}(i)
	}
	wg.Wait()
	for n, got := range results {
		if got != "id [ID] end" {
			t.Errorf("goroutine %d transcript = %q", n, got)
		}
	}
}

// jsonInner handles characters that need JSON escaping.
func TestJSONInner(t *testing.T) {
	if got := jsonInner(`a"b`); got != `a\"b` {
		t.Errorf("jsonInner = %q", got)
	}
	if got := jsonInner(""); got != "" {
		t.Errorf("jsonInner empty = %q", got)
	}
}

// phantomCodec reports a fixed text for a fixed sentinel frame body whose data
// does NOT literally contain that text — exercises the reencode "value not
// located" branch.
type phantomCodec struct{}

func (phantomCodec) ChunkText(data string) (string, bool) {
	if data == "PH" {
		return "ZZZ", true
	}
	return "", false
}

func TestReencodeChunkText_Branches(t *testing.T) {
	oa := openAICodec{}
	// not located: data has no "ZZZ" value.
	if _, ok := reencodeChunkText("PH", "ZZZ", "X", phantomCodec{}); ok {
		t.Errorf("expected not-located fail")
	}
	// masked == "" success: content emptied, codec sees no text.
	d := oaText("x").Data
	if nd, ok := reencodeChunkText(d, "x", "", oa); !ok || !strings.Contains(nd, `"content":""`) {
		t.Errorf("empty-mask success branch failed: ok=%v data=%s", ok, nd)
	}
	// masked == "" but codec still reports text → fail.
	if _, ok := reencodeChunkText(d, "x", "", constTextCodec{text: "y"}); ok {
		t.Errorf("expected fail when emptied frame still reports text")
	}
	// non-empty masked but re-extract mismatches → fail.
	if _, ok := reencodeChunkText(d, "x", "Y", constTextCodec{text: "x"}); ok {
		t.Errorf("expected fail on verify mismatch")
	}
	// normal success.
	if nd, ok := reencodeChunkText(d, "x", "Z", oa); !ok || !strings.Contains(nd, `"content":"Z"`) {
		t.Errorf("expected success, ok=%v data=%s", ok, nd)
	}
}

func TestMaskedMatchesModified_Branches(t *testing.T) {
	// non-text block skipped, text block matches.
	mc := []core.ContentBlock{{Type: "image", Text: "ignored"}, {Type: "text", Text: "ok"}}
	if !maskedMatchesModified("ok", mc) {
		t.Errorf("expected match with non-text block skipped")
	}
	// all-non-text → no authoritative text → false.
	if maskedMatchesModified("", []core.ContentBlock{{Type: "image", Text: "x"}}) {
		t.Errorf("expected false when no text block present")
	}
	// empty modified → false.
	if maskedMatchesModified("x", nil) {
		t.Errorf("expected false for empty ModifiedContent")
	}
	// empty Type treated as text.
	if !maskedMatchesModified("hi", []core.ContentBlock{{Text: "hi"}}) {
		t.Errorf("empty Type should count as text")
	}
}

func TestIsTextContentAddress(t *testing.T) {
	cases := map[string]bool{
		"messages.0.content.0":            true,
		"messages.12.content.3":           true,
		"messages.0.content.0.toolResult": false,
		"messages.x.content.0":            false,
		"messages.0.content.y":            false,
		"http.bodyView":                   false,
		"messages.0.parts.0":              false,
		"":                                false,
	}
	for addr, want := range cases {
		if got := isTextContentAddress(addr); got != want {
			t.Errorf("isTextContentAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

// spans supplied in descending order are sorted before the splice.
func TestSpliceTextFrames_DescendingSpanInput(t *testing.T) {
	events := []*SSEEvent{oaText("a 11 b 22 c"), doneEvt()}
	// "a 11 b 22 c": 11=[2,4), 22=[7,9); supply 22 before 11.
	res := modifyResult("a X b Y c",
		span("messages.0.content.0", 7, 9, "Y"),
		span("messages.0.content.0", 2, 4, "X"),
	)
	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if !ok {
		t.Fatalf("expected supported splice")
	}
	if got := reconstruct(out, openAICodec{}); got != "a X b Y c" {
		t.Fatalf("transcript = %q", got)
	}
}

// reencodeChunkText: a text value containing escapable characters round-trips.
func TestSpliceTextFrames_EscapingRoundTrips(t *testing.T) {
	events := []*SSEEvent{oaText(`say "hi" 555`), doneEvt()}
	// mask "555" = [9,12)
	res := modifyResult(`say "hi" [N]`, span("messages.0.content.0", 9, 12, "[N]"))
	out, ok := SpliceTextFrames(events, res, openAICodec{})
	if !ok {
		t.Fatalf("expected supported splice with escaped quotes")
	}
	if got := reconstruct(out, openAICodec{}); got != `say "hi" [N]` {
		t.Fatalf("transcript = %q", got)
	}
}
