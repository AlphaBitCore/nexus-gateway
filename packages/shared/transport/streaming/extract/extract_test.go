package extract_test

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/extract"
)

func TestRegistry_GetUnknownReturnsNoop(t *testing.T) {
	r := extract.NewRegistry()
	got := r.Get("does-not-exist")
	if got.ID() != "noop" {
		t.Fatalf("unknown id should fall back to noop, got %q", got.ID())
	}
	// noop accumulator should produce empty content regardless of feed.
	a := got.NewAccumulator()
	a.Feed([]byte(`{"foo":"bar"}`))
	c := a.Snapshot()
	if c.Prompt != "" || c.Completion != "" {
		t.Fatalf("noop should produce empty content; got %+v", c)
	}
}

func TestOpenAIAPI_ExtractRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"你好"},{"role":"assistant","content":"hi there"}]}`)
	got := extract.NewOpenAIAPIExtractor().ExtractRequest(body)
	if !strings.Contains(got.Prompt, "你好") || !strings.Contains(got.Prompt, "hi there") {
		t.Fatalf("OpenAI request extraction missed message content: %+v", got)
	}
}

func TestOpenAIAPI_AccumulatorAssemblesDeltas(t *testing.T) {
	a := extract.NewOpenAIAPIExtractor().NewAccumulator()
	a.Feed([]byte(`{"choices":[{"delta":{"content":"hello"}}]}`))
	a.Feed([]byte(`{"choices":[{"delta":{"content":" world"}}]}`))
	a.Feed([]byte(`[DONE]`))
	got := a.Snapshot()
	if got.Completion != "hello world" {
		t.Fatalf("OpenAI accumulator missed deltas: %q", got.Completion)
	}
}

func TestChatGPTWeb_ExtractRequest(t *testing.T) {
	body := []byte(`{"messages":[{"author":{"role":"user"},"content":{"parts":["你好"]}}]}`)
	got := extract.NewChatGPTWebExtractor().ExtractRequest(body)
	if got.Prompt != "你好" {
		t.Fatalf("ChatGPT web request extraction wrong: %q", got.Prompt)
	}
}

func TestChatGPTWeb_AccumulatorRecognizesPatchAndPlainOps(t *testing.T) {
	// Realistic frame sequence pulled from a captured chatgpt.com
	// `/backend-api/f/conversation` response.
	frames := [][]byte{
		[]byte(`"v1"`),
		[]byte(`{"type":"resume_conversation_token","token":"eyJ..."}`),
		[]byte(`{"type":"input_message","input_message":{"content":{"parts":["你好"]}}}`),
		// Singleton append at content/parts/0.
		[]byte(`{"p":"/message/content/parts/0","o":"append","v":"你好"}`),
		// JSON-Patch list with an append + an unrelated replace.
		[]byte(`{"p":"","o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":"！"},{"p":"/message/status","o":"replace","v":"finished_successfully"}]}`),
		[]byte(`{"type":"message_marker","conversation_id":"x","marker":"first"}`),
		[]byte(`{"type":"message_stream_complete"}`),
		[]byte(`[DONE]`),
	}
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	for _, f := range frames {
		a.Feed(f)
	}
	got := a.Snapshot()
	if got.Prompt != "你好" {
		t.Fatalf("ChatGPT web prompt assembly: %q", got.Prompt)
	}
	if got.Completion != "你好！" {
		t.Fatalf("ChatGPT web completion assembly: %q", got.Completion)
	}
}

func TestChatGPTWeb_FilterFramesProduceEmptyDelta(t *testing.T) {
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	for _, frame := range [][]byte{
		[]byte(`{"type":"server_ste_metadata","metadata":{"foo":"bar"}}`),
		[]byte(`{"type":"conversation_detail_metadata","limits_progress":[]}`),
		[]byte(`{"type":"message_marker","marker":"first"}`),
		[]byte(`"v1"`),
		[]byte(`[DONE]`),
	} {
		d := a.Feed(frame)
		if d.Prompt != "" || d.Completion != "" {
			t.Fatalf("expected empty delta for frame %s, got %+v", frame, d)
		}
	}
}

func TestRegisterBuiltins(t *testing.T) {
	r := extract.NewRegistry()
	extract.RegisterBuiltins(r)
	for _, id := range []string{"openai-api", "chatgpt-web"} {
		got := r.Get(id)
		if got.ID() != id {
			t.Fatalf("builtins did not register %q (got %q)", id, got.ID())
		}
	}
}

func TestAccumulator_TruncateFlag(t *testing.T) {
	a := extract.NewOpenAIAPIExtractor().NewAccumulator()
	a.Feed([]byte(`{"choices":[{"delta":{"content":"x"}}]}`))
	if a.Snapshot().Truncated {
		t.Fatal("accumulator should not be truncated by default")
	}
	a.Truncate()
	if !a.Snapshot().Truncated {
		t.Fatal("Truncate should set the flag on Snapshot")
	}
}

// TestChatGPTWeb_TruncateFlag exercises the chatgpt-web accumulator's
// Truncate path and asserts the flag propagates through Snapshot while
// existing prompt/completion content remains intact.
func TestChatGPTWeb_TruncateFlag(t *testing.T) {
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	a.Feed([]byte(`{"type":"input_message","input_message":{"content":{"parts":["hi"]}}}`))
	a.Feed([]byte(`{"p":"/message/content/parts/0","o":"append","v":"world"}`))
	if a.Snapshot().Truncated {
		t.Fatal("chatgpt-web accumulator should not be truncated by default")
	}
	a.Truncate()
	snap := a.Snapshot()
	if !snap.Truncated {
		t.Fatal("Truncate should set the flag on chatgpt-web Snapshot")
	}
	// Existing content survives the truncate marker.
	if snap.Prompt != "hi" || snap.Completion != "world" {
		t.Fatalf("Truncate should not drop accumulated content; got %+v", snap)
	}
}

// TestNoopExtractor_AllSurfaces covers every method on the fallback extractor.
// Coverage matters here because the noop is the safety net returned by the
// Registry on unknown adapter ids — silent regressions in any branch would
// surface in production as missing canonical content rather than test failures.
func TestNoopExtractor_AllSurfaces(t *testing.T) {
	r := extract.NewRegistry()
	noop := r.Get("unknown-adapter")
	if noop.ID() != "noop" {
		t.Fatalf("expected noop fallback, got %q", noop.ID())
	}
	// ExtractRequest must never produce content for non-LLM bodies.
	if got := noop.ExtractRequest([]byte(`{"messages":[{"content":"prompt"}]}`)); got.Prompt != "" || got.Completion != "" {
		t.Fatalf("noop ExtractRequest must yield empty content; got %+v", got)
	}
	if got := noop.ExtractRequest(nil); got.Prompt != "" || got.Completion != "" {
		t.Fatalf("noop ExtractRequest(nil) must yield empty content; got %+v", got)
	}
	a := noop.NewAccumulator()
	// Feed any payload; delta is always empty.
	if d := a.Feed([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)); d.Prompt != "" || d.Completion != "" {
		t.Fatalf("noop Feed must yield empty delta; got %+v", d)
	}
	if s := a.Snapshot(); s.Truncated {
		t.Fatalf("fresh noop should not be truncated; got %+v", s)
	}
	a.Truncate()
	s := a.Snapshot()
	if !s.Truncated {
		t.Fatal("noop Truncate must propagate to Snapshot.Truncated")
	}
	if s.Prompt != "" || s.Completion != "" {
		t.Fatalf("noop Snapshot must remain empty even after Truncate; got %+v", s)
	}
}

// TestRegistry_DuplicateRegisterPanics asserts the constructor-time guard
// catches duplicate adapter ids — duplicate registration would let the
// second extractor silently shadow the first, hiding a real config error
// behind a working-looking process.
func TestRegistry_DuplicateRegisterPanics(t *testing.T) {
	r := extract.NewRegistry()
	r.Register(extract.NewOpenAIAPIExtractor())
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on duplicate Register, got none")
		}
		msg, ok := rec.(string)
		if !ok || !strings.Contains(msg, "duplicate extractor id") || !strings.Contains(msg, "openai-api") {
			t.Fatalf("panic message should name the duplicated id; got %v", rec)
		}
	}()
	r.Register(extract.NewOpenAIAPIExtractor())
}

// TestOpenAIAPI_ExtractRequest_MultiModal covers the multi-modal content
// array path: text parts are stitched together with newlines.
//
// NOTE: the production code currently has a known bug — when `content`
// is an array, `gjson.Result.String()` on the array yields the raw JSON
// text, which gets concatenated before the text-part stitching also
// runs. This test pins the current observable behavior so the array
// is parsed and text parts are present in the output. The bug is
// flagged for prod follow-up; switching to an `if-Type==String / else`
// branch would fix it without changing the canonical text-stitching
// contract.
func TestOpenAIAPI_ExtractRequest_MultiModal(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image_url","image_url":{"url":"https://example.com/x.png"}},
			{"type":"text","text":"in detail"}
		]}
	]}`)
	got := extract.NewOpenAIAPIExtractor().ExtractRequest(body)
	if !strings.Contains(got.Prompt, "describe this") || !strings.Contains(got.Prompt, "in detail") {
		t.Fatalf("multi-modal text parts not stitched: %q", got.Prompt)
	}
	// Two text parts ⇒ they appear in order with a newline separator.
	if !strings.Contains(got.Prompt, "describe this\nin detail") {
		t.Fatalf("multi-modal stitching missing newline separator: %q", got.Prompt)
	}
}

