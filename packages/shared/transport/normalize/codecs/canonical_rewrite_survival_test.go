package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The gate that should have existed, stated the only way that cannot go stale:
// after a rewrite, the pre-redaction text must not survive ANYWHERE in the body.
//
// Every earlier assertion here named a channel — content, reasoning, refusal,
// tool arguments — and a named list can only fail for the names on it. The
// defect that got through was a SECOND COPY of the reasoning text in a field no
// codec decodes and no rewriter touches: `nexus_thinking`, the provider-private
// exact-replay carrier that holds Anthropic's thinking blocks with their
// signatures. The redaction landed on `reasoning_content`, the copy in the
// carrier stayed, and every consumer prefers the carrier — the request leg
// rebuilds native thinking blocks from it, and the Anthropic stream encoder
// explicitly discards its reasoning buffer when a carrier is present. So the
// masked copy was dropped and the original delivered, under an audit row that
// said redacted.
//
// Scanning the whole body needs no list and cannot forget a field. A future
// carrier fails this the day it is added.
const bodyWithReplayCarrier = `{"id":"resp-1","object":"chat.completion","model":"claude-opus-4",` +
	`"choices":[{"index":0,"message":{"role":"assistant",` +
	`"reasoning_content":"the user's ssn is 123-45-6789",` +
	`"nexus_thinking":[{"thinking":"the user's ssn is 123-45-6789","signature":"sig-abc"}],` +
	`"content":"I cannot help with that."},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

const requestBodyWithReplayCarrier = `{"model":"claude-opus-4","messages":[` +
	`{"role":"user","content":"hello"},` +
	`{"role":"assistant","reasoning_content":"the user's ssn is 123-45-6789",` +
	`"nexus_thinking":[{"thinking":"the user's ssn is 123-45-6789","signature":"sig-abc"}],` +
	`"content":"ok"}]}`

func maskEverySecret(p core.NormalizedPayload, secret string) core.NormalizedPayload {
	for mi := range p.Messages {
		for bi := range p.Messages[mi].Content {
			b := &p.Messages[mi].Content[bi]
			b.Text = strings.ReplaceAll(b.Text, secret, "[REDACTED]")
		}
	}
	return p
}

func TestRewrittenResponseBodyKeepsNoCopyOfTheRedactedText(t *testing.T) {
	const secret = "123-45-6789"

	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(bodyWithReplayCarrier), core.Meta{
		AdapterType: "openai", Direction: core.DirectionResponse,
		EndpointPath: "/v1/chat/completions", ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The control: the secret must be IN the payload the hooks see, or this test
	// asserts absence from a body that never carried it.
	found := false
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, secret) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the decode produced no block carrying the secret — the redaction below has " +
			"nothing to apply and the absence assertion would pass vacuously")
	}

	out, n, err := RewriteCanonicalResponseContent([]byte(bodyWithReplayCarrier), maskEverySecret(p, secret))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n == 0 {
		t.Fatal("the rewrite reported no writes for a body whose reasoning was masked")
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("the redacted text survives somewhere in the rewritten body — a copy the "+
			"rewrite does not reach is a copy the client receives:\n%s", out)
	}
	if !strings.Contains(string(out), "[REDACTED]") {
		t.Errorf("the mask is absent, so the body may simply have lost the channel:\n%s", out)
	}
	// The envelope must still be intact: dropping the carrier must not turn into
	// dropping the message.
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "I cannot help with that." {
		t.Errorf("visible content = %q — the carrier drop took more than the carrier", got)
	}
	if gjson.GetBytes(out, "usage.total_tokens").Int() != 2 {
		t.Errorf("usage was disturbed by a content redaction:\n%s", out)
	}
}

func TestRewrittenRequestBodyKeepsNoCopyOfTheRedactedText(t *testing.T) {
	const secret = "123-45-6789"

	p := decodeCanonicalRequest(t, requestBodyWithReplayCarrier)
	found := false
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, secret) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the decode produced no block carrying the secret — the assertion below would " +
			"pass vacuously")
	}

	out, n, err := RewriteCanonicalRequestContent([]byte(requestBodyWithReplayCarrier), maskEverySecret(p, secret))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n == 0 {
		t.Fatal("no writes for a request whose reasoning was masked")
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("the redacted text survives in the request that goes upstream — the replay "+
			"carrier is rebuilt into native thinking blocks, so this reaches the provider:\n%s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hello" {
		t.Errorf("an untouched earlier turn was disturbed: %q", got)
	}
}

// A body with a carrier that nothing redacted must keep it. The carrier is an
// optimisation for exact replay, and dropping it on every request would cost
// signature-preserving replay for no compliance gain.
func TestReplayCarrierSurvivesWhenNothingWasRedacted(t *testing.T) {
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(bodyWithReplayCarrier), core.Meta{
		AdapterType: "openai", Direction: core.DirectionResponse,
		EndpointPath: "/v1/chat/completions", ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, n, err := RewriteCanonicalResponseContent([]byte(bodyWithReplayCarrier), p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 0 {
		t.Errorf("reported %d writes for an unedited payload", n)
	}
	if !gjson.GetBytes(out, "choices.0.message.nexus_thinking.0.signature").Exists() {
		t.Errorf("the replay carrier was dropped from a response nothing redacted:\n%s", out)
	}
}
