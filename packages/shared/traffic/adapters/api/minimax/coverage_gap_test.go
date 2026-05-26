package minimax

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// ExtractRequest — empty messages array branch (the gjson.Array() len==0
// short-circuit returning early without iterating).

func TestExtractRequest_EmptyMessagesArray(t *testing.T) {
	body := []byte(`{"model":"abab","messages":[]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/text/chatcompletion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
	if nc.Metadata["model"] != "abab" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// DetectResponseUsage — empty / malformed / missing usage / partial fields.

func TestDetectResponseUsage_EmptyBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

func TestDetectResponseUsage_Malformed(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{bad`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

func TestDetectResponseUsage_MissingUsage(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"choices":[]}`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

func TestDetectResponseUsage_OnlyPromptTokens(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"usage":{"prompt_tokens":5}}`))
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("Status=%q", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 5 {
		t.Errorf("PromptTokens=%v", um.PromptTokens)
	}
	if um.CompletionTokens != nil {
		t.Errorf("CompletionTokens should be nil, got %v", *um.CompletionTokens)
	}
}

// RewriteRequestBody — sjson error paths + segment-exhausted early returns
// + non-string field skip.

func TestRewriteRequestBody_PromptSegmentsExhausted(t *testing.T) {
	// prompt is set on the body but caller passes an empty Segments
	// slice → segIdx>=len(Segments) on the prompt step → early return
	// with written=0.
	body := []byte(`{"prompt":"sys","messages":[{"sender_type":"USER","text":"hi"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: nil})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if gjson.GetBytes(out, "prompt").String() != "sys" {
		t.Errorf("prompt should be untouched: %q", gjson.GetBytes(out, "prompt").String())
	}
}

func TestRewriteRequestBody_MessageSegmentsExhausted(t *testing.T) {
	// 1 segment provided, body has prompt + 2 messages — adapter writes
	// prompt and the first message text, then exits cleanly on segIdx
	// underflow.
	body := []byte(`{"prompt":"sys","messages":[` +
		`{"sender_type":"USER","text":"first"},` +
		`{"sender_type":"USER","text":"second"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"NEW SYS", "NEW FIRST"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if gjson.GetBytes(out, "prompt").String() != "NEW SYS" {
		t.Errorf("prompt=%q", gjson.GetBytes(out, "prompt").String())
	}
	if gjson.GetBytes(out, "messages.0.text").String() != "NEW FIRST" {
		t.Errorf("messages.0.text=%q", gjson.GetBytes(out, "messages.0.text").String())
	}
	if gjson.GetBytes(out, "messages.1.text").String() != "second" {
		t.Errorf("messages.1.text should be untouched: %q", gjson.GetBytes(out, "messages.1.text").String())
	}
}

func TestRewriteRequestBody_NoMessagesAfterPrompt(t *testing.T) {
	// Prompt rewrite step writes ok, then messages array is empty → the
	// "len(msgList)==0" short-circuit fires.
	body := []byte(`{"prompt":"sys","messages":[]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"NEW SYS"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if gjson.GetBytes(out, "prompt").String() != "NEW SYS" {
		t.Errorf("prompt=%q", gjson.GetBytes(out, "prompt").String())
	}
}

func TestRewriteRequestBody_NativeSkipNonStringText(t *testing.T) {
	// First message has a STRING text (so native is detected) but the
	// second has text as an object — adapter must skip the second cleanly.
	body := []byte(`{"messages":[` +
		`{"sender_type":"USER","text":"ok"},` +
		`{"sender_type":"USER","text":{"nested":true}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"NEW"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if gjson.GetBytes(out, "messages.0.text").String() != "NEW" {
		t.Errorf("messages.0.text=%q", gjson.GetBytes(out, "messages.0.text").String())
	}
}

func TestRewriteRequestBody_CompatSkipMissingContent(t *testing.T) {
	// First message has STRING content → compat detected. Second message
	// omits content entirely → adapter must skip it without erroring.
	body := []byte(`{"messages":[` +
		`{"role":"user","content":"ok"},` +
		`{"role":"user"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"NEW"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "NEW" {
		t.Errorf("messages.0.content=%q", gjson.GetBytes(out, "messages.0.content").String())
	}
}

// RewriteResponseBody — full coverage (malformed / missing choices / native
// (.message.text) / compat (.message.content) / segment-exhausted / skip
// non-string).

func TestRewriteResponseBody_Malformed(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{bad`), "/x",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestRewriteResponseBody_MissingChoices(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"reply":"x"}`), "/x",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestRewriteResponseBody_NativeFormat(t *testing.T) {
	body := []byte(`{"choices":[` +
		`{"message":{"sender_type":"BOT","text":"first"}},` +
		`{"message":{"sender_type":"BOT","text":"second"}}` +
		`]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/x",
		traffic.NormalizedContent{Segments: []string{"NEW FIRST", "NEW SECOND"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if gjson.GetBytes(out, "choices.0.message.text").String() != "NEW FIRST" {
		t.Errorf("choices.0.text=%q", gjson.GetBytes(out, "choices.0.message.text").String())
	}
	if gjson.GetBytes(out, "choices.1.message.text").String() != "NEW SECOND" {
		t.Errorf("choices.1.text=%q", gjson.GetBytes(out, "choices.1.message.text").String())
	}
}

func TestRewriteResponseBody_CompatFormat(t *testing.T) {
	body := []byte(`{"choices":[` +
		`{"message":{"role":"assistant","content":"hello"}}` +
		`]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/x",
		traffic.NormalizedContent{Segments: []string{"REDACTED"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if gjson.GetBytes(out, "choices.0.message.content").String() != "REDACTED" {
		t.Errorf("content=%q", gjson.GetBytes(out, "choices.0.message.content").String())
	}
}

func TestRewriteResponseBody_NativeSegmentsExhausted(t *testing.T) {
	body := []byte(`{"choices":[` +
		`{"message":{"text":"first"}},` +
		`{"message":{"text":"second"}}` +
		`]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/x",
		traffic.NormalizedContent{Segments: []string{"X"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if gjson.GetBytes(out, "choices.0.message.text").String() != "X" {
		t.Errorf("choices.0.text=%q", gjson.GetBytes(out, "choices.0.message.text").String())
	}
	if gjson.GetBytes(out, "choices.1.message.text").String() != "second" {
		t.Errorf("choices.1.text should be untouched: %q", gjson.GetBytes(out, "choices.1.message.text").String())
	}
}

func TestRewriteResponseBody_CompatSegmentsExhausted(t *testing.T) {
	body := []byte(`{"choices":[` +
		`{"message":{"content":"first"}},` +
		`{"message":{"content":"second"}}` +
		`]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/x",
		traffic.NormalizedContent{Segments: []string{"X"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if gjson.GetBytes(out, "choices.0.message.content").String() != "X" {
		t.Errorf("choices.0.content=%q", gjson.GetBytes(out, "choices.0.message.content").String())
	}
}

func TestRewriteResponseBody_NoMatchingFields(t *testing.T) {
	// choices without message.text or message.content — adapter walks
	// the loop without doing anything; returns written=0.
	body := []byte(`{"choices":[{"message":{"role":"assistant"}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/x",
		traffic.NormalizedContent{Segments: []string{"X"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body modified")
	}
}

// Normalize — Tier 1 contract delegating to extract.NormalizeForAdapter.

func TestNormalize_OpenAIChatRequest(t *testing.T) {
	body := []byte(`{"model":"MiniMax-M2.7","messages":[
		{"role":"user","content":"hello minimax"}
	]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "minimax",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("Kind=%v want KindAIChat", payload.Kind)
	}
	if payload.DetectedSpec != "minimax" {
		t.Errorf("DetectedSpec=%q want minimax", payload.DetectedSpec)
	}
	if payload.Model != "MiniMax-M2.7" {
		t.Errorf("Model=%q", payload.Model)
	}
}

func TestNormalize_BelowConfidence(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "minimax",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for low-confidence body")
	}
}
