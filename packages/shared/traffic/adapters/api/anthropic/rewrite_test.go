package anthropic

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_StringContent(t *testing.T) {
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"my email is a@b.com"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"my email is [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "my email is [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}

func TestRewriteRequestBody_ArrayContent(t *testing.T) {
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"my SSN is X"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}},` +
		`{"type":"text","text":"ok"}]}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"my SSN is [REDACTED]", "ok"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content.0.text").String(); got != "my SSN is [REDACTED]" {
		t.Errorf("text[0] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content.1.source.data").String(); got != "abc" {
		t.Errorf("image data mutated: %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content.2.text").String(); got != "ok" {
		t.Errorf("text[2] = %q", got)
	}
}

func TestRewriteRequestBody_SystemString(t *testing.T) {
	body := []byte(`{"model":"claude-3","system":"act as a helper","messages":[{"role":"user","content":"hi a@b.com"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"act as a helper", "hi [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "system").String(); got != "act as a helper" {
		t.Errorf("system = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "hi [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}

func TestRewriteRequestBody_SystemArray(t *testing.T) {
	body := []byte(`{"model":"claude-3","system":[` +
		`{"type":"text","text":"sys prompt"},` +
		`{"type":"text","text":"ssn 123-45-6789"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"sys prompt", "ssn [REDACTED]", "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	if got := gjson.GetBytes(rewritten, "system.0.text").String(); got != "sys prompt" {
		t.Errorf("system[0] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "system.1.text").String(); got != "ssn [REDACTED]" {
		t.Errorf("system[1] = %q", got)
	}
}

func TestRewriteRequestBody_RoundTrip(t *testing.T) {
	body := []byte(`{"model":"claude-3","system":"x","messages":[{"role":"user","content":[{"type":"text","text":"y"}]}]}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != len(nc.Segments) {
		t.Errorf("n = %d, want %d", n, len(nc.Segments))
	}
	nc2, err := a.ExtractRequest(context.Background(), rewritten, "/v1/messages")
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	for i := range nc.Segments {
		if nc.Segments[i] != nc2.Segments[i] {
			t.Errorf("segment[%d] mismatch: %q vs %q", i, nc.Segments[i], nc2.Segments[i])
		}
	}
}

func TestRewriteRequestBody_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`not json`), "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestRewriteRequestBody_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"model":"claude-3"}`), "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

// TestRewriteRequestBody_SegmentsExhausted pins the partial-rewrite
// contract: when the redaction pipeline supplies fewer segments than the
// body has text slots, Rewrite writes what it has, leaves the remaining
// slots untouched, and reports the true written count — it must never
// error or clobber unvisited slots.
func TestRewriteRequestBody_SegmentsExhausted(t *testing.T) {
	a := &Adapter{}

	t.Run("system_string_zero_segments", func(t *testing.T) {
		body := []byte(`{"system":"keep me","messages":[{"role":"user","content":"keep too"}]}`)
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{})
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("n=%d want 0", n)
		}
		if got := gjson.GetBytes(out, "system").String(); got != "keep me" {
			t.Errorf("system clobbered: %q", got)
		}
		if got := gjson.GetBytes(out, "messages.0.content").String(); got != "keep too" {
			t.Errorf("content clobbered: %q", got)
		}
	})

	t.Run("system_array_nontext_skipped_then_exhausted", func(t *testing.T) {
		body := []byte(`{"system":[` +
			`{"type":"text","text":"sys A"},` +
			`{"type":"cache_control_marker"},` +
			`{"type":"text","text":"sys B"}],` +
			`"messages":[{"role":"user","content":"hi"}]}`)
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"clean A"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "system.0.text").String(); got != "clean A" {
			t.Errorf("system[0]=%q", got)
		}
		if got := gjson.GetBytes(out, "system.2.text").String(); got != "sys B" {
			t.Errorf("system[2] clobbered after exhaustion: %q", got)
		}
	})

	t.Run("string_content_exhausted_on_second_message", func(t *testing.T) {
		body := []byte(`{"messages":[` +
			`{"role":"user","content":"first"},` +
			`{"role":"assistant","content":"second"}]}`)
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"FIRST"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "messages.0.content").String(); got != "FIRST" {
			t.Errorf("messages[0]=%q", got)
		}
		if got := gjson.GetBytes(out, "messages.1.content").String(); got != "second" {
			t.Errorf("messages[1] clobbered: %q", got)
		}
	})

	t.Run("text_block_exhausted_mid_array", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"blk A"},` +
			`{"type":"text","text":"blk B"}]}]}`)
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"CLEAN A"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "CLEAN A" {
			t.Errorf("block0=%q", got)
		}
		if got := gjson.GetBytes(out, "messages.0.content.1.text").String(); got != "blk B" {
			t.Errorf("block1 clobbered: %q", got)
		}
	})

	t.Run("tool_result_string_zero_segments", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"t","content":"tool output"}]}]}`)
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{})
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("n=%d want 0", n)
		}
		if got := gjson.GetBytes(out, "messages.0.content.0.content").String(); got != "tool output" {
			t.Errorf("tool_result clobbered: %q", got)
		}
	})

	t.Run("tool_result_array_sub_exhausted", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"t","content":[` +
			`{"type":"text","text":"sub A"},` +
			`{"type":"text","text":"sub B"}]}]}]}`)
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"SUB A"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "messages.0.content.0.content.0.text").String(); got != "SUB A" {
			t.Errorf("sub0=%q", got)
		}
		if got := gjson.GetBytes(out, "messages.0.content.0.content.1.text").String(); got != "sub B" {
			t.Errorf("sub1 clobbered: %q", got)
		}
	})
}

// TestRewriteRequestBody_MessageWithoutContent pins that a message
// lacking a `content` field is skipped without consuming a segment —
// segment alignment with ExtractRequest (which also skips it) must hold
// or redactions land on the wrong message.
func TestRewriteRequestBody_MessageWithoutContent(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user"},` +
		`{"role":"user","content":"target"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"REDACTED"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "messages.1.content").String(); got != "REDACTED" {
		t.Errorf("messages[1]=%q want segment applied to the content-bearing message", got)
	}
	if gjson.GetBytes(out, "messages.0.content").Exists() {
		t.Errorf("messages[0] gained a content field it never had")
	}
}

// RewriteResponseBody — reverses ExtractResponse for non-streaming
// Messages responses: only top-level content[] text blocks are
// writable; thinking and tool_use blocks stay untouched.

func TestRewriteResponseBody_TextBlocks(t *testing.T) {
	body := []byte(`{"id":"msg_1","content":[` +
		`{"type":"text","text":"my email is a@b.com"},` +
		`{"type":"tool_use","id":"toolu_1","name":"f","input":{"k":"v"}},` +
		`{"type":"text","text":"second"}],` +
		`"stop_reason":"end_turn"}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"my email is [REDACTED]", "SECOND"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "content.0.text").String(); got != "my email is [REDACTED]" {
		t.Errorf("text[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "f" {
		t.Errorf("tool_use block mutated: name=%q", got)
	}
	if got := gjson.GetBytes(out, "content.1.input.k").String(); got != "v" {
		t.Errorf("tool_use input mutated: %q", got)
	}
	if got := gjson.GetBytes(out, "content.2.text").String(); got != "SECOND" {
		t.Errorf("text[2]=%q", got)
	}
	if got := gjson.GetBytes(out, "stop_reason").String(); got != "end_turn" {
		t.Errorf("stop_reason mutated: %q", got)
	}
}

