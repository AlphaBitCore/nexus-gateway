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