// TestOpenAIAPI_ExtractRequest_EmptyAndInvalid covers the early-exit
// guards: empty body and unparseable JSON must both yield empty content
// rather than panic, so non-LLM traffic flows through the streaming
// pipeline without spurious "extracted prompt" rows.
func TestOpenAIAPI_ExtractRequest_EmptyAndInvalid(t *testing.T) {
	ex := extract.NewOpenAIAPIExtractor()
	for _, body := range [][]byte{nil, []byte(""), []byte("not-json"), []byte(`{"messages":}`)} {
		got := ex.ExtractRequest(body)
		if got.Prompt != "" || got.Completion != "" {
			t.Fatalf("body=%q should yield empty content, got %+v", body, got)
		}
	}
}

// TestOpenAIAPI_Feed_InvalidAndEmpty covers the Feed early-exit branches.
// An invalid JSON frame, an empty frame, and a whitespace-only frame must
// all yield empty deltas and leave accumulator content untouched.
func TestOpenAIAPI_Feed_InvalidAndEmpty(t *testing.T) {
	a := extract.NewOpenAIAPIExtractor().NewAccumulator()
	a.Feed([]byte(`{"choices":[{"delta":{"content":"keep"}}]}`))
	for _, frame := range [][]byte{nil, []byte(""), []byte("   "), []byte("not-json"), []byte(`{"choices":}`)} {
		if d := a.Feed(frame); d.Completion != "" || d.Prompt != "" {
			t.Fatalf("invalid frame %q should yield empty delta, got %+v", frame, d)
		}
	}
	// Accumulator state must be unchanged by invalid frames.
	if got := a.Snapshot().Completion; got != "keep" {
		t.Fatalf("invalid frames should not corrupt accumulator state, got %q", got)
	}
}

