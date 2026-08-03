package typology

import "testing"

// TestEndpointKindConstants pins the canonical wire-format string for every
// EndpointKind constant. These strings are persisted into traffic_event,
// embedded in MQ payloads, and used as Prometheus label values — renaming
// any of them is a coordinated breaking change. Failing this test means
// somebody changed a constant value without updating the migration / wire
// contract.
func TestEndpointKindConstants(t *testing.T) {
	cases := []struct {
		k    EndpointKind
		want string
	}{
		{EndpointKindChat, "chat"},
		{EndpointKindEmbeddings, "embeddings"},
		{EndpointKindImageGeneration, "image_generation"},
		{EndpointKindTTS, "tts"},
		{EndpointKindSTT, "stt"},
		{EndpointKindVideoGeneration, "video_generation"},
		{EndpointKindBatch, "batch"},
		{EndpointKindJob, "job"},
		{EndpointKindModels, "models"},
		{EndpointKindResponses, "responses"},
		{EndpointKindGuardrail, "guardrail"},
	}
	for _, c := range cases {
		if string(c.k) != c.want {
			t.Errorf("EndpointKind %v string = %q, want %q", c.k, string(c.k), c.want)
		}
		if c.k.String() != c.want {
			t.Errorf("(EndpointKind).String() = %q, want %q", c.k.String(), c.want)
		}
	}
}

// TestAllEndpointKindsExhaustive verifies the AllEndpointKinds slice
// matches the count + identity of constants defined in endpointkind.go.
// Adding a new constant requires appending to AllEndpointKinds; this
// test catches the forgotten-append failure mode.
func TestAllEndpointKindsExhaustive(t *testing.T) {
	want := []EndpointKind{
		EndpointKindChat,
		EndpointKindEmbeddings,
		EndpointKindRerank,
		EndpointKindImageGeneration,
		EndpointKindTTS,
		EndpointKindSTT,
		EndpointKindVideoGeneration,
		EndpointKindBatch,
		EndpointKindJob,
		EndpointKindModels,
		EndpointKindResponses,
		EndpointKindGuardrail,
		EndpointKindRealtime,
	}
	if len(AllEndpointKinds) != len(want) {
		t.Fatalf("len(AllEndpointKinds) = %d, want %d", len(AllEndpointKinds), len(want))
	}
	for i, k := range want {
		if AllEndpointKinds[i] != k {
			t.Errorf("AllEndpointKinds[%d] = %v, want %v", i, AllEndpointKinds[i], k)
		}
	}
}

func TestEndpointKindAcceptsModelType(t *testing.T) {
	cases := []struct {
		kind      EndpointKind
		modelType string
		want      bool
	}{
		// chat / responses accept only chat models.
		{EndpointKindChat, "chat", true},
		{EndpointKindChat, "image", false},
		{EndpointKindChat, "audio", false},
		{EndpointKindChat, "embedding", false},
		{EndpointKindResponses, "chat", true},
		{EndpointKindResponses, "image", false},
		// embeddings.
		{EndpointKindEmbeddings, "embedding", true},
		{EndpointKindEmbeddings, "chat", false},
		// image generation accepts image only.
		{EndpointKindImageGeneration, "image", true},
		{EndpointKindImageGeneration, "chat", false},
		{EndpointKindImageGeneration, "video", false},
		// the three audio endpoints accept the coarse `audio` type AND their
		// own fine type, and reject a sibling audio sub-modality's fine type.
		{EndpointKindTTS, "audio", true},
		{EndpointKindTTS, "tts", true},
		{EndpointKindTTS, "stt", false},
		{EndpointKindTTS, "chat", false},
		{EndpointKindSTT, "audio", true},
		{EndpointKindSTT, "stt", true},
		{EndpointKindSTT, "tts", false},
		{EndpointKindSTT, "image", false},
		{EndpointKindRealtime, "audio", true},
		{EndpointKindRealtime, "realtime", true},
		{EndpointKindRealtime, "tts", false},
		{EndpointKindRealtime, "chat", false},
		// video accepts video and image (Sora is catalogued image).
		{EndpointKindVideoGeneration, "video", true},
		{EndpointKindVideoGeneration, "image", true},
		{EndpointKindVideoGeneration, "chat", false},
		// rerank.
		{EndpointKindRerank, "rerank", true},
		{EndpointKindRerank, "chat", false},
		// kinds that do not bind to a catalog model impose no constraint.
		{EndpointKindGuardrail, "chat", true},
		{EndpointKindGuardrail, "image", true},
		{EndpointKindBatch, "audio", true},
		// empty modelType is unconstrained (fail-open) for every kind.
		{EndpointKindChat, "", true},
		{EndpointKindImageGeneration, "", true},
	}
	for _, c := range cases {
		if got := EndpointKindAcceptsModelType(c.kind, c.modelType); got != c.want {
			t.Errorf("EndpointKindAcceptsModelType(%q, %q) = %v, want %v", c.kind, c.modelType, got, c.want)
		}
	}
}

// TestModalityMatrixExhaustive guards against the "orphaned model type" class of
// bug: every model-type in the catalog vocabulary must be routable by at least
// one endpoint kind (a type the schema blesses but no endpoint accepts would be
// rejected everywhere), and every model-binding endpoint kind must accept at
// least one type (a binding kind that accepts nothing rejects all traffic).
func TestModalityMatrixExhaustive(t *testing.T) {
	// The full Model.type vocabulary (tools/db-migrate/schema/providers.prisma).
	modelTypes := []string{"chat", "embedding", "image", "audio", "rerank", "video", "realtime", "tts", "stt"}
	// Endpoint kinds that resolve to a catalog model (must constrain by type).
	modelBinding := []EndpointKind{
		EndpointKindChat, EndpointKindResponses, EndpointKindEmbeddings,
		EndpointKindImageGeneration, EndpointKindTTS, EndpointKindSTT,
		EndpointKindRealtime, EndpointKindVideoGeneration, EndpointKindRerank,
	}

	for _, mt := range modelTypes {
		routable := false
		for _, k := range modelBinding {
			if EndpointKindAcceptsModelType(k, mt) {
				routable = true
				break
			}
		}
		if !routable {
			t.Errorf("model type %q is orphaned: no model-binding endpoint kind accepts it", mt)
		}
	}

	for _, k := range modelBinding {
		accepts := false
		for _, mt := range modelTypes {
			if EndpointKindAcceptsModelType(k, mt) {
				accepts = true
				break
			}
		}
		if !accepts {
			t.Errorf("endpoint kind %q accepts no model type: it would reject all traffic", k)
		}
	}
}

func TestEndpointKind_IsValid(t *testing.T) {
	for _, k := range AllEndpointKinds {
		if !k.IsValid() {
			t.Errorf("IsValid(%v) = false, want true for defined constant", k)
		}
	}
	// Empty string is NOT a valid EndpointKind — callers needing
	// "unclassified" semantics check for "" separately.
	if EndpointKind("").IsValid() {
		t.Errorf("IsValid(\"\") = true, want false (empty is unclassified, not valid)")
	}
	// Random non-defined string is invalid.
	if EndpointKind("bogus").IsValid() {
		t.Errorf("IsValid(\"bogus\") = true, want false")
	}
}
