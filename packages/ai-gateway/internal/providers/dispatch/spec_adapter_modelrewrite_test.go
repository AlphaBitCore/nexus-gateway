package dispatch

// The sjson fast path for the passthrough
// model rewrite must be behaviourally identical to the map[string]any round-trip
// it replaces, and must preserve the strict gate (nexus strip; fall back to the
// map path for streaming / per-adapter-rewrite / duplicate-top-level-model).

import (
	"github.com/goccy/go-json"
	"strings"
	"testing"

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

// TestA2_FastPath_SetsModelAndStripsNexus: non-stream, no rewrite → fast path.
func TestA2_FastPath_SetsModelAndStripsNexus(t *testing.T) {
	body := chatBody(`"nexus":{"ext":{"x":1}},"temperature":0.5`)
	out, rewrites, err := rewritePassthroughModel(reqFor(body, false), nil, nil)
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
	if _, ok := m["nexus"]; ok {
		t.Error("nexus namespace must be stripped on the fast path")
	}
	if m["temperature"] != 0.5 {
		t.Errorf("temperature lost: %v", m["temperature"])
	}
}

// TestA2_FastPath_EqualsMapPath: fast path output canonicalises identically to
// the old map round-trip for a normal body.
func TestA2_FastPath_EqualsMapPath(t *testing.T) {
	body := chatBody(`"temperature":0.5,"top_p":0.9`)
	fast, _, err := rewritePassthroughModel(reqFor(body, false), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Force the map path with a no-op rewrite callback.
	noop := func(m map[string]any, id string) []string { return nil }
	mapOut, _, err := rewritePassthroughModel(reqFor(body, false), noop, nil)
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
	out, _, err := rewritePassthroughModel(reqFor(body, false), nil, nil)
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
	out, rewrites, err := rewritePassthroughModel(reqFor(body, true), nil, nil)
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
	out, _, err := rewritePassthroughModel(reqFor(body, true), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("already-conformant streaming body must be returned byte-identical (zero rewrite):\n got=%s\nwant=%s", out, body)
	}
}

// TestA2_Streaming_ConformantButRewriteApplies_TakesMapPath: even a conformant
// body must NOT all-skip when a per-adapter rewrite applies — the rewrite has to
// run, so the map path is taken and the body is round-tripped.
func TestA2_Streaming_ConformantButRewriteApplies_TakesMapPath(t *testing.T) {
	body := []byte(`{"model":"provider-real-model","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	called := false
	rw := func(m map[string]any, id string) []string { called = true; return []string{"r"} }
	out, rewrites, err := rewritePassthroughModel(reqFor(body, true), rw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("adapter rewrite must run even for a conformant body (all-skip must not swallow it)")
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
		out, _, err := rewritePassthroughModel(reqFor([]byte(`{"model":"x"`), false), nil, nil)
		if err == nil && len(out) == 0 {
			t.Error("expected either a forwarded body or an error, got neither")
		}
	})
	t.Run("absent_model_added", func(t *testing.T) {
		out, _, err := rewritePassthroughModel(reqFor([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), false), nil, nil)
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
		out, _, err := rewritePassthroughModel(reqFor([]byte(`{"model":123,"messages":[]}`), false), nil, nil)
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
		out, _, err := rewritePassthroughModel(req, nil, nil)
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
	noop := func(m map[string]any, id string) []string { return nil }
	b.Run("fast_sjson", func(b *testing.B) {
		req := reqFor(body, false)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rewritePassthroughModel(req, nil, nil)
		}
	})
	b.Run("old_map_roundtrip", func(b *testing.B) {
		req := reqFor(body, false)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rewritePassthroughModel(req, noop, nil)
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
			_, _, _ = rewritePassthroughModel(req, nil, nil)
		}
	})
	b.Run("nonconformant_map_path", func(b *testing.B) {
		req := reqFor(rewrite, true)
		b.ReportAllocs()
		for range b.N {
			_, _, _ = rewritePassthroughModel(req, nil, nil)
		}
	})
}

// TestPassthroughRewriteApplies_GatesMapPath locks the behavior: a non-nil
// per-adapter rewrite no longer forces the (allocation-heavy) map round-trip
// when PassthroughRewriteApplies reports the rewrite is a no-op for this model.
func TestPassthroughRewriteApplies_GatesMapPath(t *testing.T) {
	body := chatBody(`"temperature":0.7`)
	rewriteCalls := 0
	rewrite := func(_ map[string]any, _ string) []string { rewriteCalls++; return nil }

	// Probe false → surgical sjson path: rewrite NOT invoked, model set, other
	// fields (temperature) preserved verbatim.
	out, _, err := rewritePassthroughModel(reqFor(body, false), rewrite, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if rewriteCalls != 0 {
		t.Fatalf("rewrite must NOT run when applies-probe is false; got %d", rewriteCalls)
	}
	if !strings.Contains(string(out), `"model":"provider-real-model"`) {
		t.Fatalf("model not rewritten on fast path: %s", out)
	}
	if !strings.Contains(string(out), `"temperature":0.7`) {
		t.Fatalf("fast path must preserve other fields verbatim: %s", out)
	}

	// Probe true → map path: rewrite invoked.
	rewriteCalls = 0
	if _, _, err = rewritePassthroughModel(reqFor(body, false), rewrite, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if rewriteCalls != 1 {
		t.Fatalf("rewrite must run when applies-probe is true; got %d", rewriteCalls)
	}

	// Nil probe ⇒ conservative map path (prior behavior).
	rewriteCalls = 0
	if _, _, err = rewritePassthroughModel(reqFor(body, false), rewrite, nil); err != nil {
		t.Fatal(err)
	}
	if rewriteCalls != 1 {
		t.Fatalf("nil probe must keep the map path; got %d", rewriteCalls)
	}
}
