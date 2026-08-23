package dispatch

// The sjson fast path for the passthrough
// model rewrite must be behaviourally identical to the map[string]any round-trip
// it replaces, and must preserve the strict gate (nexus strip; fall back to the
// map path for streaming / per-adapter-rewrite / duplicate-top-level-model).

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func canon(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canon unmarshal: %v (body=%s)", err, b)
	}
	out, err := json.Marshal(v) // encoding/json sorts map keys → stable form
	if err != nil {
		t.Fatalf("canon marshal: %v", err)
	}
	return string(out)
}

func chatBody(extra string) []byte {
	base := `{"model":"client-alias","messages":[{"role":"user","content":"hi"}]`
	if extra != "" {
		base += "," + extra
	}
	return []byte(base + "}")
}

func reqFor(body []byte, stream bool) Request {
	return Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       body,
		Stream:     stream,
		Target:     CallTarget{ProviderModelID: "provider-real-model"},
	}
}

// rpm drives the native leg through prepareBodyFull on an OpenAI-format
// adapter with the real identity codec — the same composition production
// wiring uses, so these tests pin dispatch guards + triage + codec
// differential together (model rewrite driven by the OpenAI wire shape).
func rpm(req Request) ([]byte, []string, error) {
	a := &specAdapter{spec: AdapterSpec{
		Format:      FormatOpenAI,
		SchemaCodec: openaicodec.New(openaicodec.Contract{}),
	}, log: slog.Default()}
	body, rewrites, _, err := a.prepareBodyFull(req)
	return body, rewrites, err
}

// rpmForceDecode is rpm with an always-gating no-op structural rule, which
// routes every chat body through the decode door — the semantic reference
// the surgical fast path must be equivalent to.
func rpmForceDecode(req Request) ([]byte, []string, error) {
	a := &specAdapter{spec: AdapterSpec{
		Format: FormatOpenAI,
		SchemaCodec: openaicodec.New(openaicodec.Contract{
			ChatStructural: []openaicodec.StructuralRule{{
				Applies: func(string) bool { return true },
				Apply:   func(map[string]any, string) []string { return nil },
			}},
		}),
	}, log: slog.Default()}
	body, rewrites, _, err := a.prepareBodyFull(req)
	return body, rewrites, err
}

// stampingCodec is a minimal model-in-body codec double: its RewriteNative
// is the shared surgical stamp, mirroring what the real anthropic codec's
// differential does today. The real anthropic codec cannot be imported here
// (it imports this package for a metrics emitter), so its RewriteNative
// semantics are pinned in its own package tests; THESE tests pin the
// dispatch side — guards, triage, and delegation — against a model-in-body
// wire.
type stampingCodec struct{}

func (stampingCodec) EncodeRequest(_ typology.WireShape, canonicalBody []byte, _ CallTarget) (EncodeResult, error) {
	return EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
}

func (stampingCodec) DecodeResponse(_ typology.WireShape, nativeBody []byte, _ string, _ DecodeContext) (DecodeResult, error) {
	return DecodeResult{CanonicalBody: nativeBody}, nil
}

func (stampingCodec) RewriteNative(_ typology.WireShape, nativeBody []byte, target CallTarget, _ bool) (EncodeResult, error) {
	out, err := SurgicalModelStamp(nativeBody, target.ProviderModelID)
	if err != nil {
		return EncodeResult{}, err
	}
	return EncodeResult{Body: out, ContentType: "application/json"}, nil
}

// rpmAnthropic drives the Anthropic-shaped native /v1/messages leg (the
// model-at-body-root stamp is the codec's RewriteNative differential now).
func rpmAnthropic(req Request) ([]byte, []string, error) {
	a := &specAdapter{spec: AdapterSpec{
		Format:      FormatAnthropic,
		SchemaCodec: stampingCodec{},
	}, log: slog.Default()}
	body, rewrites, _, err := a.prepareBodyFull(req)
	return body, rewrites, err
}

// anthropicReqFor builds an Anthropic /v1/messages passthrough request: the
// native wire shape, FormatAnthropic body, and a model that differs from the
// resolved ProviderModelID (the alias/routing case).
func anthropicReqFor(body []byte) Request {
	return Request{
		WireShape:  typology.WireShapeAnthropicMessages,
		BodyFormat: FormatAnthropic,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "claude-opus-4-8"},
	}
}

