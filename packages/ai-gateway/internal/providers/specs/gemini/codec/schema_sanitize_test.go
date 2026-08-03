// Schema sanitization coverage: Gemini's proto-backed Schema rejects any
// unknown key with a whole-request 400 (`Unknown name "<key>" ... Cannot
// find field`), so EncodeRequest must forward only the live-probed accept
// set and convert/drop the rest. Named failure modes:
//   - additionalProperties / $comment / $schema / $defs / $ref forwarded → upstream 400
//   - OpenAI response_format json_schema envelope (name/strict) forwarded → upstream 400
//   - type:["T","null"] forwarded as a list → proto "field is not repeating" 400
//   - unknown future keys must be dropped, never fail the request
package codec_test

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	gemcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func encodeChatBody(t *testing.T, body string) []byte {
	t.Helper()
	var c gemcodec.Codec
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, []byte(body), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return encRes.Body
}

func toolBody(params string) string {
	return `{"model":"g","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"fn","description":"d","parameters":` + params + `}}]}`
}

const declPath = "tools.0.functionDeclarations.0.parameters"

func TestEncodeRequest_toolParameters_stripsGeminiUnknownSchemaKeys(t *testing.T) {
	// The exact shape observed failing upstream: additionalProperties at the
	// schema root and nested inside a property, plus $comment.
	out := encodeChatBody(t, toolBody(`{
		"$comment":"internal note",
		"$schema":"http://json-schema.org/draft-07/schema#",
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"city":{"type":"string","description":"city name"},
			"opts":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}
		},
		"required":["city"]
	}`))

	for _, gone := range []string{
		declPath + ".additionalProperties",
		declPath + ".$comment",
		declPath + ".$schema",
		declPath + ".properties.opts.additionalProperties",
	} {
		if gjson.GetBytes(out, gone).Exists() {
			t.Errorf("%s must be stripped, body: %s", gone, out)
		}
	}
	if got := gjson.GetBytes(out, declPath+".properties.city.type").String(); got != "string" {
		t.Errorf("supported property type lost: %q", got)
	}
	if got := gjson.GetBytes(out, declPath+".properties.city.description").String(); got != "city name" {
		t.Errorf("supported description lost: %q", got)
	}
	if got := gjson.GetBytes(out, declPath+".required.0").String(); got != "city" {
		t.Errorf("required lost: %q", got)
	}
	if got := gjson.GetBytes(out, declPath+".properties.opts.properties.a.type").String(); got != "string" {
		t.Errorf("nested supported property lost: %q", got)
	}
}

func TestEncodeRequest_toolParameters_convertsSchemaKeywords(t *testing.T) {
	out := encodeChatBody(t, toolBody(`{
		"type":"object",
		"properties":{
			"city":{"type":["string","null"],"const":"beijing","examples":["beijing","shanghai"]},
			"n":{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":10}
		}
	}`))

	city := declPath + ".properties.city"
	if got := gjson.GetBytes(out, city+".type").String(); got != "string" {
		t.Errorf("type union not collapsed: %q", got)
	}
	if !gjson.GetBytes(out, city+".nullable").Bool() {
		t.Errorf("nullable not derived from type union: %s", gjson.GetBytes(out, city).Raw)
	}
	if got := gjson.GetBytes(out, city+".enum.0").String(); got != "beijing" {
		t.Errorf("const not converted to enum: %q", got)
	}
	if got := gjson.GetBytes(out, city+".example").String(); got != "beijing" {
		t.Errorf("examples not converted to singular example: %q", got)
	}
	if gjson.GetBytes(out, city+".const").Exists() || gjson.GetBytes(out, city+".examples").Exists() {
		t.Errorf("original const/examples keys must not survive: %s", gjson.GetBytes(out, city).Raw)
	}
	n := declPath + ".properties.n"
	if got := gjson.GetBytes(out, n+".minimum").Float(); got != 0 || !gjson.GetBytes(out, n+".minimum").Exists() {
		t.Errorf("exclusiveMinimum not converted: %s", gjson.GetBytes(out, n).Raw)
	}
	if got := gjson.GetBytes(out, n+".maximum").Float(); got != 10 {
		t.Errorf("exclusiveMaximum not converted: %v", got)
	}
	if gjson.GetBytes(out, n+".exclusiveMinimum").Exists() || gjson.GetBytes(out, n+".exclusiveMaximum").Exists() {
		t.Errorf("exclusive bounds must not survive: %s", gjson.GetBytes(out, n).Raw)
	}
}

