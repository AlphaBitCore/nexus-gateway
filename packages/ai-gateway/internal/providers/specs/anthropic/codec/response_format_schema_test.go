package codec

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Structured outputs are the one thing the caller asks for that our Anthropic
// adapter refused while OpenAI, Gemini and Cohere all served it. Probed on each
// provider's own wire on 2026-08-19: OpenAI response_format.json_schema with
// strict:true, Gemini generationConfig.responseSchema, Cohere response_format,
// and Anthropic output_config.format — all four returned 200 with JSON matching
// the schema. Anthropic's own 400 named the field:
//
//	"output_format: This field is deprecated. Use 'output_config.format'
//	 instead."
//
// and plain `response_format` answered "Extra inputs are not permitted", so the
// OpenAI spelling cannot be forwarded as-is.
const canonJSONSchema = `{
	"model": "claude-sonnet-4-6",
	"max_tokens": 64,
	"messages": [{"role": "user", "content": "should I respond?"}],
	"response_format": {
		"type": "json_schema",
		"json_schema": {
			"name": "verdict",
			"strict": true,
			"schema": {
				"type": "object",
				"properties": {"respond": {"type": "boolean"}, "reason": {"type": "string"}},
				"required": ["respond", "reason"],
				"additionalProperties": false
			}
		}
	}
}`

func encodeAnthropic(t *testing.T, canon string) []byte {
	t.Helper()
	var c Codec
	res, err := c.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(canon), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return res.Body
}

// The refusal is gone and the schema reaches the wire under the name Anthropic
// accepts. Before this, a caller who moved a working OpenAI structured-output
// request onto a Claude model got a hard 400 from us — never from Anthropic.
func TestEncodeRequest_jsonSchemaBecomesOutputConfigFormat(t *testing.T) {
	out := encodeAnthropic(t, canonJSONSchema)

	if got := gjson.GetBytes(out, "output_config.format.type").String(); got != "json_schema" {
		t.Errorf("output_config.format.type = %q, want json_schema; body: %s", got, out)
	}
	// The schema must be the BARE schema, not OpenAI's {name,strict,schema}
	// envelope — the envelope keys are not part of Anthropic's field and the
	// whole request fails if they ride along.
	schema := gjson.GetBytes(out, "output_config.format.schema")
	if !schema.Exists() || !schema.IsObject() {
		t.Fatalf("output_config.format.schema missing or not an object: %s", out)
	}
	if schema.Get("name").Exists() || schema.Get("strict").Exists() {
		t.Errorf("the OpenAI envelope leaked into the schema: %s", schema.Raw)
	}
	if schema.Get("type").String() != "object" {
		t.Errorf("schema.type = %q, want object", schema.Get("type").String())
	}
	if !schema.Get("properties.respond").Exists() || !schema.Get("properties.reason").Exists() {
		t.Errorf("schema properties lost in translation: %s", schema.Raw)
	}
	if schema.Get("additionalProperties").Type != gjson.False {
		t.Errorf("additionalProperties dropped; Anthropic accepted it in the probe: %s", schema.Raw)
	}

	// The OpenAI spelling must NOT be forwarded: Anthropic answers
	// "response_format: Extra inputs are not permitted".
	if gjson.GetBytes(out, "response_format").Exists() {
		t.Errorf("response_format forwarded verbatim; Anthropic rejects the field: %s", out)
	}
	// And a schema request must not silently fall back to the json_object
	// system instruction, which asks for JSON without holding it to anything.
	if sys := gjson.GetBytes(out, "system").String(); sys != "" {
		t.Errorf("json_schema must not add the json_object system instruction, got %q", sys)
	}
}