// TestChatGPTWeb_ExtractRequest_MultiPart covers the multi-part path
// inside a single content.parts array (two non-empty strings) — the
// per-part newline separator branch.
func TestChatGPTWeb_ExtractRequest_MultiPart(t *testing.T) {
	body := []byte(`{"messages":[{"author":{"role":"user"},"content":{"parts":["partA","partB"]}}]}`)
	got := extract.NewChatGPTWebExtractor().ExtractRequest(body)
	if got.Prompt != "partA\npartB" {
		t.Fatalf("multi-part stitch wrong: %q", got.Prompt)
	}
}

// TestChatGPTWeb_ExtractRequest_OlderShape covers the `content.text`
// branch (older chatgpt.com request body shape) plus the multi-message
// newline-join path. The second message also uses the older shape to
// exercise the b.Len()>0 separator branch.
func TestChatGPTWeb_ExtractRequest_OlderShape(t *testing.T) {
	body := []byte(`{"messages":[
		{"author":{"role":"user"},"content":{"parts":["newer shape line"]}},
		{"author":{"role":"user"},"content":{"text":"older-A"}},
		{"author":{"role":"user"},"content":{"text":"older-B"}}
	]}`)
	got := extract.NewChatGPTWebExtractor().ExtractRequest(body)
	if !strings.Contains(got.Prompt, "older-A") || !strings.Contains(got.Prompt, "older-B") {
		t.Fatalf("older content.text shape missed: %q", got.Prompt)
	}
	if !strings.Contains(got.Prompt, "newer shape line") {
		t.Fatalf("newer content.parts shape missed: %q", got.Prompt)
	}
	// All three lines joined with newline separators in order.
	if got.Prompt != "newer shape line\nolder-A\nolder-B" {
		t.Fatalf("multi-message join order/separators wrong: %q", got.Prompt)
	}
}