// TestA2_FastPath_SetsModel: non-stream, no rewrite → fast path.
func TestA2_FastPath_SetsModel(t *testing.T) {
	body := chatBody(`"nexus":{"ext":{"x":1}},"temperature":0.5`)
	out, rewrites, err := rpm(reqFor(body, false))
	if err != nil {
		t.Fatal(err)
	}
	if rewrites != nil {
		t.Errorf("fast path must not return rewrites, got %v", rewrites)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "provider-real-model" {
		t.Errorf("model = %v, want provider-real-model", m["model"])
	}
	// `nexus` is deliberately still present here. The carrier is removed at the
	// transport frame, not on a leg: removing it before the codec would kill
	// nexus.ext.<provider>.<key>, which the codec has to READ. Asserted end to
	// end by TestEgress_NoInternalCarrierReachesTheTransport, on both legs.
	if m["temperature"] != 0.5 {
		t.Errorf("temperature lost: %v", m["temperature"])
	}
}

// TestA2_FastPath_EqualsMapPath: fast path output canonicalises identically to
// the old map round-trip for a normal body.
func TestA2_FastPath_EqualsMapPath(t *testing.T) {
	body := chatBody(`"temperature":0.5,"top_p":0.9`)
	fast, _, err := rpm(reqFor(body, false))
	if err != nil {
		t.Fatal(err)
	}
	// Force the decode door with a no-op structural rule.
	mapOut, _, err := rpmForceDecode(reqFor(body, false))
	if err != nil {
		t.Fatal(err)
	}
	if canon(t, fast) != canon(t, mapOut) {
		t.Errorf("fast path diverges from map path:\n fast=%s\n  map=%s", canon(t, fast), canon(t, mapOut))
	}
}

// TestA2_DuplicateModel_FallsBackToMapPath: a duplicate top-level model must
// resolve to the provider model with NO stale duplicate (map path collapses).
func TestA2_DuplicateModel_FallsBackToMapPath(t *testing.T) {
	body := []byte(`{"model":"alias-a","messages":[],"model":"alias-b"}`)
	out, _, err := rpm(reqFor(body, false))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "provider-real-model" {
		t.Errorf("model = %v, want provider-real-model (no stale duplicate)", m["model"])
	}
	if strings.Count(string(out), `"model"`) != 1 {
		t.Errorf("expected exactly one model key after rewrite, got %q", out)
	}
}

// TestA2_Streaming_NonConformant_MapPath_AppliesUsageOption: a streaming body
// that still needs a rewrite (model alias, no stream/usage fields) takes the
// map[string]any path — it must rename the model, inject
// stream_options.include_usage + stream:true, and preserve sibling fields.
func TestA2_Streaming_NonConformant_MapPath_AppliesUsageOption(t *testing.T) {
	body := chatBody(`"temperature":0.5`)
	out, rewrites, err := rpm(reqFor(body, true))
	if err != nil {
		t.Fatal(err)
	}
	if rewrites != nil {
		t.Errorf("no adapter rewrite configured, got %v", rewrites)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "provider-real-model" {
		t.Errorf("model = %v, want provider-real-model", m["model"])
	}
	so, ok := m["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("streaming must set stream_options.include_usage=true, got %v", m["stream_options"])
	}
	if m["stream"] != true {
		t.Errorf("streaming must set stream=true, got %v", m["stream"])
	}
	if m["temperature"] != 0.5 {
		t.Errorf("sibling field temperature must be preserved, got %v", m["temperature"])
	}
}

// TestA2_Streaming_AlreadyConformant_ZeroRewrite: when the body already carries
// the provider model + stream:true + stream_options.include_usage, the streaming
// all-skip returns it byte-identical with ZERO allocation — the common shape for
// the loadtest and real OpenAI-stream clients that send the upstream model name.
func TestA2_Streaming_AlreadyConformant_ZeroRewrite(t *testing.T) {
	body := []byte(`{"model":"provider-real-model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	out, _, err := rpm(reqFor(body, true))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("already-conformant streaming body must be returned byte-identical (zero rewrite):\n got=%s\nwant=%s", out, body)
	}
}

// TestA2_Streaming_ConformantButStructuralRuleApplies_TakesDecodeDoor: even a
// conformant body must NOT all-skip when a gate-matching structural rule
// exists — the rule has to run, so the decode door is taken and the body is
// round-tripped.
func TestA2_Streaming_ConformantButStructuralRuleApplies_TakesDecodeDoor(t *testing.T) {
	body := []byte(`{"model":"provider-real-model","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	called := false
	a := &specAdapter{spec: AdapterSpec{
		Format: FormatOpenAI,
		SchemaCodec: openaicodec.New(openaicodec.Contract{
			ChatStructural: []openaicodec.StructuralRule{{
				Applies: func(string) bool { return true },
				Apply:   func(map[string]any, string) []string { called = true; return []string{"r"} },
			}},
		}),
	}, log: slog.Default()}
	out, rewrites, _, err := a.prepareBodyFull(reqFor(body, true))
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("structural rule must run even for a conformant body (all-skip must not swallow it)")
	}
	if len(rewrites) != 1 {
		t.Errorf("rewrites = %v, want one entry", rewrites)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "provider-real-model" {
		t.Errorf("model = %v", m["model"])
	}
}

// TestA2_EdgeCases locks the fast-path edge behaviour: a malformed client body is
// the client's problem (no gw-side json.Valid gate — the upstream rejects it);
// absent/numeric model must still resolve to the provider model string; embeddings
// wire shape takes the fast path.
func TestA2_EdgeCases(t *testing.T) {
	t.Run("malformed_json_is_clients_problem_no_gw_gate", func(t *testing.T) {
		// The passthrough fast path no longer pays a full-body
		// json.Valid just to force a gw 400 on malformed input. A truncated body
		// either gets its model rewritten and forwarded (upstream returns the
		// error) or sjson itself errors — both acceptable; the gateway must not
		// crash and must produce a deterministic result.
		out, _, err := rpm(reqFor([]byte(`{"model":"x"`), false))
		if err == nil && len(out) == 0 {
			t.Error("expected either a forwarded body or an error, got neither")
		}
	})
	t.Run("absent_model_added", func(t *testing.T) {
		out, _, err := rpm(reqFor([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), false))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if m["model"] != "provider-real-model" {
			t.Errorf("absent model not added: %v", m["model"])
		}
	})
	t.Run("numeric_model_overwritten_with_string", func(t *testing.T) {
		out, _, err := rpm(reqFor([]byte(`{"model":123,"messages":[]}`), false))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if m["model"] != "provider-real-model" {
			t.Errorf("numeric model not overwritten with provider string: %v (%T)", m["model"], m["model"])
		}
	})
	t.Run("embeddings_wire_shape_fast_path", func(t *testing.T) {
		req := Request{
			WireShape:  typology.WireShapeOpenAIEmbeddings,
			BodyFormat: FormatOpenAI,
			Body:       []byte(`{"model":"client-alias","input":"hello"}`),
			Target:     CallTarget{ProviderModelID: "provider-real-model"},
		}
		out, _, err := rpm(req)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if m["model"] != "provider-real-model" {
			t.Errorf("embeddings model rewrite failed: %v", m["model"])
		}
	})
}

func bigChatBodyA2() []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"client-alias","stream":false,"messages":[`)
	for i := range 40 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"role":"user","content":"`)
		sb.WriteString(strings.Repeat("the quick brown fox ", 60))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func BenchmarkA2ModelRewrite(b *testing.B) {
	body := bigChatBodyA2()
	b.Run("fast_sjson", func(b *testing.B) {
		req := reqFor(body, false)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rpm(req)
		}
	})
	b.Run("old_map_roundtrip", func(b *testing.B) {
		req := reqFor(body, false)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rpmForceDecode(req)
		}
	})
}

// BenchmarkA2StreamAllSkip measures the streaming all-skip (conformant body →
// zero rewrite) against the map round-trip a non-conformant body pays. The
// all-skip arm should report ~0 B/op.
func BenchmarkA2StreamAllSkip(b *testing.B) {
	conformant := []byte(`{"model":"provider-real-model","stream":true,"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"user","content":"` + strings.Repeat("the quick brown fox ", 600) + `"}]}`)
	rewrite := bigChatBodyA2() // model=client-alias → needs the map round-trip
	b.Run("conformant_zero_rewrite", func(b *testing.B) {
		req := reqFor(conformant, true)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rpm(req)
		}
	})
	b.Run("nonconformant_map_path", func(b *testing.B) {
		req := reqFor(rewrite, true)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rpm(req)
		}
	})
}

