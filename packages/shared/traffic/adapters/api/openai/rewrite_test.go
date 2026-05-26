package openai

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_ChatCompletions_StringContent(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"my email is a@b.com"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"my email is [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("rewrite count = %d, want 1", n)
	}
	got := gjson.GetBytes(rewritten, "messages.0.content").String()
	if got != "my email is [REDACTED]" {
		t.Errorf("content = %q", got)
	}
	if gjson.GetBytes(rewritten, "model").String() != "gpt-4" {
		t.Errorf("model field was mutated")
	}
}

func TestRewriteRequestBody_ChatCompletions_ArrayContent(t *testing.T) {
	body := []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"look at this"},` +
		`{"type":"image_url","image_url":{"url":"https://ex.com/i.png"}},` +
		`{"type":"text","text":"my SSN is 123-45-6789"}]}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"look at this", "my SSN is [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("rewrite count = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content.0.text").String(); got != "look at this" {
		t.Errorf("text[0] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content.2.text").String(); got != "my SSN is [REDACTED]" {
		t.Errorf("text[2] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content.1.image_url.url").String(); got != "https://ex.com/i.png" {
		t.Errorf("image_url mutated: %q", got)
	}
}

func TestRewriteRequestBody_ChatCompletions_MultiMessage(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"system","content":"You are helpful."},` +
		`{"role":"user","content":"my card is 4242 4242 4242 4242"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"You are helpful.", "my card is [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "You are helpful." {
		t.Errorf("system = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.1.content").String(); got != "my card is [REDACTED]" {
		t.Errorf("user = %q", got)
	}
}

func TestRewriteRequestBody_ChatCompletions_RoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"system","content":"x"},{"role":"user","content":"y"}]}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Rewrite with identical segments → semantic equivalence.
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != len(nc.Segments) {
		t.Errorf("n = %d, expected %d", n, len(nc.Segments))
	}
	// Re-extract and confirm segments survive.
	nc2, err := a.ExtractRequest(context.Background(), rewritten, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	if len(nc2.Segments) != len(nc.Segments) {
		t.Fatalf("roundtrip segments %d vs %d", len(nc2.Segments), len(nc.Segments))
	}
	for i := range nc.Segments {
		if nc.Segments[i] != nc2.Segments[i] {
			t.Errorf("segment[%d] %q → %q", i, nc.Segments[i], nc2.Segments[i])
		}
	}
}

