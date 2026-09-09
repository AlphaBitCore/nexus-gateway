package typology

import "testing"

// A DENYLIST naming only image generation and TTS fails open for every other
// non-streaming kind: `{"input":"…","stream":true}` on /v1/embeddings or
// /v1/rerank sets Stream on the upstream request and takes the SSE responder,
// for an upstream that answers with one JSON object and has no event stream to
// parse.
//
// These arms pin the ALLOWLIST, and pin it exhaustively against
// AllEndpointKinds: a kind added later without a decision recorded here fails
// the test rather than silently inheriting "streams".

func TestEndpointKindSupportsStreaming_ExactSet(t *testing.T) {
	// The complete expected answer for every kind that exists. Adding a kind
	// without adding it here is the failure this map is for.
	want := map[EndpointKind]bool{
		EndpointKindChat:            true,
		EndpointKindResponses:       true,
		EndpointKindRealtime:        true, // WebSocket; streaming by protocol
		EndpointKindEmbeddings:      false,
		EndpointKindRerank:          false,
		EndpointKindImageGeneration: false,
		EndpointKindTTS:             false,
		EndpointKindSTT:             false,
		EndpointKindVideoGeneration: false,
		EndpointKindBatch:           false,
		EndpointKindJob:             false,
		EndpointKindModels:          false,
		EndpointKindGuardrail:       false,
	}

	if len(want) != len(AllEndpointKinds) {
		t.Fatalf("this test decides %d kinds but AllEndpointKinds has %d — a kind was added without a streaming decision",
			len(want), len(AllEndpointKinds))
	}
	for _, k := range AllEndpointKinds {
		exp, ok := want[k]
		if !ok {
			t.Errorf("%s has no streaming decision recorded", k)
			continue
		}
		if got := EndpointKindSupportsStreaming(k); got != exp {
			t.Errorf("EndpointKindSupportsStreaming(%s) = %v, want %v", k, got, exp)
		}
	}
}

// An unclassified request must not be able to opt into streaming either: an
// empty kind is what an unrecognised wire shape resolves to, and "we could not
// tell what this is" is not a reason to hand it the SSE responder.
func TestEndpointKindSupportsStreaming_UnknownIsNotStreaming(t *testing.T) {
	for _, k := range []EndpointKind{"", "not-a-kind", "chat "} {
		if EndpointKindSupportsStreaming(k) {
			t.Errorf("EndpointKindSupportsStreaming(%q) = true — an unrecognised kind must default to non-stream", k)
		}
	}
}

// The two kinds the old denylist DID name must still be refused, so the
// allowlist is a strict superset of the protection it replaced.
func TestEndpointKindSupportsStreaming_KeepsTheOldDenylist(t *testing.T) {
	for _, k := range []EndpointKind{EndpointKindImageGeneration, EndpointKindTTS} {
		if EndpointKindSupportsStreaming(k) {
			t.Errorf("%s streams again — cost metering and the artifact fingerprint live only on the non-stream path", k)
		}
	}
}
