package vertex

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_Anthropic(t *testing.T) {
	body := []byte(`{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"email a@b.com"}]}`)
	path := "/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-3-5-sonnet:rawPredict"

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, path,
		traffic.NormalizedContent{Segments: []string{"email [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "email [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}

func TestRewriteRequestBody_Gemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"ssn 123"}]}]}`)
	path := "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent"

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, path,
		traffic.NormalizedContent{Segments: []string{"ssn [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "contents.0.parts.0.text").String(); got != "ssn [REDACTED]" {
		t.Errorf("text = %q", got)
	}
}

func TestRewriteRequestBody_UnknownPublisherUnsupported(t *testing.T) {
	body := []byte(`{"x":"y"}`)
	path := "/v1/projects/p/locations/us/publishers/mistralai/models/m:predict"

	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), body, path,
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}