// TestStructuralGate_RoutesTheDecodeDoor locks the gating behavior the
// deleted applies-probe used to provide: a model outside every structural
// rule's gate takes the surgical path (rule never invoked, fields
// preserved verbatim), while a gate-matching model takes the decode door
// exactly once.
func TestStructuralGate_RoutesTheDecodeDoor(t *testing.T) {
	body := chatBody(`"temperature":0.7`)
	ruleCalls := 0
	adapterFor := func(gate func(string) bool) *specAdapter {
		return &specAdapter{spec: AdapterSpec{
			Format: FormatOpenAI,
			SchemaCodec: openaicodec.New(openaicodec.Contract{
				ChatStructural: []openaicodec.StructuralRule{{
					Applies: gate,
					Apply:   func(map[string]any, string) []string { ruleCalls++; return nil },
				}},
			}),
		}, log: slog.Default()}
	}

	// Gate false → surgical sjson path: rule NOT invoked, model set, other
	// fields (temperature) preserved verbatim.
	out, _, _, err := adapterFor(func(string) bool { return false }).prepareBodyFull(reqFor(body, false))
	if err != nil {
		t.Fatal(err)
	}
	if ruleCalls != 0 {
		t.Fatalf("rule must NOT run when its gate is false; got %d", ruleCalls)
	}
	if !strings.Contains(string(out), `"model":"provider-real-model"`) {
		t.Fatalf("model not rewritten on fast path: %s", out)
	}
	if !strings.Contains(string(out), `"temperature":0.7`) {
		t.Fatalf("fast path must preserve other fields verbatim: %s", out)
	}

	// Gate true → decode door: rule invoked exactly once.
	ruleCalls = 0
	if _, _, _, err = adapterFor(func(string) bool { return true }).prepareBodyFull(reqFor(body, false)); err != nil {
		t.Fatal(err)
	}
	if ruleCalls != 1 {
		t.Fatalf("rule must run once when its gate matches; got %d", ruleCalls)
	}
}