// Gemini 400s on an empty-string entry in an enum ("...enum[N]: cannot be
// empty", observed on gemini-2.5-pro from OpenAPI-generated tool schemas).
// The sanitizer drops empty entries, keeps the rest, and drops the enum key
// entirely when nothing survives.
func TestEncodeRequest_toolParameters_dropsEmptyEnumEntries(t *testing.T) {
	out := encodeChatBody(t, toolBody(`{
		"type":"object",
		"properties":{
			"level":{"type":"string","enum":["low","high",""]},
			"mode":{"type":"string","enum":[""]}
		}
	}`))

	level := declPath + ".properties.level"
	if got := gjson.GetBytes(out, level+".enum.#").Int(); got != 2 {
		t.Errorf("empty enum entry not dropped: %s", gjson.GetBytes(out, level+".enum").Raw)
	}
	for _, v := range gjson.GetBytes(out, level+".enum").Array() {
		if v.String() == "" {
			t.Errorf("empty string survived in enum: %s", gjson.GetBytes(out, level+".enum").Raw)
		}
	}
	// An enum that is empty after cleaning drops the key — Gemini rejects an
	// empty enum too, and no constraint beats a rejected request.
	if gjson.GetBytes(out, declPath+".properties.mode.enum").Exists() {
		t.Errorf("all-empty enum must be dropped entirely: %s", gjson.GetBytes(out, declPath+".properties.mode").Raw)
	}
}

func TestEncodeRequest_toolParameters_conversionsYieldToExplicitKeys(t *testing.T) {
	// When the caller already supplies enum/example/minimum, the converted
	// variants must not overwrite them.
	out := encodeChatBody(t, toolBody(`{
		"type":"object",
		"properties":{
			"c":{"type":"string","const":"x","enum":["a","b"],"examples":["e1"],"example":"e0"},
			"n":{"type":"number","exclusiveMinimum":5,"minimum":1}
		}
	}`))

	c := declPath + ".properties.c"
	if got := gjson.GetBytes(out, c+".enum.#").Int(); got != 2 {
		t.Errorf("explicit enum overwritten by const: %s", gjson.GetBytes(out, c+".enum").Raw)
	}
	if got := gjson.GetBytes(out, c+".example").String(); got != "e0" {
		t.Errorf("explicit example overwritten by examples: %q", got)
	}
	if got := gjson.GetBytes(out, declPath+".properties.n.minimum").Float(); got != 1 {
		t.Errorf("explicit minimum overwritten by exclusiveMinimum: %v", got)
	}
}

