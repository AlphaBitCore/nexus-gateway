package typology

import (
	"os"
	"strings"
	"testing"
)

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
		{EndpointKindTTS, "tts", true},
		{EndpointKindTTS, "stt", false},
		{EndpointKindTTS, "chat", false},
		{EndpointKindSTT, "stt", true},
		{EndpointKindSTT, "tts", false},
		{EndpointKindSTT, "image", false},
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
		// Batch is unconstrained, so it accepts anything — including a
		// string no model carries any more.
		{EndpointKindBatch, "audio", true},
		// "audio" is DEPRECATED, not removed. No new row acquires it — the
		// discovery heuristic stopped minting it and the seeded rows are
		// retyped — but an admin-created row carrying it predates that, and
		// no fixture reseed repairs such a row. Refusing it here would make
		// it permanently unroutable on every endpoint with no migration,
		// which is the shipped-contract break the 1.0 GA rule forbids.
		{EndpointKindTTS, "audio", true},
		{EndpointKindSTT, "audio", true},
		{EndpointKindRealtime, "audio", true},
		// What was actually wrong: chat never took it, so gpt-audio-* were
		// rejected on the endpoint that serves them. That is fixed by the
		// TYPE being right, not by widening this arm.
		{EndpointKindChat, "audio", false},
		{EndpointKindChat, "chat", true},
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
	modelTypes := []string{"chat", "embedding", "image", "rerank", "video", "realtime", "tts", "stt"}
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

// TestSimulate_TheFormOffersEveryConstrainedEndpointKind.
//
// The rule-detail simulator lets an admin choose which endpoint the simulated
// request arrives on, because that is the input a rule's outcome most depends
// on: modelTypes conditions, the modality filter and non-chat auto all key off
// it. Locked to chat — which is how it shipped — it taught admins the wrong
// thing about every other kind.
//
// The <select> needs a list, so there are now two: this predicate and a
// TypeScript array. A second vocabulary that can drift from the first is a
// defect this codebase has already produced, so the two are compared here.
//
// The four kinds deliberately absent (batch, job, models, guardrail) are the
// ones this predicate leaves unconstrained — they accept any model type, so
// simulating them teaches nothing. Asserting their absence is half the point:
// a list that grew to all thirteen would offer choices that change no answer.
func TestSimulate_TheFormOffersEveryConstrainedEndpointKind(t *testing.T) {
	const form = "../../../control-plane-ui/src/pages/ai-gateway/routing/_shared/routing-rule-config.ts"
	src, err := os.ReadFile(form)
	if err != nil {
		t.Fatalf("read %s: %v — the form holds one of the two lists; a test that cannot find "+
			"it silently stops comparing", form, err)
	}
	block := string(src)
	const decl = "SIMULATABLE_ENDPOINT_KINDS = ["
	start := strings.Index(block, decl)
	if start < 0 {
		t.Fatalf("%s no longer declares SIMULATABLE_ENDPOINT_KINDS", form)
	}
	// From AFTER the bracket: the declaration itself shares a comma-delimited
	// run with the first entry, and a parser that swallowed it would report
	// that entry missing forever.
	body := block[start+len(decl):]
	end := strings.Index(body, "]")
	if end < 0 {
		t.Fatalf("%s: SIMULATABLE_ENDPOINT_KINDS is not closed", form)
	}
	listed := map[string]bool{}
	for _, part := range strings.Split(body[:end], ",") {
		if v := strings.Trim(strings.TrimSpace(part), "'\""); v != "" {
			listed[v] = true
		}
	}

	// "Constrained" is asked of the predicate, not written down: a kind that
	// starts constraining a model type joins the form's list without anyone
	// remembering, because this goes red until it does.
	for _, k := range AllEndpointKinds {
		constrained := !EndpointKindAcceptsModelType(k, "definitely-not-a-model-type")
		switch {
		case constrained && !listed[string(k)]:
			t.Errorf("%s constrains which models may serve it, and the simulator does not "+
				"offer it — an admin cannot ask the one question whose answer differs", k)
		case !constrained && listed[string(k)]:
			t.Errorf("%s accepts any model type, so offering it in the simulator adds a "+
				"choice that changes no answer", k)
		}
	}
	if len(listed) == 0 {
		t.Errorf("the form's list parsed to nothing — this test would then pass against a " +
			"form that offers no endpoint at all")
	}
}
