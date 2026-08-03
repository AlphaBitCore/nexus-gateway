package codec

import (
	"encoding/json"
	"strings"
	"testing"
)

func inlineJSON(t *testing.T, in string) (map[string]any, error) {
	t.Helper()
	m, _, err := inlineJSONMode(t, in, false)
	return m, err
}

func inlineJSONMode(t *testing.T, in string, lenient bool) (map[string]any, []string, error) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("bad test input: %v", err)
	}
	out, dropped, err := inlineSchemaRefs(v, lenient)
	if err != nil {
		return nil, nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T", out)
	}
	return m, dropped, nil
}

// The mainstream Python shape: a Pydantic BaseModel with a nested BaseModel
// field. Before inlining, `addr` sanitized to {} — a typeless empty schema still
// named in `required` — and the Address definition was gone, so the model was
// handed a contract it could not satisfy as declared.
func TestInlineSchemaRefs_PydanticNestedModel(t *testing.T) {
	m, err := inlineJSON(t, `{
		"$defs":{"Address":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
		"type":"object",
		"properties":{"name":{"type":"string"},"addr":{"$ref":"#/$defs/Address"}},
		"required":["name","addr"]
	}`)
	if err != nil {
		t.Fatalf("must inline a resolvable ref, got %v", err)
	}
	if _, leftover := m["$defs"]; leftover {
		t.Error("$defs must be folded away, not handed to the sanitizer as an unknown key")
	}
	props := m["properties"].(map[string]any)
	addr, ok := props["addr"].(map[string]any)
	if !ok {
		t.Fatalf("addr must be an object, got %T", props["addr"])
	}
	if addr["type"] != "object" {
		t.Errorf("addr lost its type: %v", addr)
	}
	addrProps, ok := addr["properties"].(map[string]any)
	if !ok || addrProps["city"] == nil {
		t.Errorf("addr lost the Address definition's properties: %v", addr)
	}

	// And the sanitizer must now preserve it end to end.
	san, ok := sanitizeGeminiSchema(m).(map[string]any)
	if !ok {
		t.Fatal("sanitized schema must stay an object")
	}
	sanAddr := san["properties"].(map[string]any)["addr"].(map[string]any)
	if len(sanAddr) == 0 {
		t.Error("the required property is still empty after sanitization — the caller's contract is gone")
	}
}

// A $ref at the root sanitized to {}, which failed the codec's len>0 gate, so
// responseSchema was never set while responseMimeType still asked for JSON: the
// model returns arbitrary JSON, HTTP 200, no signal to the caller.
func TestInlineSchemaRefs_RootRef(t *testing.T) {
	m, err := inlineJSON(t, `{"$ref":"#/$defs/Root","$defs":{"Root":{"type":"object","properties":{"a":{"type":"string"}}}}}`)
	if err != nil {
		t.Fatalf("must inline a root ref, got %v", err)
	}
	san, ok := sanitizeGeminiSchema(m).(map[string]any)
	if !ok || len(san) == 0 {
		t.Fatalf("root schema sanitized to empty — responseSchema would be dropped: %v", san)
	}
	if san["type"] != "object" {
		t.Errorf("root lost its type: %v", san)
	}
}