func TestEncodeRequest_toolParameters_recursesNestedSchemas(t *testing.T) {
	out := encodeChatBody(t, toolBody(`{
		"type":"object",
		"properties":{
			"tags":{"type":"array","items":{"type":"string","$comment":"drop me"},"uniqueItems":true},
			"tuple":{"type":"array","items":[{"type":"string","readOnly":true},{"type":"number"}]},
			"choice":{"anyOf":[{"type":"string","multipleOf":2},{"type":"number","deprecated":true}]},
			"one":{"oneOf":[{"type":"string","patternProperties":{"^x":{"type":"string"}}}]},
			"all":{"allOf":[{"type":"object","contains":{"type":"string"}}]}
		}
	}`))

	p := declPath + ".properties"
	checks := []struct{ path, wantAbsent string }{
		{p + ".tags.items", "$comment"},
		{p + ".tags", "uniqueItems"},
		{p + ".tuple.items", "readOnly"},
		{p + ".choice.anyOf.0", "multipleOf"},
		{p + ".choice.anyOf.1", "deprecated"},
		{p + ".one.oneOf.0", "patternProperties"},
		{p + ".all.allOf.0", "contains"},
	}
	for _, c := range checks {
		if gjson.GetBytes(out, c.path+"."+c.wantAbsent).Exists() {
			t.Errorf("%s.%s must be stripped: %s", c.path, c.wantAbsent, gjson.GetBytes(out, c.path).Raw)
		}
	}
	// Tuple-form items collapses to the first element's schema.
	if got := gjson.GetBytes(out, p+".tuple.items.type").String(); got != "string" {
		t.Errorf("tuple items not collapsed to first schema: %s", gjson.GetBytes(out, p+".tuple.items").Raw)
	}
	if got := gjson.GetBytes(out, p+".choice.anyOf.#").Int(); got != 2 {
		t.Errorf("anyOf arity changed: %d", got)
	}
	if got := gjson.GetBytes(out, p+".tags.items.type").String(); got != "string" {
		t.Errorf("items type lost: %q", got)
	}
}

func TestEncodeRequest_toolParameters_unknownFutureKeyStripped(t *testing.T) {
	// Forward-compatibility contract: ANY unrecognized key is dropped rather
	// than forwarded, so novel caller keywords can never 400 the request.
	out := encodeChatBody(t, toolBody(`{
		"type":"object",
		"x-vendor-extension":{"weird":true},
		"properties":{"a":{"type":"string","someFutureKeyword":123}}
	}`))

	if gjson.GetBytes(out, declPath+".x-vendor-extension").Exists() {
		t.Errorf("unknown top-level key forwarded: %s", out)
	}
	if gjson.GetBytes(out, declPath+".properties.a.someFutureKeyword").Exists() {
		t.Errorf("unknown nested key forwarded: %s", out)
	}
	if got := gjson.GetBytes(out, declPath+".properties.a.type").String(); got != "string" {
		t.Errorf("supported key lost: %q", got)
	}
}

func TestEncodeRequest_toolParameters_onlyUnsupportedKeys_defaultsToObjectSchema(t *testing.T) {
	// A schema carrying nothing the proto accepts sanitizes to nothing and falls
	// back to the same default used when parameters are absent. ($ref is no
	// longer an example of this: it is resolved against $defs before
	// sanitization, because dropping it silently voids the caller's contract.)
	out := encodeChatBody(t, toolBody(`{"$comment":"notes for humans"}`))

	if got := gjson.GetBytes(out, declPath+".type").String(); got != "object" {
		t.Errorf("expected default object schema, got: %s", gjson.GetBytes(out, declPath).Raw)
	}
	if gjson.GetBytes(out, declPath+".$comment").Exists() {
		t.Errorf("$comment forwarded: %s", out)
	}
}

func TestEncodeRequest_responseFormat_jsonSchema_unwrapsEnvelopeAndSanitizes(t *testing.T) {
	out := encodeChatBody(t, `{"model":"g","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{
			"name":"result","strict":true,
			"schema":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string","$comment":"x"}}}
		}}}`)

	rs := "generationConfig.responseSchema"
	if got := gjson.GetBytes(out, rs+".type").String(); got != "object" {
		t.Fatalf("responseSchema not unwrapped to inner schema: %s", gjson.GetBytes(out, rs).Raw)
	}
	for _, gone := range []string{rs + ".name", rs + ".strict", rs + ".schema", rs + ".additionalProperties", rs + ".properties.a.$comment"} {
		if gjson.GetBytes(out, gone).Exists() {
			t.Errorf("%s must not reach the wire: %s", gone, gjson.GetBytes(out, rs).Raw)
		}
	}
	if got := gjson.GetBytes(out, rs+".properties.a.type").String(); got != "string" {
		t.Errorf("inner schema property lost: %q", got)
	}
	if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
		t.Errorf("responseMimeType: %q", got)
	}
}