// A schema given directly, without OpenAI's envelope, is still a schema.
func TestEncodeRequest_jsonSchemaWithoutEnvelope(t *testing.T) {
	out := encodeAnthropic(t, `{
		"model": "claude-sonnet-4-6", "max_tokens": 16,
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {"type": "json_schema",
			"json_schema": {"type": "object", "properties": {"a": {"type": "string"}}}}
	}`)
	if got := gjson.GetBytes(out, "output_config.format.schema.properties.a.type").String(); got != "string" {
		t.Errorf("bare schema not carried: %s", out)
	}
}

// json_schema with no schema at all still asks for JSON — the caller's intent
// is unambiguous — but must not emit an empty `schema` key, which Anthropic
// would read as a schema permitting nothing.
func TestEncodeRequest_jsonSchemaWithoutSchemaAsksForJSONOnly(t *testing.T) {
	out := encodeAnthropic(t, `{
		"model": "claude-sonnet-4-6", "max_tokens": 16,
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {"type": "json_schema"}
	}`)
	if got := gjson.GetBytes(out, "output_config.format.type").String(); got != "json_schema" {
		t.Errorf("output_config.format.type = %q: %s", got, out)
	}
	if gjson.GetBytes(out, "output_config.format.schema").Exists() {
		t.Errorf("an absent schema must not become an empty one: %s", out)
	}
}

// An EMPTY json_schema envelope is the same fact as an absent one, and the
// branch that handles it is different code — a mutant that emitted `schema: {}`
// survived until this case existed. `{}` is not "no constraint" to a wire that
// reads it as a schema; it is a contract the caller never wrote.
func TestEncodeRequest_emptyJSONSchemaEmitsNoSchemaKey(t *testing.T) {
	for _, canon := range []string{
		`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}],
		  "response_format":{"type":"json_schema","json_schema":{}}}`,
		`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}],
		  "response_format":{"type":"json_schema","json_schema":{"name":"x","strict":true,"schema":{}}}}`,
	} {
		out := encodeAnthropic(t, canon)
		if got := gjson.GetBytes(out, "output_config.format.type").String(); got != "json_schema" {
			t.Errorf("output_config.format.type = %q: %s", got, out)
		}
		if s := gjson.GetBytes(out, "output_config.format.schema"); s.Exists() {
			t.Errorf("an empty schema envelope must not become schema:%s — %s", s.Raw, out)
		}
	}
}

// Two of the three payloads captured for this class carry a `$schema` key
// (`http://json-schema.org/draft-07/schema#`) — every schema a JSON-Schema
// library emits does. The synthetic cases above did not, so nothing here
// covered forwarding it until the ORIGINAL bodies were replayed byte-for-byte
// against prod: all three returned 200 with a conforming object, so Anthropic
// tolerates the key and stripping it would be us inventing a limit the wire
// does not have. Pinned so a future "sanitize the schema" change has to face
// the evidence rather than quietly drop it.
func TestEncodeRequest_jsonSchemaKeepsDollarSchema(t *testing.T) {
	out := encodeAnthropic(t, `{
		"model":"claude-haiku-4-5-20251001",
		"messages":[{"role":"user","content":"Return JSON"}],
		"response_format":{"type":"json_schema","json_schema":{
			"name":"response","strict":true,
			"schema":{"$schema":"http://json-schema.org/draft-07/schema#",
				"type":"object",
				"properties":{"ok":{"type":"boolean"},"word":{"type":"string"}},
				"required":["ok","word"],"additionalProperties":false}}}
	}`)
	schema := gjson.GetBytes(out, "output_config.format.schema")
	if got := schema.Get(`\$schema`).String(); got != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("$schema dropped from the forwarded schema: %s", schema.Raw)
	}
	if !schema.Get("properties.ok").Exists() || !schema.Get("properties.word").Exists() {
		t.Errorf("properties lost alongside $schema: %s", schema.Raw)
	}
}

