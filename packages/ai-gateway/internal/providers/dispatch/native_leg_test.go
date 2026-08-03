package dispatch

// Native-leg triage + RewriteNative property pins for the request-contract
// mechanism: the codec is always in the path; the native leg skips only the
// canonical round-trip. The triage is three keys, never naive format
// equality (that check shipped a 400 once — /v1/responses to an
// OpenAI target was canonicalized instead of forwarded natively).

import (
	"bytes"
	"log/slog"
	"testing"

	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func triageAdapter(f Format, shapes []typology.WireShape) *specAdapter {
	return &specAdapter{spec: AdapterSpec{
		Format:        f,
		SchemaCodec:   openaicodec.New(openaicodec.Contract{}),
		RequestShapes: shapes,
	}, log: slog.Default()}
}

func TestNativeLeg_ThreeKeyTriage(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	responsesShapes := []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIResponses}

	cases := []struct {
		name   string
		body   Format
		target Format
		wire   typology.WireShape
		shapes []typology.WireShape
		serves *bool
		want   bool
	}{
		// Key 1 — true same format.
		{"same format", FormatAnthropic, FormatAnthropic, typology.WireShapeAnthropicMessages, nil, nil, true},
		// Key 2 — both OpenAI-family (distinct formats).
		{"openai family both sides", FormatOpenAI, FormatMoonshot, typology.WireShapeOpenAIChat, nil, nil, true},
		{"family one side only", FormatOpenAI, FormatAnthropic, typology.WireShapeAnthropicMessages, nil, nil, false},
		// Key 3 — Responses capability: formats differ, verbatim is correct.
		{"responses to responses-serving adapter", FormatOpenAIResponses, FormatOpenAI, typology.WireShapeOpenAIResponses, responsesShapes, nil, true},
		{"responses, adapter does not serve it", FormatOpenAIResponses, FormatOpenAI, typology.WireShapeOpenAIResponses, nil, nil, false},
		// The override is DOWNGRADE-ONLY (canonicalbridge parity; the field's
		// contract says true cannot exceed the adapter capability) — an
		// operator flag must never route a verbatim Responses body onto a
		// wire whose codec cannot serve it.
		{"responses, override cannot grant beyond adapter shapes", FormatOpenAIResponses, FormatOpenAI, typology.WireShapeOpenAIResponses, nil, boolPtr(true), false},
		{"responses, provider override revokes it", FormatOpenAIResponses, FormatOpenAI, typology.WireShapeOpenAIResponses, responsesShapes, boolPtr(false), false},
		{"responses, override true confirms declared capability", FormatOpenAIResponses, FormatOpenAI, typology.WireShapeOpenAIResponses, responsesShapes, boolPtr(true), true},
		// Key 3 requires the responses WIRE, not just the body format.
		{"responses body on a chat wire", FormatOpenAIResponses, FormatOpenAI, typology.WireShapeOpenAIChat, responsesShapes, nil, false},
		// Plain cross-format goes to the codec.
		{"cross format", FormatOpenAI, FormatGemini, typology.WireShapeGeminiGenerateContent, nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := triageAdapter(tc.target, tc.shapes)
			got := a.nativeLeg(Request{
				BodyFormat: tc.body,
				WireShape:  tc.wire,
				Target:     CallTarget{ServesResponsesAPI: tc.serves},
			})
			if got != tc.want {
				t.Fatalf("nativeLeg = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRewriteNative_VerbatimFastPath_SameSlice pins the zero-copy contract:
// a body with nothing due comes back as the SAME slice, not a copy — the
// fast path's whole point on a ~50KB body.
func TestRewriteNative_VerbatimFastPath_SameSlice(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	res, err := openaicodec.New(openaicodec.Contract{}).RewriteNative(
		typology.WireShapeOpenAIChat, body, CallTarget{ProviderModelID: "gpt-4o"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("verbatim fast path must return the same slice, not a copy")
	}
}

// TestRewriteNative_Idempotent is the property pin from the design: applying
// the differential to its own output changes nothing and reports no
// duplicate rewrites — the executor re-prepares on retry/failover, so a
// non-idempotent differential would double-mutate the second attempt.
func TestRewriteNative_Idempotent(t *testing.T) {
	target := CallTarget{ProviderModelID: "provider-real-model"}
	cases := []struct {
		name   string
		shape  typology.WireShape
		body   []byte
		stream bool
	}{
		{"chat stamp", typology.WireShapeOpenAIChat, []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`), false},
		{"chat streaming inject", typology.WireShapeOpenAIChat, []byte(`{"model":"alias","messages":[]}`), true},
		{"chat dup-key collapse", typology.WireShapeOpenAIChat, []byte(`{"model":"a","messages":[],"model":"b"}`), false},
		{"embeddings stamp", typology.WireShapeOpenAIEmbeddings, []byte(`{"model":"alias","input":"x"}`), false},
		{"responses stamp+strip", typology.WireShapeOpenAIResponses, []byte(`{"model":"alias","input":"x","temperature":0}`), false},
	}
	codec := openaicodec.New(openaicodec.Contract{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := codec.RewriteNative(tc.shape, tc.body, target, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			second, err := codec.RewriteNative(tc.shape, first.Body, target, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Body, second.Body) {
				t.Fatalf("not idempotent:\n first=%s\nsecond=%s", first.Body, second.Body)
			}
			if len(second.Rewrites) != 0 {
				t.Fatalf("second pass reported rewrites %v — the differential re-applied on its own output means duplicate x-nexus-coerced entries on retry/failover", second.Rewrites)
			}
		})
	}
}

// TestPrepareNative_NonObjectCarveOut pins the dispatch carve-out boundary:
// a non-object body — array, bare scalar, or whitespace-only — is forwarded
// BYTE-VERBATIM with no rewrites and the codec is never asked to edit it.
// A JSON edit on any of these fabricates a synthetic object: the upstream
// would receive an invented request while the audit row stored the client's
// original bytes (wire ≠ audit).
func TestPrepareNative_NonObjectCarveOut(t *testing.T) {
	a := &specAdapter{spec: AdapterSpec{
		Format:      FormatOpenAI,
		SchemaCodec: openaicodec.New(openaicodec.Contract{}),
	}, log: slog.Default()}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"array", `[{"model":"alias"}]`},
		{"bare scalar", `"model"`},
		{"whitespace only", "   \n\t "},
		{"leading whitespace then array", "  \n[1,2]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rw, _, err := a.prepareBodyFull(Request{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: FormatOpenAI,
				Body:       []byte(tc.body),
				Target:     CallTarget{ProviderModelID: "provider-real-model"},
			})
			if err != nil {
				t.Fatalf("carve-out must not error: %v", err)
			}
			if string(got) != tc.body {
				t.Fatalf("non-object body must forward byte-verbatim:\n got=%q\nwant=%q", got, tc.body)
			}
			if rw != nil {
				t.Fatalf("carve-out must report no rewrites, got %v", rw)
			}
		})
	}
}

// TestRewriteNative_StreamingMissingStreamOptions_Injected pins the exact
// injection condition: an otherwise-conformant streaming body (provider
// model set, stream:true) that merely lacks stream_options MUST get
// include_usage injected — without it the audit row silently degrades to
// usage_extraction_status=streaming_unavailable.
func TestRewriteNative_StreamingMissingStreamOptions_Injected(t *testing.T) {
	body := []byte(`{"model":"provider-real-model","stream":true,"messages":[]}`)
	res, err := openaicodec.New(openaicodec.Contract{}).RewriteNative(
		typology.WireShapeOpenAIChat, body, CallTarget{ProviderModelID: "provider-real-model"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(res.Body, []byte(`"include_usage":true`)) {
		t.Fatalf("conformant-but-optionless streaming body must gain include_usage: %s", res.Body)
	}
}

// TestPrepareBody_FastPathAllocs makes the G1/G3 alloc pins CI-enforceable:
// the verbatim fast path stays at 1 alloc (gjson's matched-value copy) and
// the streaming all-skip at ≤4 (the three-field probe) — a second decode
// pass or a hoisted scan that allocates shows up here as a hard failure.
func TestPrepareBody_FastPathAllocs(t *testing.T) {
	a := &specAdapter{spec: AdapterSpec{
		Format:      FormatOpenAI,
		SchemaCodec: openaicodec.New(openaicodec.Contract{}),
	}, log: slog.Default()}
	fastReq := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		Target:     CallTarget{ProviderModelID: "gpt-4o"},
	}
	if n := testing.AllocsPerRun(200, func() {
		if _, _, _, err := a.prepareBodyFull(fastReq); err != nil {
			t.Fatal(err)
		}
	}); n > 1 {
		t.Fatalf("verbatim fast path allocs = %v, want ≤1", n)
	}
	streamReq := fastReq
	streamReq.Stream = true
	streamReq.Body = []byte(`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	if n := testing.AllocsPerRun(200, func() {
		if _, _, _, err := a.prepareBodyFull(streamReq); err != nil {
			t.Fatal(err)
		}
	}); n > 4 {
		t.Fatalf("streaming all-skip allocs = %v, want ≤4", n)
	}
}

// TestPrepareNative_ContractQuirkPath_MatchesLegacyBytes pins the codec
// contract path end-to-end through dispatch: a quirk-family model on the
// streaming chat wire gets the strip + model stamp + usage injection in
// one decode-door round-trip — exactly what the deleted transitional
// callback branch produced — while a non-quirk model takes the surgical
// stamp and keeps its temperature.
func TestPrepareNative_ContractQuirkPath_MatchesLegacyBytes(t *testing.T) {
	quirkGate := func(modelID string) bool { return modelID == "quirk-model" }
	a := &specAdapter{spec: AdapterSpec{
		Format: FormatOpenAI,
		SchemaCodec: openaicodec.New(openaicodec.Contract{
			Chat: []openaicodec.FieldRule{{Applies: quirkGate, Field: "temperature"}},
		}),
	}, log: slog.Default()}

	body := []byte(`{"model":"alias","messages":[],"temperature":0}`)
	got, rw, _, err := a.prepareBodyFull(Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       body,
		Stream:     true,
		Target:     CallTarget{ProviderModelID: "quirk-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rw) != 1 || rw[0] != "temperature→removed" {
		t.Fatalf("contract rewrites = %v", rw)
	}
	for _, want := range []string{`"model":"quirk-model"`, `"stream":true`, `"include_usage":true`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("contract path lost %s; body=%s", want, got)
		}
	}
	if bytes.Contains(got, []byte("temperature")) {
		t.Fatalf("quirk strip did not fire; body=%s", got)
	}

	// A non-quirk model on the same adapter keeps the surgical path: no
	// rewrites, temperature intact.
	surgical, rw2, _, err := a.prepareBodyFull(Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"alias","messages":[],"temperature":0}`),
		Target:     CallTarget{ProviderModelID: "plain-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rw2 != nil {
		t.Fatalf("non-quirk model must not report rewrites, got %v", rw2)
	}
	if !bytes.Contains(surgical, []byte(`"temperature":0`)) {
		t.Fatalf("non-quirk model lost its temperature; body=%s", surgical)
	}
}