// TestModelInBody_Anthropic_AliasRewritten is the core fix: an Anthropic
// /v1/messages native passthrough (non-OpenAI wire, non-OpenAI-family body)
// whose codec RewriteNative stamps the resolved model MUST rewrite the
// top-level `model` from the client-facing alias to the resolved
// ProviderModelID — otherwise the alias reaches Anthropic verbatim and 404s.
func TestModelInBody_Anthropic_AliasRewritten(t *testing.T) {
	body := []byte(`{"model":"my-fast-alias","messages":[{"role":"user","content":"hi"}],"nexus":{"ext":{"anthropic":{"topK":42}}}}`)
	out, rewrites, err := rpmAnthropic(anthropicReqFor(body))
	if err != nil {
		t.Fatal(err)
	}
	if rewrites != nil {
		t.Errorf("model rewrite is not a coercion; want nil rewrites, got %v", rewrites)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "claude-opus-4-8" {
		t.Errorf("model = %v, want claude-opus-4-8 (alias resolved to ProviderModelID)", m["model"])
	}
	// See TestEgress_NoInternalCarrierReachesTheTransport — the carrier leaves at
	// the transport frame, after the codec has consumed the extension.
	// The messages payload must survive untouched.
	if _, ok := m["messages"]; !ok {
		t.Error("messages array lost during model rewrite")
	}
}

// TestModelInBody_Anthropic_NoOpWhenEqual: when the client already sent the
// provider model name (no alias), the body must be returned unchanged — the
// GetBytes==ProviderModelID short-circuit avoids a wasted sjson rebuild on the
// hot path (nexus strip aside).
func TestModelInBody_Anthropic_NoOpWhenEqual(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)
	out, _, err := rpmAnthropic(anthropicReqFor(body))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("no-alias body must be returned byte-identical; got %s", out)
	}
}

