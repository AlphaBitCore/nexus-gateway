package proxy

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// TestRewriteRoundTrip_ChatCompletions verifies the positional contract
// between openai.Adapter.ExtractRequest, hookResult.ModifiedContent,
// contentBlocksToNormalized, and openai.Adapter.RewriteRequestBody.
// A stub "redactor" flips every extracted segment; the rewritten body
// must carry the flipped values at the right positions while the
// non-text parts (image_url, model) survive untouched. The handler's
// hook pipeline relies on this round-trip staying stable.
func TestRewriteRoundTrip_ChatCompletions(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"my email a@b.com"},` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"look here"},` +
		`{"type":"image_url","image_url":{"url":"https://x.com/i.png"}},` +
		`{"type":"text","text":"card 4242 4242 4242 4242"}]}]}`)

	a := &openai.Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got, want := len(nc.Segments), 4; got != want {
		t.Fatalf("extracted %d segments, want %d", got, want)
	}
	modified := make([]goHooks.ContentBlock, len(nc.Segments))
	for i, seg := range nc.Segments {
		modified[i] = goHooks.ContentBlock{Role: "user", Type: "text", Text: "[REDACTED_" + seg + "]"}
	}

	nc = contentBlocksToNormalized(modified)
	if got, want := len(nc.Segments), 4; got != want {
		t.Fatalf("normalized %d segments, want %d", got, want)
	}

	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 4 {
		t.Errorf("rewrite count = %d, want 4", n)
	}

	cases := []struct {
		path, want string
	}{
		{"messages.0.content", "[REDACTED_sys]"},
		{"messages.1.content", "[REDACTED_my email a@b.com]"},
		{"messages.2.content.0.text", "[REDACTED_look here]"},
		{"messages.2.content.1.image_url.url", "https://x.com/i.png"},
		{"messages.2.content.2.text", "[REDACTED_card 4242 4242 4242 4242]"},
		{"model", "gpt-4"},
	}
	for _, c := range cases {
		if got := gjson.GetBytes(rewritten, c.path).String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestContentBlocksToNormalized_SkipsNonText(t *testing.T) {
	blocks := []goHooks.ContentBlock{
		{Role: "user", Type: "text", Text: "a"},
		{Role: "user", Type: "image", Text: ""},
		{Role: "user", Type: "", Text: "b"}, // blank type treated as text
		{Role: "user", Type: "tool_call", Text: "ignored"},
	}
	nc := contentBlocksToNormalized(blocks)
	if got, want := len(nc.Segments), 2; got != want {
		t.Fatalf("segments = %d, want %d", got, want)
	}
	if nc.Segments[0] != "a" || nc.Segments[1] != "b" {
		t.Errorf("segments = %v", nc.Segments)
	}
}
