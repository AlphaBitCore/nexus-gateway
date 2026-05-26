package bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_DelegatesToAnthropic(t *testing.T) {
	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[{"role":"user","content":"email a@b.com"}]}`)
	path := "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke"

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

func TestRewriteRequestBody_NonAnthropicUnsupported(t *testing.T) {
	body := []byte(`{"inputText":"hi"}`)
	path := "/model/amazon.titan-text-express-v1/invoke"

	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), body, path,
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}