// TestModelInBody_False_NonOpenAI_NoRewrite: a codec whose RewriteNative is
// verbatim leaves the body untouched even when ProviderModelID differs —
// this is the Gemini/Bedrock contract (model lives in the URL / is deleted
// from the body, applied by the transport/codec instead).
func TestModelInBody_False_NonOpenAI_NoRewrite(t *testing.T) {
	body := []byte(`{"model":"gemini-1.5-pro","contents":[]}`)
	req := Request{
		WireShape:  typology.WireShapeGeminiGenerateContent,
		BodyFormat: FormatGemini,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "gemini-2.0-flash"},
	}
	a := &specAdapter{spec: AdapterSpec{Format: FormatGemini, SchemaCodec: noopCodec{}}, log: slog.Default()}
	out, rw, _, err := a.prepareBodyFull(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) || rw != nil {
		t.Errorf("no-capability non-OpenAI body must pass through unchanged; got %s rewrites=%v", out, rw)
	}
}

// TestModelInBody_Anthropic_Idempotent: PrepareBody runs on both the cache-prep
// and execute paths, so the rewrite must be idempotent — a second run over the
// already-rewritten body is a no-op.
func TestModelInBody_Anthropic_Idempotent(t *testing.T) {
	body := []byte(`{"model":"my-fast-alias","messages":[]}`)
	first, _, err := rpmAnthropic(anthropicReqFor(body))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := rpmAnthropic(anthropicReqFor(first))
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Errorf("second rewrite must be a byte-identical no-op; first=%s second=%s", first, second)
	}
}

// TestPrepareBody_Anthropic_ModelInBody_Wired proves the model-in-body
// differential is codec-owned and reached through the public PrepareBody
// entry point (the executor + cache-prep path): an adapter whose codec's
// RewriteNative stamps (anthropic-style model-at-body-root) rewrites the
// body model; an adapter whose codec is verbatim (Gemini/Bedrock —
// model-in-URL) must NOT touch the body.
func TestPrepareBody_Anthropic_ModelInBody_Wired(t *testing.T) {
	spec := specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatAnthropic)
	ad := NewSpecAdapter(spec, nil)
	body := []byte(`{"model":"my-fast-alias","messages":[]}`)
	got, _, _, err := ad.PrepareBody(Request{
		WireShape:  typology.WireShapeAnthropicMessages,
		BodyFormat: FormatAnthropic,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "claude-opus-4-8"},
	})
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if !strings.Contains(string(got), `"model":"claude-opus-4-8"`) {
		t.Fatalf("capability spec must rewrite the Anthropic body model to ProviderModelID; got %s", got)
	}
	// A verbatim codec (model-in-URL wires) must NOT rewrite the body.
	specNoCap := specFrom(&fakeTransport{}, noopCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatAnthropic)
	adNoCap := NewSpecAdapter(specNoCap, nil)
	gotNoCap, _, _, err := adNoCap.PrepareBody(Request{
		WireShape:  typology.WireShapeAnthropicMessages,
		BodyFormat: FormatAnthropic,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "claude-opus-4-8"},
	})
	if err != nil {
		t.Fatalf("PrepareBody(no-cap): %v", err)
	}
	if string(gotNoCap) != string(body) {
		t.Fatalf("no-capability spec must pass the body through unchanged; got %s", gotNoCap)
	}
}

// TestModelInBody_Anthropic_DuplicateModel_MapPath: a pathological duplicate
// top-level model must still resolve to ProviderModelID with a single key (the
// map path collapses last-wins), matching the OpenAI dup-key guarantee.
func TestModelInBody_Anthropic_DuplicateModel_MapPath(t *testing.T) {
	body := []byte(`{"model":"alias-a","messages":[],"model":"alias-b"}`)
	out, _, err := rpmAnthropic(anthropicReqFor(body))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "claude-opus-4-8" {
		t.Errorf("model = %v, want claude-opus-4-8 (no stale duplicate)", m["model"])
	}
	if strings.Count(string(out), `"model"`) != 1 {
		t.Errorf("expected exactly one model key after rewrite, got %q", out)
	}
}