// $ref siblings are the caller's overrides (draft 2020-12 allows them;
// generators use them for description), so they win over the definition.
func TestInlineSchemaRefs_SiblingsOverrideTheDefinition(t *testing.T) {
	m, err := inlineJSON(t, `{
		"$defs":{"S":{"type":"string","description":"from the definition"}},
		"type":"object",
		"properties":{"f":{"$ref":"#/$defs/S","description":"from the caller"}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	f := m["properties"].(map[string]any)["f"].(map[string]any)
	if f["type"] != "string" {
		t.Errorf("the definition's fields must be inlined: %v", f)
	}
	if f["description"] != "from the caller" {
		t.Errorf("a $ref sibling must win over the definition: %v", f)
	}
}

// draft-07 and many generators emit `definitions` rather than `$defs`.
func TestInlineSchemaRefs_Draft07Definitions(t *testing.T) {
	m, err := inlineJSON(t, `{"definitions":{"A":{"type":"integer"}},"type":"object","properties":{"n":{"$ref":"#/definitions/A"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, leftover := m["definitions"]; leftover {
		t.Error("definitions must be folded away too")
	}
	if m["properties"].(map[string]any)["n"].(map[string]any)["type"] != "integer" {
		t.Errorf("draft-07 definitions must resolve: %v", m)
	}
}

// A recursive model has no finite expansion and the Schema proto has no
// reference mechanism to defer to, so there is nothing correct to send. Failing
// loudly is the point: the alternative is a silently wrong contract.
func TestInlineSchemaRefs_RecursiveModelIsAnError(t *testing.T) {
	_, err := inlineJSON(t, `{
		"$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}},
		"$ref":"#/$defs/Node"
	}`)
	if err == nil {
		t.Fatal("a recursive model must fail loudly, not expand forever or degrade silently")
	}
	if !strings.Contains(err.Error(), "recursive") {
		t.Errorf("the error must name the cause: %v", err)
	}
}

func TestInlineSchemaRefs_UnresolvableRefsAreErrors(t *testing.T) {
	for name, in := range map[string]string{
		"missing definition": `{"type":"object","properties":{"a":{"$ref":"#/$defs/Nope"}}}`,
		"remote pointer":     `{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`,
		"openapi components": `{"type":"object","properties":{"a":{"$ref":"#/components/schemas/X"}}}`,
	} {
		if _, err := inlineJSON(t, in); err == nil {
			t.Errorf("%s: strict mode must error rather than guess", name)
		}
	}
}

// The observed prod shape (2026-07-17, proxy_error 400): a tool schema
// extracted from an OpenAPI document keeps an OpenAPI-style pointer whose
// components dictionary was never shipped. Lenient mode — tool parameters —
// degrades exactly that property to an open object, keeps the caller's
// description, and reports the reference; everything else is untouched.
func TestInlineSchemaRefs_Lenient_UnshippedNamespaceDegradesAndReports(t *testing.T) {
	m, dropped, err := inlineJSONMode(t, `{
		"type":"object",
		"properties":{
			"detector_type":{"type":"string"},
			"context":{"$ref":"#/components/schemas/handler_DryRunContext","description":"Optional routing context forwarded with the classify call."}
		},
		"required":["detector_type"]
	}`, true)
	if err != nil {
		t.Fatalf("lenient mode must not fail on an un-shipped namespace: %v", err)
	}
	props := m["properties"].(map[string]any)
	ctx := props["context"].(map[string]any)
	if ctx["type"] != "object" {
		t.Fatalf("degraded property must be an open object: %v", ctx)
	}
	if _, hasRef := ctx["$ref"]; hasRef {
		t.Fatalf("the unresolvable pointer must not survive to the sanitizer: %v", ctx)
	}
	if ctx["description"] != "Optional routing context forwarded with the classify call." {
		t.Fatalf("the caller's description must survive the degradation: %v", ctx)
	}
	if dt := props["detector_type"].(map[string]any); dt["type"] != "string" {
		t.Fatalf("sibling properties must be untouched: %v", dt)
	}
	if len(dropped) != 1 || dropped[0] != "#/components/schemas/handler_DryRunContext" {
		t.Fatalf("the degradation must be reported: %v", dropped)
	}
}

// Lenient mode also covers remote pointers (equally un-shipped) and nested
// occurrences, but never dangling pointers into a dictionary the document DOES
// use — those mean the schema itself is broken.
func TestInlineSchemaRefs_Lenient_Boundaries(t *testing.T) {
	t.Run("remote pointer degrades", func(t *testing.T) {
		m, dropped, err := inlineJSONMode(t,
			`{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`, true)
		if err != nil {
			t.Fatalf("remote pointers are the same un-shipped class: %v", err)
		}
		if a := m["properties"].(map[string]any)["a"].(map[string]any); a["type"] != "object" {
			t.Fatalf("degraded: %v", a)
		}
		if len(dropped) != 1 {
			t.Fatalf("reported: %v", dropped)
		}
	})
	t.Run("nested occurrence inside items degrades", func(t *testing.T) {
		m, dropped, err := inlineJSONMode(t,
			`{"type":"object","properties":{"list":{"type":"array","items":{"$ref":"#/components/schemas/Row"}}}}`, true)
		if err != nil {
			t.Fatal(err)
		}
		items := m["properties"].(map[string]any)["list"].(map[string]any)["items"].(map[string]any)
		if items["type"] != "object" {
			t.Fatalf("nested degradation: %v", items)
		}
		if len(dropped) != 1 || dropped[0] != "#/components/schemas/Row" {
			t.Fatalf("reported: %v", dropped)
		}
	})
	t.Run("dangling $defs pointer stays fatal", func(t *testing.T) {
		if _, _, err := inlineJSONMode(t,
			`{"$defs":{"Real":{"type":"string"}},"type":"object","properties":{"a":{"$ref":"#/$defs/Nope"}}}`, true); err == nil {
			t.Fatal("a dangling pointer into a shipped dictionary is a broken schema; lenient mode must not mask it")
		}
	})
	t.Run("dangling definitions pointer stays fatal without the dictionary", func(t *testing.T) {
		if _, _, err := inlineJSONMode(t,
			`{"type":"object","properties":{"a":{"$ref":"#/definitions/Nope"}}}`, true); err == nil {
			t.Fatal("the schema-native dictionary namespaces stay fatal even when absent — the author used the mechanism this file indexes")
		}
	})
	t.Run("resolvable refs still inline in lenient mode", func(t *testing.T) {
		m, dropped, err := inlineJSONMode(t,
			`{"$defs":{"City":{"type":"string"}},"type":"object","properties":{"city":{"$ref":"#/$defs/City"}}}`, true)
		if err != nil {
			t.Fatal(err)
		}
		if c := m["properties"].(map[string]any)["city"].(map[string]any); c["type"] != "string" {
			t.Fatalf("real inlining must still happen: %v", c)
		}
		if dropped != nil {
			t.Fatalf("nothing was degraded: %v", dropped)
		}
	})
	t.Run("recursion stays fatal in lenient mode", func(t *testing.T) {
		if _, _, err := inlineJSONMode(t, `{
			"$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}},
			"$ref":"#/$defs/Node"
		}`, true); err == nil {
			t.Fatal("recursive models have no finite expansion in any mode")
		}
	})
}

// A schema with no references at all must come through untouched — this runs on
// every Gemini request carrying tools, and the common case must not change.
func TestInlineSchemaRefs_RefLessSchemaIsUnchanged(t *testing.T) {
	in := `{"type":"object","properties":{"q":{"type":"string"},"n":{"type":"integer"}},"required":["q"]}`
	m, err := inlineJSON(t, in)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(m)
	var want, have any
	_ = json.Unmarshal([]byte(in), &want)
	_ = json.Unmarshal(got, &have)
	wantB, _ := json.Marshal(want)
	haveB, _ := json.Marshal(have)
	if string(wantB) != string(haveB) {
		t.Errorf("a ref-less schema must be unchanged\n want %s\n have %s", wantB, haveB)
	}
}

// The degradation keeps the caller's $ref siblings — the one part of the
// contract that DID ship — and only defaults type when the siblings did
// not state one.
func TestInlineSchemaRefs_Lenient_SiblingsSurviveDegradation(t *testing.T) {
	t.Run("sibling type wins over the object default", func(t *testing.T) {
		m, dropped, err := inlineJSONMode(t, `{
			"type":"object",
			"properties":{"a":{"$ref":"#/components/schemas/X","type":"string","description":"d"}}
		}`, true)
		if err != nil {
			t.Fatal(err)
		}
		a := m["properties"].(map[string]any)["a"].(map[string]any)
		if a["type"] != "string" {
			t.Fatalf("the caller's explicit sibling type must survive: %v", a)
		}
		if a["description"] != "d" {
			t.Fatalf("sibling description must survive: %v", a)
		}
		if len(dropped) != 1 {
			t.Fatalf("still reported: %v", dropped)
		}
	})
	t.Run("resolvable ref inside a kept sibling still inlines", func(t *testing.T) {
		m, dropped, err := inlineJSONMode(t, `{
			"$defs":{"City":{"type":"string"}},
			"type":"object",
			"properties":{"a":{"$ref":"#/components/schemas/X","properties":{"city":{"$ref":"#/$defs/City"}}}}
		}`, true)
		if err != nil {
			t.Fatal(err)
		}
		a := m["properties"].(map[string]any)["a"].(map[string]any)
		city := a["properties"].(map[string]any)["city"].(map[string]any)
		if city["type"] != "string" {
			t.Fatalf("nested resolvable ref must inline inside the kept sibling: %v", a)
		}
		if len(dropped) != 1 {
			t.Fatalf("only the un-shipped ref is reported: %v", dropped)
		}
	})
	t.Run("dangling $defs pointer inside a kept sibling stays fatal", func(t *testing.T) {
		if _, _, err := inlineJSONMode(t, `{
			"$defs":{"Real":{"type":"string"}},
			"type":"object",
			"properties":{"a":{"$ref":"#/components/schemas/X","properties":{"bad":{"$ref":"#/$defs/Nope"}}}}
		}`, true); err == nil {
			t.Fatal("shipped-dictionary refs stay fatal wherever they appear")
		}
	})
}
