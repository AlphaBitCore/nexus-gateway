package ingress_test

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/ingress"
)

// The Gemini ingress spells structured output `generationConfig.responseSchema`
// and it was not carried into the canonical body at all.
//
// Same class as the Anthropic Messages ingress, found by fixing that one: the
// generationConfig walk moved temperature, topP, topK, maxOutputTokens and
// stopSequences across and stopped there. A caller posting a responseSchema to
// `/v1beta/models/*:generateContent` had the schema dropped, so whatever target
// routing picked was asked for JSON with no shape — or, on a target that also
// never saw `responseMimeType`, asked for nothing at all.
//
// The canonical body IS an OpenAI body (§3a), so the schema becomes
// `response_format.json_schema` and carries the `name` OpenAI requires.
func TestGeminiRequest_responseSchemaBecomesResponseFormat(t *testing.T) {
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents": [{"role": "user", "parts": [{"text": "answer the schema"}]}],
		"generationConfig": {
			"responseMimeType": "application/json",
			"responseSchema": {
				"type": "OBJECT",
				"properties": {"respond": {"type": "BOOLEAN"}, "reason": {"type": "STRING"}},
				"required": ["respond", "reason"]
			}
		}
	}`), "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("GenerateContentRequestToOpenAIChatCompletion: %v", err)
	}

	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_schema" {
		t.Fatalf("response_format.type = %q, want json_schema; body: %s", got, out)
	}
	if gjson.GetBytes(out, "response_format.json_schema.name").String() == "" {
		t.Errorf("no json_schema.name — an OpenAI target answers 400 without it: %s", out)
	}
	schema := gjson.GetBytes(out, "response_format.json_schema.schema")
	if !schema.Get("properties.respond").Exists() || !schema.Get("properties.reason").Exists() {
		t.Errorf("schema properties lost in canonicalization: %s", out)
	}
	if len(schema.Get("required").Array()) != 2 {
		t.Errorf("required list lost: %s", schema.Raw)
	}
}

// `responseMimeType: application/json` WITHOUT a schema asks for "some JSON",
// which is the json_object case — every target either honours it or is
// instructed into it. Treating it as a schema constraint would narrow the
// routing pool for a requirement no model fails, and hand a codec a schema that
// is not there.
func TestGeminiRequest_jsonMimeTypeWithoutASchemaIsNotAConstraint(t *testing.T) {
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
		"generationConfig": {"responseMimeType": "application/json"}
	}`), "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("GenerateContentRequestToOpenAIChatCompletion: %v", err)
	}
	if got := gjson.GetBytes(out, "response_format.type").String(); got == "json_schema" {
		t.Errorf("a request with no schema became a json_schema constraint: %s", out)
	}
}

// No generationConfig at all must leave the canonical body alone.
func TestGeminiRequest_noResponseSchemaAddsNothing(t *testing.T) {
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`), "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("GenerateContentRequestToOpenAIChatCompletion: %v", err)
	}
	if gjson.GetBytes(out, "response_format").Exists() {
		t.Errorf("response_format invented from nothing: %s", out)
	}
}