// json_object keeps its existing behaviour — Anthropic has no json_object mode,
// so the system instruction stays. Pinning it here so the json_schema wiring
// cannot quietly take that path over.
func TestEncodeRequest_jsonObjectStillUsesTheSystemInstruction(t *testing.T) {
	out := encodeAnthropic(t, `{
		"model": "claude-sonnet-4-6", "max_tokens": 16,
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {"type": "json_object"}
	}`)
	if gjson.GetBytes(out, "output_config").Exists() {
		t.Errorf("json_object must not set output_config: %s", out)
	}
	if sys := gjson.GetBytes(out, "system").String(); sys == "" {
		t.Errorf("json_object lost its system instruction: %s", out)
	}
}

// The envelope with NO schema inside it, which is the case the bare-schema
// fallback swallows.
//
// `node = js` exists so a caller who puts the schema directly under
// `json_schema` is served (the test above). But when the envelope is present
// and its `schema` key is NOT, that same fallback marshals the WRAPPER — the
// OpenAI keys `name` and `strict` — and ships it as Anthropic's schema.
//
// Probed 2026-08-19 with exactly that body: 400 "output_config.format.schema:
// Invalid schema: Schema type is missing for schema: {'name': 'verdict',
// 'strict': True}". The upstream is quoting the caller's own envelope keys back
// at them under a field name they never wrote — an error that reads like their
// mistake and is ours.
//
// The discriminator is what the value looks like, not where it came from: a
// JSON Schema declares at least one schema keyword, and an envelope declares
// none of them.
func TestEncodeRequest_jsonSchemaEnvelopeWithoutASchemaIsNotShippedAsOne(t *testing.T) {
	out, rewrites := encodeAnthropicFull(t, `{
		"model": "claude-sonnet-4-6", "max_tokens": 16,
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {"type": "json_schema",
			"json_schema": {"name": "verdict", "strict": true}}
	}`)

	if s := gjson.GetBytes(out, "output_config.format.schema"); s.Exists() {
		t.Errorf("the OpenAI envelope was shipped as the schema: %s\n"+
			"  Anthropic answers 400 quoting these very keys back at the caller", s.Raw)
	}
	if got := gjson.GetBytes(out, "output_config.format.type").String(); got != "json_schema" {
		t.Errorf("the caller still asked for json_schema; type = %q", got)
	}
	var announced bool
	for _, r := range rewrites {
		if strings.Contains(r, "no schema") {
			announced = true
		}
	}
	if !announced {
		t.Errorf("nothing carried and x-nexus-coerced is silent: %v", rewrites)
	}
}

// The counter-cases, so the discriminator cannot become "reject everything".
// Each of these IS a schema and must still be carried whole.
func TestEncodeRequest_bareSchemasAreStillRecognised(t *testing.T) {
	cases := map[string]string{
		"a plain object schema": `{"type": "object", "properties": {"a": {"type": "string"}}}`,
		// Two of the three real payloads in the replay corpus carry $schema —
		// draft-07 — a key no synthetic fixture had until they were replayed.
		"a draft-07 schema": `{"$schema": "http://json-schema.org/draft-07/schema#",
			"type": "object", "properties": {"a": {"type": "string"}}}`,
		"a composed schema with no top-level type": `{"anyOf": [{"type": "string"}, {"type": "number"}]}`,
		"an enum": `{"enum": ["yes", "no"]}`,
		"a $ref":  `{"$ref": "#/definitions/verdict"}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			out := encodeAnthropic(t, `{
				"model": "claude-sonnet-4-6", "max_tokens": 16,
				"messages": [{"role": "user", "content": "hi"}],
				"response_format": {"type": "json_schema", "json_schema": `+schema+`}
			}`)
			got := gjson.GetBytes(out, "output_config.format.schema")
			if !got.Exists() {
				t.Fatalf("a real schema was rejected as an envelope: %s", schema)
			}
			if got.Get("name").Exists() || got.Get("strict").Exists() {
				t.Errorf("envelope keys present in a bare schema: %s", got.Raw)
			}
		})
	}
}
