// Gate for the binding rule "hooks see the canonical spec only": whatever the
// gateway hands a compliance hook must carry every text channel the canonical
// payload carries, because the response the client receives carries them too.
//
// The channel that motivated this gate is reasoning. It reaches the client on
// both legs, the owner ruled that it is scanned by default, and the response
// stage nonetheless could not see it — the body is canonicalized and then, on
// the next line, flattened back through the traffic adapter, whose output model
// has no reasoning slot at all.
package proxy

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// canonicalResponseWithReasoning is the canonical (OpenAI chat-completions)
// shape a reasoning model produces: visible answer plus the chain of thought
// that produced it, both delivered to the caller.
const canonicalResponseWithReasoning = `{
  "id": "chatcmpl-gate",
  "object": "chat.completion",
  "model": "deepseek-reasoner",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Your balance is available in the app.",
      "reasoning_content": "The customer's card is 4111 1111 1111 1111, so I should not repeat it."
    },
    "finish_reason": "stop"
  }]
}`

// TestHookInputCarriesEveryDeliveredTextChannel fails while a channel that is
// delivered to the client cannot be read by a hook.
//
// It asserts on the projection — the exact surface every scanning hook reads —
// rather than on the block list, so it stays true regardless of which block
// type the reasoning text ends up occupying.
func TestHookInputCarriesEveryDeliveredTextChannel(t *testing.T) {
	h := &Handler{deps: &Deps{NormalizeRegistry: canonicalRegistry()}}
	payload, _, _ := h.extractResponseForHooks(
		context.Background(),
		"openai",
		[]byte(canonicalResponseWithReasoning),
		"/v1/chat/completions",
		slog.Default(),
	)
	if payload == nil {
		t.Fatal("no hook payload was built for a well-formed canonical response")
	}

	projected := strings.Join(payload.TextProjection(), "\n")

	// The visible answer is the control: if this is missing the extraction is
	// broken outright and the reasoning assertion below would be meaningless.
	if !strings.Contains(projected, "balance is available") {
		t.Fatalf("the visible answer never reached the hook projection: %q", projected)
	}

	if !strings.Contains(projected, "4111 1111 1111 1111") {
		t.Errorf("reasoning text is delivered to the client but no hook can see it.\n"+
			"projection: %q\n"+
			"A scanner cannot redact what it is never shown, so the card number above "+
			"reaches the caller with an approve stamp on it.", projected)
	}
}