func TestRewriteResponseBody_SegmentsExhausted(t *testing.T) {
	body := []byte(`{"content":[` +
		`{"type":"text","text":"first"},` +
		`{"type":"text","text":"second"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"FIRST"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "content.0.text").String(); got != "FIRST" {
		t.Errorf("text[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "content.1.text").String(); got != "second" {
		t.Errorf("text[1] clobbered after exhaustion: %q", got)
	}
}

func TestRewriteResponseBody_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`not json`), "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestRewriteResponseBody_UnknownSchema(t *testing.T) {
	a := &Adapter{}
	t.Run("content_absent", func(t *testing.T) {
		_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"id":"msg_1"}`), "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"x"}})
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			t.Errorf("expected ErrUnknownSchema, got %v", err)
		}
	})
	t.Run("content_not_array", func(t *testing.T) {
		_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"content":"plain string"}`), "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"x"}})
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			t.Errorf("expected ErrUnknownSchema, got %v", err)
		}
	})
}

// TestRewriteResponseBody_RoundTrip pins extract→rewrite symmetry on the
// response side: segments extracted by ExtractResponse written back via
// RewriteResponseBody re-extract identically.
func TestRewriteResponseBody_RoundTrip(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"alpha"},{"type":"thinking","thinking":"trace"},{"type":"text","text":"beta"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rewritten, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != len(nc.Segments) {
		t.Errorf("n=%d want %d", n, len(nc.Segments))
	}
	nc2, err := a.ExtractResponse(context.Background(), rewritten, "/v1/messages")
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	for i := range nc.Segments {
		if nc.Segments[i] != nc2.Segments[i] {
			t.Errorf("segment[%d] mismatch: %q vs %q", i, nc.Segments[i], nc2.Segments[i])
		}
	}
	// thinking block must survive untouched (Rewrite targets text only).
	if got := gjson.GetBytes(rewritten, "content.1.thinking").String(); got != "trace" {
		t.Errorf("thinking block mutated: %q", got)
	}
}

// TestRewriteRequestBody_ToolResult covers the audit gap: tool_result
// content blocks emitted on Segments by ExtractRequest must be writable
// back through Rewrite so PII redaction round-trips through the schema.
func TestRewriteRequestBody_ToolResult(t *testing.T) {
	t.Run("string_content", func(t *testing.T) {
		body := []byte(`{
			"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t","content":"raw SSN 123-45-6789"}
				]}
			]
		}`)
		a := &Adapter{}
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"raw SSN [REDACTED]"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		got := gjson.GetBytes(out, "messages.0.content.0.content").String()
		if got != "raw SSN [REDACTED]" {
			t.Errorf("rewritten content=%q", got)
		}
	})
	t.Run("array_content_text_subparts", func(t *testing.T) {
		body := []byte(`{
			"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t","content":[
						{"type":"text","text":"raw A"},
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}},
						{"type":"text","text":"raw B"}
					]}
				]}
			]
		}`)
		a := &Adapter{}
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
			traffic.NormalizedContent{Segments: []string{"clean A", "clean B"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("n=%d want 2", n)
		}
		if got := gjson.GetBytes(out, "messages.0.content.0.content.0.text").String(); got != "clean A" {
			t.Errorf("subpart0=%q", got)
		}
		if got := gjson.GetBytes(out, "messages.0.content.0.content.2.text").String(); got != "clean B" {
			t.Errorf("subpart2=%q", got)
		}
		// image (subpart 1) untouched
		if got := gjson.GetBytes(out, "messages.0.content.0.content.1.source.data").String(); got != "x" {
			t.Errorf("image data clobbered")
		}
	})
}
