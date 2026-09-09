// A refusal is text the model sends the caller instead of an answer, and it is
// the only text in the turn when it fires: `content` is null. It therefore has
// to exist at the canonical waist for the same reason `content` does — every
// consumer that reads the canonical payload (compliance scanning, audit
// transcripts, the traffic record) otherwise reads the turn as empty.
package codecs

import (
	"context"
	"strings"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// refusalOnlyResponse is the shape OpenAI returns when a structured-output or
// safety refusal fires: `content` is explicitly null and the whole reply lives
// on `refusal`.
const refusalOnlyResponse = `{
  "id": "chatcmpl-refusal",
  "object": "chat.completion",
  "model": "gpt-4o",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": null,
      "refusal": "I can't help with that, and I won't repeat the account number you gave."
    },
    "finish_reason": "stop"
  }]
}`

func TestRefusalIsCarriedAtTheCanonicalWaist(t *testing.T) {
	payload, err := SharedOpenAIChat().Normalize(context.Background(), []byte(refusalOnlyResponse), core.Meta{
		AdapterType:  "openai",
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	projected := strings.Join(payload.TextProjection(), "\n")
	if !strings.Contains(projected, "I can't help with that") {
		t.Errorf("the refusal is the entire assistant turn on the wire but is absent from "+
			"the canonical payload.\nprojection: %q\n"+
			"Everything downstream of the waist — compliance scanning included — sees an "+
			"empty turn where the client sees a paragraph.", projected)
	}
}

// TestRefusalAndContentKeepTheirOrder pins the slot order a wire rewriter has
// to agree with. OpenAI never populates both today, but the canonical order is
// the contract the rewrite side indexes against, so it is asserted rather than
// left to whichever the decoder happens to append first.
func TestRefusalAndContentKeepTheirOrder(t *testing.T) {
	const both = `{"choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"visible answer","refusal":"refused part"},"finish_reason":"stop"}]}`

	payload, err := SharedOpenAIChat().Normalize(context.Background(), []byte(both), core.Meta{
		AdapterType:  "openai",
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	got := payload.TextProjection()
	want := []string{"visible answer", "refused part"}
	if len(got) != len(want) {
		t.Fatalf("projection had %d entries, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("projection[%d] = %q, want %q — the canonical slot order is what the "+
				"wire rewriter indexes against, so a swap here silently writes one channel's "+
				"redaction onto the other", i, got[i], want[i])
		}
	}
}
