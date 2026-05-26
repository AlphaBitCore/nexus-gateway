package minimax

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_Native(t *testing.T) {
	body := []byte(`{"model":"abab","prompt":"sys","messages":[` +
		`{"sender_type":"USER","text":"my email is a@b.com"},` +
		`{"sender_type":"USER","text":"ok"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"sys", "my email is [REDACTED]", "ok"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	if got := gjson.GetBytes(rewritten, "prompt").String(); got != "sys" {
		t.Errorf("prompt = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.text").String(); got != "my email is [REDACTED]" {
		t.Errorf("text[0] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "messages.1.text").String(); got != "ok" {
		t.Errorf("text[1] = %q", got)
	}
}

func TestRewriteRequestBody_Compat(t *testing.T) {
	body := []byte(`{"model":"abab","messages":[` +
		`{"role":"user","content":"my ssn 1234"},` +
		`{"role":"user","content":"ok"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"my ssn [REDACTED]", "ok"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "my ssn [REDACTED]" {
		t.Errorf("content[0] = %q", got)
	}
}

func TestRewriteRequestBody_RoundTrip(t *testing.T) {
	body := []byte(`{"model":"abab","prompt":"sys","messages":[{"sender_type":"USER","text":"hi"}]}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/text/chatcompletion_v2")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/text/chatcompletion_v2", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != len(nc.Segments) {
		t.Errorf("n = %d, want %d", n, len(nc.Segments))
	}
	nc2, err := a.ExtractRequest(context.Background(), rewritten, "/v1/text/chatcompletion_v2")
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
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`not json`), "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestRewriteRequestBody_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"model":"x"}`), "/v1/text/chatcompletion_v2",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}
