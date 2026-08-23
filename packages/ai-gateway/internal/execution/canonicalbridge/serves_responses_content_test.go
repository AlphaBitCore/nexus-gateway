package canonicalbridge

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// ServesResponses is the one signal that decides, on a /v1/responses ingress,
// whether the body is sent Responses-shape or canonicalized to chat. Reporting
// "does not serve" for a body the Responses wire cannot carry is what routes
// such a request down the wire that CAN carry it, with the response converted
// back to the shape the caller asked for.
//
// Without this the caller gets a rejection for a request the gateway could
// have served, and a message telling them to go change wires themselves —
// which is the work the gateway exists to do on their behalf.

const responsesBodyWithAudio = `{"model":"gpt-audio-mini","input":[{"role":"user","content":[` +
	`{"type":"input_text","text":"what is said?"},` +
	`{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}}]}]}`

const responsesBodyTextOnly = `{"model":"gpt-5.4","input":"hello"}`

func TestServesResponses_ContentTheWireCannotCarryForcesTheChatRoute(t *testing.T) {
	b := New(nil)

	if b.ServesResponses(provcore.FormatOpenAI, nil, []byte(responsesBodyWithAudio)) {
		t.Error("reported the Responses wire as serving a body carrying audio; the wire has " +
			"no audio content part, so the request would be forwarded to a refusal instead " +
			"of to /v1/chat/completions, which the same model serves")
	}

	if !b.ServesResponses(provcore.FormatOpenAI, nil, []byte(responsesBodyTextOnly)) {
		t.Error("a text-only Responses body must still take the native wire — downgrading it " +
			"would lose built-in tools and stateful fields for every ordinary request")
	}
}

// The content check must not resurrect a capability the target does not have,
// and must not override an explicit opt-out. Order matters: a chat-only
// OpenAI-compatible endpoint stays false whatever the body carries.
func TestServesResponses_ContentCheckDoesNotOverrideTargetOrOverride(t *testing.T) {
	b := New(nil)
	no := false
	yes := true

	if b.ServesResponses(provcore.FormatOpenAI, &no, []byte(responsesBodyTextOnly)) {
		t.Error("a false override must win regardless of content")
	}
	if b.ServesResponses(provcore.FormatAnthropic, &yes, []byte(responsesBodyTextOnly)) {
		t.Error("a true override cannot grant a wire the adapter does not serve")
	}
	if b.ServesResponses(provcore.FormatAnthropic, nil, []byte(responsesBodyWithAudio)) {
		t.Error("a non-responses target is false on its own account")
	}
}

// A nil body must not be read as "carries something unservable". The predicate
// is consulted on paths that have no body in hand yet, and a nil that forced
// the chat route would silently downgrade every one of them.
func TestServesResponses_NilBodyIsNotAnExceedance(t *testing.T) {
	b := New(nil)
	if !b.ServesResponses(provcore.FormatOpenAI, nil, nil) {
		t.Error("nil body reported as unservable; absence of content is not unservable content")
	}
}