func TestRewriteRequestBody_ChatCompletions_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`not json`), "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestRewriteRequestBody_ChatCompletions_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"model":"gpt-4"}`), "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestRewriteRequestBody_ResponsesCreate_String(t *testing.T) {
	body := []byte(`{"model":"gpt-4","input":"my email is a@b.com"}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"my email is [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "input").String(); got != "my email is [REDACTED]" {
		t.Errorf("input = %q", got)
	}
}

func TestRewriteRequestBody_ResponsesCreate_Array(t *testing.T) {
	body := []byte(`{"model":"gpt-4","input":[` +
		`{"content":"hello"},` +
		`{"content":[{"type":"input_text","text":"my card is X"},{"type":"text","text":"ok"}]}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"hello", "my card is [REDACTED]", "ok"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	if got := gjson.GetBytes(rewritten, "input.0.content").String(); got != "hello" {
		t.Errorf("input[0].content = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "input.1.content.0.text").String(); got != "my card is [REDACTED]" {
		t.Errorf("input[1].content[0].text = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "input.1.content.1.text").String(); got != "ok" {
		t.Errorf("input[1].content[1].text = %q", got)
	}
}

func TestRewriteRequestBody_Embeddings_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"model":"text-embedding-3-small","input":"hello"}`)
	_, _, err := a.RewriteRequestBody(context.Background(), body, "/v1/embeddings",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}

func TestRewriteRequestBody_UnknownPath_Unsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{}`), "/v1/files",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}

func TestRewriteRequestBody_FewerSegments_Stops(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"user","content":"second"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"first-redacted"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "first-redacted" {
		t.Errorf("msg[0] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.1.content").String(); got != "second" {
		t.Errorf("msg[1] = %q (should survive)", got)
	}
}

func TestRewriteResponseBody_ChatCompletions(t *testing.T) {
	body := []byte(`{"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello secret"}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"hello [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}

// rewriteChatRequest must skip messages that have no `content` field
// (e.g. an assistant turn whose only payload is tool_calls). Without the
// skip the iterator would consume a segment for the missing slot.
func TestRewriteRequestBody_ChatCompletions_SkipsContentlessMessages(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"user","content":"second"}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"FIRST", "SECOND"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2 (assistant tool-only skipped)", n)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "FIRST" {
		t.Errorf("[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "messages.2.content").String(); got != "SECOND" {
		t.Errorf("[2]=%q", got)
	}
}

// rewriteChatRequest array-content path with mid-array short-circuit when
// segments run out — preserves un-rewritten text parts.
func TestRewriteRequestBody_ChatCompletions_ArrayMidShortCircuit(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"a"},
			{"type":"image_url","image_url":{"url":"u"}},
			{"type":"text","text":"b"},
			{"type":"text","text":"c"}
		]}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"A", "B"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2 (stops at 3rd text slot)", n)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "A" {
		t.Errorf("[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.text").String(); got != "B" {
		t.Errorf("[2]=%q", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.3.text").String(); got != "c" {
		t.Errorf("[3] should survive: %q", got)
	}
}

// rewriteResponsesCreate malformed JSON → ErrMalformed.
func TestRewriteRequestBody_ResponsesCreate_Malformed(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{bad`), "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// rewriteResponsesCreate missing `input` → ErrUnknownSchema.
func TestRewriteRequestBody_ResponsesCreate_MissingInput(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"model":"gpt-4"}`), "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// rewriteResponsesCreate array-content mid-short-circuit on inner parts.
func TestRewriteRequestBody_ResponsesCreate_ArrayPartsMidShortCircuit(t *testing.T) {
	body := []byte(`{"input":[
		{"content":[
			{"type":"input_text","text":"x"},
			{"type":"image","image":"u"},
			{"type":"text","text":"y"}
		]}
	]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"X"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "X" {
		t.Errorf("[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.2.text").String(); got != "y" {
		t.Errorf("[2] should survive: %q", got)
	}
}

// rewriteChatResponseBody short-circuit when only refusal slot present
// but segments runs out at content slot. Distinct from the refusal_only
// test because here the content slot is also present and consumes a
// segment that doesn't exist.
func TestRewriteResponseBody_ChatCompletions_RefusalShortCircuit(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"a","refusal":"b"}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"A"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "A" {
		t.Errorf("content=%q", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.refusal").String(); got != "b" {
		t.Errorf("refusal should survive: %q", got)
	}
}

// rewriteChatResponseBody short-circuit BEFORE the content slot — segments
// empty entirely.
func TestRewriteResponseBody_ChatCompletions_NoSegments_NoRewrite(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"a"}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: nil})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "a" {
		t.Errorf("content mutated: %q", got)
	}
}

// RewriteResponseBody dispatch — /embeddings is unsupported, unknown paths
// fall through to ErrRewriteUnsupported, malformed JSON surfaces ErrMalformed,
// missing choices surfaces ErrUnknownSchema. These dispatch arms repeatedly
// regress when adding new endpoints, so they are pinned here.

func TestRewriteResponseBody_Embeddings_Unsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(),
		[]byte(`{"data":[{"embedding":[0.1]}]}`), "/v1/embeddings",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

func TestRewriteResponseBody_UnknownPath_Unsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{}`), "/v1/files",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

func TestRewriteResponseBody_ChatCompletions_Malformed(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{bad`), "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestRewriteResponseBody_ChatCompletions_MissingChoices(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"model":"x"}`), "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// rewriteResponsesResponseBody — /v1/responses output_text rewrite.

func TestRewriteResponseBody_ResponsesAPI(t *testing.T) {
	body := []byte(`{
		"id":"resp_1","model":"gpt-4o","status":"completed",
		"output":[
			{"type":"message","content":[
				{"type":"output_text","text":"hello secret"},
				{"type":"output_text","text":"more secret"}
			]}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"hello [REDACTED]", "more [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "output.0.content.0.text").String(); got != "hello [REDACTED]" {
		t.Errorf("output[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "output.0.content.1.text").String(); got != "more [REDACTED]" {
		t.Errorf("output[1]=%q", got)
	}
}

func TestRewriteResponseBody_ResponsesAPI_Malformed(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{bad`), "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestRewriteResponseBody_ResponsesAPI_MissingOutput(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"id":"x"}`), "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Non-array output (e.g. error envelope before stream begin) must surface
// ErrUnknownSchema cleanly rather than silently dropping the body.
func TestRewriteResponseBody_ResponsesAPI_OutputNotArray(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"output":"oops"}`), "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Function-call output items have no `content` array; the rewriter must
// skip them without consuming a segment so subsequent text items still
// align with their indices.
func TestRewriteResponseBody_ResponsesAPI_SkipsFunctionCall(t *testing.T) {
	body := []byte(`{
		"output":[
			{"type":"function_call","id":"fc","name":"f","arguments":"{}"},
			{"type":"message","content":[{"type":"output_text","text":"after fc"}]}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"clean"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1 (function_call skipped)", n)
	}
	if got := gjson.GetBytes(out, "output.1.content.0.text").String(); got != "clean" {
		t.Errorf("output[1].content[0].text=%q", got)
	}
}

// Output items where a content part is non-output_text (e.g. annotations)
// must be skipped without consuming a segment.
func TestRewriteResponseBody_ResponsesAPI_SkipsNonOutputTextParts(t *testing.T) {
	body := []byte(`{
		"output":[
			{"type":"message","content":[
				{"type":"refusal","refusal":"nope"},
				{"type":"output_text","text":"answer"}
			]}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"redacted"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1 (refusal part skipped)", n)
	}
	if got := gjson.GetBytes(out, "output.0.content.1.text").String(); got != "redacted" {
		t.Errorf("text=%q", got)
	}
}

// Running out of segments mid-walk must stop rewriting and preserve the
// already-written count so the caller can detect partial rewrite.
func TestRewriteResponseBody_ResponsesAPI_FewerSegments_Stops(t *testing.T) {
	body := []byte(`{
		"output":[{"type":"message","content":[
			{"type":"output_text","text":"first"},
			{"type":"output_text","text":"second"}
		]}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"FIRST"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "output.0.content.0.text").String(); got != "FIRST" {
		t.Errorf("output[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "output.0.content.1.text").String(); got != "second" {
		t.Errorf("output[1] should survive: %q", got)
	}
}

// rewriteResponsesCreate edge paths.

// String-input rewrite stops cleanly when segments are empty (segIdx==0
// path; the caller doesn't request any rewrite). Distinct from the
// "fewer segments" array path because the string-input branch returns
// (out, 0, nil) without iterating.
func TestRewriteRequestBody_ResponsesCreate_StringNoSegments(t *testing.T) {
	body := []byte(`{"input":"orig"}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: nil})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if got := gjson.GetBytes(out, "input").String(); got != "orig" {
		t.Errorf("input mutated: %q", got)
	}
}

// Array-input rewrite where a later item runs out of segments must stop
// preserving earlier writes and remaining items intact.
func TestRewriteRequestBody_ResponsesCreate_ArrayFewerSegments_Stops(t *testing.T) {
	body := []byte(`{"input":[
		{"content":"a"},
		{"content":[{"type":"input_text","text":"b"},{"type":"text","text":"c"}]},
		{"content":"d"}
	]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/responses",
		traffic.NormalizedContent{Segments: []string{"A", "B"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "input.0.content").String(); got != "A" {
		t.Errorf("input[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "input.1.content.0.text").String(); got != "B" {
		t.Errorf("input[1].content[0].text=%q", got)
	}
	// c and d preserved.
	if got := gjson.GetBytes(out, "input.1.content.1.text").String(); got != "c" {
		t.Errorf("input[1].content[1] should survive: %q", got)
	}
	if got := gjson.GetBytes(out, "input.2.content").String(); got != "d" {
		t.Errorf("input[2] should survive: %q", got)
	}
}

// TestRewriteResponseBody_ChatCompletions_RefusalSlot pins the rewrite
// counterpart of extractChatResponse: when only refusal is present (the
// content slot is null and skipped), the single refusal slot consumes
// segment 0. When both are present, content+refusal both rewrite in
// order.
func TestRewriteResponseBody_ChatCompletions_RefusalSlot(t *testing.T) {
	t.Run("refusal_only", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"I cannot help."}}]}`)
		a := &Adapter{}
		out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
			traffic.NormalizedContent{Segments: []string{"[refusal redacted]"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "choices.0.message.refusal").String(); got != "[refusal redacted]" {
			t.Errorf("refusal=%q", got)
		}
	})
	t.Run("content_then_refusal", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x","refusal":"y"}}]}`)
		a := &Adapter{}
		out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
			traffic.NormalizedContent{Segments: []string{"X", "Y"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("n=%d want 2", n)
		}
		if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "X" {
			t.Errorf("content=%q", got)
		}
		if got := gjson.GetBytes(out, "choices.0.message.refusal").String(); got != "Y" {
			t.Errorf("refusal=%q", got)
		}
	})
}