func TestEncodeRequest_responseFormat_jsonSchema_emptyAfterSanitize_omitsResponseSchema(t *testing.T) {
	// An envelope whose inner schema carries no proto-expressible content at all
	// degrades to plain JSON mode instead of sending an empty proto schema —
	// there is no contract to lose. ($ref is NOT such a case: it names a real
	// contract, so it is resolved before sanitization rather than dropped.)
	out := encodeChatBody(t, `{"model":"g","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"$comment":"notes"}}}}`)

	if gjson.GetBytes(out, "generationConfig.responseSchema").Exists() {
		t.Errorf("empty responseSchema must be omitted: %s", out)
	}
	if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
		t.Errorf("responseMimeType: %q", got)
	}
}

// A $ref that resolves to nothing is a broken schema. Before references were
// inlined it sanitized away and the function silently shipped with no arguments;
// now it fails where the caller can see it, which is what the vendor itself used
// to do (400 Unknown name "$ref").
func TestEncodeRequest_unresolvableRefIsAnError(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.EncodeRequest(
		typology.WireShapeGeminiGenerateContent,
		[]byte(toolBody(`{"type":"object","properties":{"a":{"$ref":"#/$defs/Gone"}}}`)),
		provcore.CallTarget{},
	)
	if err == nil {
		t.Fatal("a $ref resolving to nothing must fail the request, not silently strip the argument")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("the error must name the cause: %v", err)
	}
}

// The schema pipeline memoizes its result across turns, and an agent resends
// the same declaration on every one of them. So the turn that rings the bell
// must not be only the first: if reuse ever answers a rejected schema with the
// empty schema it sanitizes to, the caller gets one error and then a silent
// wrong answer for the rest of the conversation — which is the defect itself,
// merely delayed by a turn.
func TestEncodeRequest_unresolvableRefFailsOnEveryTurn(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(toolBody(`{"type":"object","properties":{"a":{"$ref":"#/$defs/Gone"}}}`))

	for turn := 1; turn <= 3; turn++ {
		if _, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{}); err == nil {
			t.Fatalf("turn %d: the request must keep failing, not resolve to a cached empty schema", turn)
		}
	}
}

// The same guard on the response_format path, whose failure is the worse of the
// two: an omitted responseSchema still leaves responseMimeType asking for JSON,
// so the caller gets arbitrary JSON with a 200 and no way to detect it.
func TestEncodeRequest_responseFormat_unresolvableRefFailsOnEveryTurn(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"$ref":"#/$defs/Gone"}}}}`)

	for turn := 1; turn <= 3; turn++ {
		if _, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{}); err == nil {
			t.Fatalf("turn %d: the request must keep failing, not degrade to schemaless JSON mode", turn)
		}
	}
}

// The mainstream Pydantic shape must survive end to end: nested model inlined,
// $defs folded away, no reference left on the wire.
func TestEncodeRequest_nestedModelReachesTheWire(t *testing.T) {
	out := encodeChatBody(t, toolBody(`{"$defs":{"Address":{"type":"object","properties":{"city":{"type":"string"}}}},"type":"object","properties":{"addr":{"$ref":"#/$defs/Address"}},"required":["addr"]}`))
	if got := gjson.GetBytes(out, declPath+".properties.addr.properties.city.type").String(); got != "string" {
		t.Errorf("the nested model must survive to the wire, got: %s", gjson.GetBytes(out, declPath).Raw)
	}
	if gjson.GetBytes(out, declPath+".$defs").Exists() {
		t.Errorf("$defs must not reach the wire: %s", out)
	}
	if gjson.GetBytes(out, declPath+".properties.addr.$ref").Exists() {
		t.Errorf("$ref must not reach the wire: %s", out)
	}
}
