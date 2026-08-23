package dispatch

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestStripInternalCarriers_LeavesCallerAuthoredSchemasAlone is the regression
// for a defect this strip itself introduced and shipped.
//
// The reserved-prefix rule holds only over keys the GATEWAY writes. A tool
// schema and a json_schema response format are arbitrary JSON the CALLER wrote,
// where a key name means whatever their domain means — and a customer
// integrating a product called Nexus writes `nexus_id` as a matter of course.
// The first version of this walk descended into both and deleted the property
// while leaving `required:["nexus_id"]` pointing at nothing: an invalid schema,
// a model answering about a contract it never received, HTTP 200, and every
// byte-level assertion green. Exactly the silent-wrong-answer class the strip
// was written to prevent, produced by the strip.
func TestStripInternalCarriers_LeavesCallerAuthoredSchemasAlone(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":` +
		`{"type":"object","properties":{"nexus_id":{"type":"string"},"other":{"type":"string"}},` +
		`"required":["nexus_id"]}}}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` +
		`{"type":"object","properties":{"nexus":{"type":"string"}}}}}}`)

	out := stripInternalCarriers(in)
	if string(out) != string(in) {
		t.Fatalf("caller-authored schema was mutated\n in: %s\nout: %s", in, out)
	}
}

// The strip must still do its job on the keys the gateway does write, including
// when a caller schema sits in the same body.
func TestStripInternalCarriers_RemovesGatewayKeysBesideACallerSchema(t *testing.T) {
	in := []byte(`{"model":"m",` +
		`"nexus":{"ext":{"cohere":{"input_type":"search_document"}}},` +
		`"messages":[{"role":"assistant","content":"hi","reasoning_content":"why",` +
		`"nexus_thinking":[{"thinking":"why","signature":"s"}]}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":` +
		`{"type":"object","properties":{"nexus_id":{"type":"string"}}}}}]}`)

	out := stripInternalCarriers(in)

	if gjson.GetBytes(out, "nexus").Exists() {
		t.Error("root nexus namespace survived")
	}
	if gjson.GetBytes(out, "messages.0.nexus_thinking").Exists() {
		t.Error("per-message nexus_thinking survived")
	}
	if !gjson.GetBytes(out, "tools.0.function.parameters.properties.nexus_id").Exists() {
		t.Error("caller's nexus_id was deleted from a tool schema")
	}
	if gjson.GetBytes(out, "messages.0.reasoning_content").String() != "why" {
		t.Error("reasoning_content is canonical, not internal, and must survive")
	}
}

// The substring fast path must not change the answer, only the cost.
func TestStripInternalCarriers_NoCarrierIsByteIdentical(t *testing.T) {
	for _, in := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{}`,
		`not json at all`,
		``,
	} {
		if got := string(stripInternalCarriers([]byte(in))); got != in {
			t.Errorf("stripInternalCarriers(%q) = %q, want it untouched", in, got)
		}
	}
}

// A user message whose TEXT contains the reserved word triggers the walk but
// must not lose the text — the fast path is a cost optimisation, not a filter.
func TestStripInternalCarriers_ReservedWordInUserTextSurvives(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"what is \"nexus\" for?"}]}`)
	out := stripInternalCarriers(in)
	if gjson.GetBytes(out, "messages.0.content").String() != `what is "nexus" for?` {
		t.Errorf("user text was altered: %s", out)
	}
}

func TestStripInternalCarriers_RemovesThoughtSignatureLiteralAndUnicodeEscaped(t *testing.T) {
	for _, in := range []string{
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"thought_signature":"sig"}}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"\u0074\u0068\u006f\u0075\u0067\u0068\u0074\u005f\u0073\u0069\u0067\u006e\u0061\u0074\u0075\u0072\u0065":"sig"}}]}]}`,
	} {
		out := stripInternalCarriers([]byte(in))
		if gjson.GetBytes(out, "messages.0.tool_calls.0.function.thought_signature").Exists() {
			t.Fatalf("thought_signature survived: %s", out)
		}
	}
}
