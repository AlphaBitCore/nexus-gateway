package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_DelegatesToOpenAI(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"my email a@b.com"}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body,
		"/openai/deployments/gpt4/chat/completions?api-version=2024-02-01",
		traffic.NormalizedContent{Segments: []string{"my email [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "messages.0.content").String(); got != "my email [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}

func TestRewriteRequestBody_EmbeddingsUnsupported(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"x"}`)

	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), body,
		"/openai/deployments/emb/embeddings?api-version=2024-02-01",
		traffic.NormalizedContent{Segments: []string{"y"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}

// TestRewriteResponseBody_DelegatesToOpenAI pins response-side delegation:
// after Azure path remap the openai-compat rewriter must replace
// choices[0].message.content with the redacted segment. This is the
// outbound DLP path that surfaces redactions back to the caller.
func TestRewriteResponseBody_DelegatesToOpenAI(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-z",
		"choices": [
			{"index": 0, "message": {"role": "assistant", "content": "secret was 4111-1111-1111-1111"}, "finish_reason": "stop"}
		]
	}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteResponseBody(context.Background(), body,
		"/openai/deployments/gpt4/chat/completions?api-version=2024-02-01",
		traffic.NormalizedContent{Segments: []string{"secret was [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if got := gjson.GetBytes(rewritten, "choices.0.message.content").String(); got != "secret was [REDACTED]" {
		t.Errorf("content = %q, want %q", got, "secret was [REDACTED]")
	}
}

// TestRewriteResponseBody_EmbeddingsUnsupported pins that the Azure
// embeddings endpoint inherits the openai-compat ErrRewriteUnsupported
// for response rewrite — embeddings responses carry vectors, not text,
// so there's nothing for DLP to rewrite.
func TestRewriteResponseBody_EmbeddingsUnsupported(t *testing.T) {
	body := []byte(`{"data":[{"object":"embedding","embedding":[0.1,0.2,0.3]}]}`)

	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), body,
		"/openai/deployments/emb/embeddings?api-version=2024-02-01",
		traffic.NormalizedContent{Segments: []string{"unused"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("expected ErrRewriteUnsupported, got %v", err)
	}
}