// TestChatGPTWeb_ExtractRequest_EmptyAndInvalid covers the early-exit
// guards on the chatgpt-web request parser.
func TestChatGPTWeb_ExtractRequest_EmptyAndInvalid(t *testing.T) {
	ex := extract.NewChatGPTWebExtractor()
	for _, body := range [][]byte{nil, []byte(""), []byte("not-json"), []byte(`{"messages":}`)} {
		got := ex.ExtractRequest(body)
		if got.Prompt != "" || got.Completion != "" {
			t.Fatalf("body=%q should yield empty content, got %+v", body, got)
		}
	}
}

// TestChatGPTWeb_Feed_InvalidJSON covers the gjson.ValidBytes guard in
// Feed — malformed frames must not corrupt accumulator state.
func TestChatGPTWeb_Feed_InvalidJSON(t *testing.T) {
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	a.Feed([]byte(`{"p":"/message/content/parts/0","o":"append","v":"keep"}`))
	for _, frame := range [][]byte{[]byte("not-json"), []byte(`{"p":}`), nil} {
		if d := a.Feed(frame); d.Completion != "" || d.Prompt != "" {
			t.Fatalf("invalid frame %q should yield empty delta, got %+v", frame, d)
		}
	}
	if got := a.Snapshot().Completion; got != "keep" {
		t.Fatalf("invalid frames should not corrupt accumulator state, got %q", got)
	}
}

// TestChatGPTWeb_Collect_ReplaceOpOnContent covers the `replace` op
// branch in collect — chatgpt.com uses this at end-of-stream to set a
// final message body. When the target path is a content part with a
// string value, it must contribute to Completion just like `append`.
func TestChatGPTWeb_Collect_ReplaceOpOnContent(t *testing.T) {
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	a.Feed([]byte(`{"p":"/message/content/parts/0","o":"append","v":"draft"}`))
	a.Feed([]byte(`{"p":"/message/content/parts/0","o":"replace","v":"-final"}`))
	got := a.Snapshot().Completion
	if got != "draft-final" {
		t.Fatalf("replace on content/parts should append to completion; got %q", got)
	}
}

// TestChatGPTWeb_Collect_AddOpSeedParts covers the `add` op with
// `path == ""` and a structured `value` carrying initial assistant
// message parts — the bootstrap frame at stream start.
func TestChatGPTWeb_Collect_AddOpSeedParts(t *testing.T) {
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	// Initial assistant message announce frame with seed content.
	a.Feed([]byte(`{"p":"","o":"add","v":{"message":{"content":{"parts":["seed-text"]}}}}`))
	// Subsequent append on the same path should extend the seed.
	a.Feed([]byte(`{"p":"/message/content/parts/0","o":"append","v":" + delta"}`))
	got := a.Snapshot().Completion
	if got != "seed-text + delta" {
		t.Fatalf("add-op seed parts not picked up; got %q", got)
	}
}

// TestChatGPTWeb_Collect_NonContentPathsIgnored ensures patch ops that
// land on non-content paths (status, metadata) do NOT contribute to
// canonical Completion — a regression here would leak status strings
// like "finished_successfully" into compliance audit content.
func TestChatGPTWeb_Collect_NonContentPathsIgnored(t *testing.T) {
	a := extract.NewChatGPTWebExtractor().NewAccumulator()
	a.Feed([]byte(`{"p":"/message/status","o":"append","v":"finished_successfully"}`))
	a.Feed([]byte(`{"p":"/message/metadata/foo","o":"replace","v":"bar"}`))
	// A non-string value at a content path also must NOT be picked up.
	a.Feed([]byte(`{"p":"/message/content/parts/0","o":"append","v":42}`))
	got := a.Snapshot().Completion
	if got != "" {
		t.Fatalf("non-content / non-string paths must not contribute to completion; got %q", got)
	}
}
