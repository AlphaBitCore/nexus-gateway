package ingress_test

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
)

// A caller on the Anthropic Messages ingress spells structured output
// `output_config.format`, and the canonical body has to carry it.
//
// Found by a live probe against prod, not by reading: `/v1/messages` with
// `model: auto` and an `output_config.format` schema was routed to
// gpt-5.6-terra — the routing dimension had done its job and picked a model
// that honours schemas — and came back with `{"should_respond": true,
// "probe_id": "..."}`. Valid JSON, wrong keys: the model was told to produce
// JSON and never told the shape, because the ingress dropped the schema on the
// way into the canonical body.
//
// That is the sharper half of the same failure the routing dimension exists to
// prevent, and shipping only the routing half made it worse in one respect: the
// pool is now narrowed to schema-capable models for a request whose schema is
// discarded anyway.
//
// The canonical spelling is OpenAI's, per §3a — canonical IS the OpenAI shape —
// so a request that lands back on an Anthropic target round-trips through the
// codec into `output_config.format` again.
func TestMessagesRequest_outputConfigFormatBecomesResponseFormat(t *testing.T) {
	out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model": "claude-opus-5",
		"max_tokens": 400,
		"messages": [{"role": "user", "content": "answer the schema"}],
		"output_config": {"format": {"type": "json_schema", "schema": {
			"type": "object",
			"properties": {"respond": {"type": "boolean"}, "reason": {"type": "string"}},
			"required": ["respond", "reason"],
			"additionalProperties": false
		}}}
	}`), "")
	if err != nil {
		t.Fatalf("MessagesRequestToOpenAIChatCompletion: %v", err)
	}

	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_schema" {
		t.Fatalf("response_format.type = %q, want json_schema; body: %s", got, out)
	}
	schema := gjson.GetBytes(out, "response_format.json_schema.schema")
	if !schema.Exists() || !schema.IsObject() {
		t.Fatalf("the schema did not survive canonicalization: %s", out)
	}
	// The schema's CONTENT, not just its presence: a translation that carries an
	// empty object satisfies "exists" and constrains nothing, which is the same
	// outcome as dropping it.
	if !schema.Get("properties.respond").Exists() || !schema.Get("properties.reason").Exists() {
		t.Errorf("schema properties lost: %s", schema.Raw)
	}
	if schema.Get("additionalProperties").Type != gjson.False {
		t.Errorf("additionalProperties dropped: %s", schema.Raw)
	}
	if len(schema.Get("required").Array()) != 2 {
		t.Errorf("required list lost: %s", schema.Raw)
	}
}

// Anthropic's `output_config` also carries `effort`, which is NOT a response
// format and must not be mistaken for one. A body with effort and no format
// asks for no schema at all, and inventing a `response_format` for it would
// narrow the routing pool for a constraint the caller never wrote.
func TestMessagesRequest_outputConfigEffortAloneIsNotAFormat(t *testing.T) {
	out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model": "claude-opus-5",
		"max_tokens": 400,
		"messages": [{"role": "user", "content": "think about it"}],
		"output_config": {"effort": "high"}
	}`), "")
	if err != nil {
		t.Fatalf("MessagesRequestToOpenAIChatCompletion: %v", err)
	}
	if gjson.GetBytes(out, "response_format").Exists() {
		t.Errorf("a caller who asked for no schema got one: %s", out)
	}
}

// A format with no schema inside it is the shape Anthropic itself rejects
// ("output_config.format.schema: Field required"). Carrying the type alone
// keeps the caller's intent visible to routing without fabricating a schema
// they did not send.
func TestMessagesRequest_outputConfigFormatWithoutASchema(t *testing.T) {
	out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model": "claude-opus-5",
		"max_tokens": 400,
		"messages": [{"role": "user", "content": "hi"}],
		"output_config": {"format": {"type": "json_schema"}}
	}`), "")
	if err != nil {
		t.Fatalf("MessagesRequestToOpenAIChatCompletion: %v", err)
	}
	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_schema" {
		t.Errorf("response_format.type = %q, want the caller's intent preserved: %s", got, out)
	}
	if gjson.GetBytes(out, "response_format.json_schema.schema").Exists() {
		t.Errorf("a schema was invented for a caller who sent none: %s", out)
	}
}

// No output_config at all must leave the canonical body alone.
func TestMessagesRequest_noOutputConfigAddsNothing(t *testing.T) {
	out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model": "claude-opus-5",
		"max_tokens": 400,
		"messages": [{"role": "user", "content": "hi"}]
	}`), "")
	if err != nil {
		t.Fatalf("MessagesRequestToOpenAIChatCompletion: %v", err)
	}
	if gjson.GetBytes(out, "response_format").Exists() {
		t.Errorf("response_format invented from nothing: %s", out)
	}
}

// A format whose type is NOT json_schema must not become one.
//
// The `effort`-only case above cannot cover this: with no `format` key at all
// the branch never runs, so that test is satisfied by an implementation with no
// type check whatsoever — proved by mutation, where deleting the check left it
// green. `output_config.format` is a field Anthropic can extend, and the day it
// carries a second type, claiming a json_schema constraint the caller did not
// write would narrow the routing pool for nothing and hand a downstream codec a
// schema that is not there.
func TestMessagesRequest_outputConfigFormatOfAnotherTypeIsNotASchema(t *testing.T) {
	for _, typ := range []string{"text", "json_object", ""} {
		body := `{
			"model": "claude-opus-5",
			"max_tokens": 400,
			"messages": [{"role": "user", "content": "hi"}],
			"output_config": {"format": {"type": "` + typ + `"}}
		}`
		out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(body), "")
		if err != nil {
			t.Fatalf("type=%q: %v", typ, err)
		}
		if gjson.GetBytes(out, "response_format").Exists() {
			t.Errorf("output_config.format.type=%q became a response_format: %s", typ, out)
		}
	}
}

// The canonical body IS an OpenAI body (§3a), and OpenAI REQUIRES
// `response_format.json_schema.name`.
//
// Measured live on prod after the translation above shipped: `/v1/messages`
// with a schema, routed to an OpenAI target, answered
// 400 "Missing required parameter: 'response_format.json_schema.name'". The
// same request routed to an Anthropic target answered 200 with the schema
// honoured — so the translation was right about the schema and wrong about the
// shape it has to live in.
//
// Anthropic's `output_config.format` carries no name, so the caller genuinely
// sent none. Filling it is the sanctioned adapter auto-fill: a
// protocol-required field the source wire does not have, supplied so the
// request works, rather than a knob or a refusal. The value is a label OpenAI
// echoes back and nothing routes on.
func TestMessagesRequest_schemaCarriesTheNameOpenAIRequires(t *testing.T) {
	out, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model": "claude-opus-5",
		"max_tokens": 400,
		"messages": [{"role": "user", "content": "answer the schema"}],
		"output_config": {"format": {"type": "json_schema", "schema": {"type": "object"}}}
	}`), "")
	if err != nil {
		t.Fatalf("MessagesRequestToOpenAIChatCompletion: %v", err)
	}
	name := gjson.GetBytes(out, "response_format.json_schema.name").String()
	if name == "" {
		t.Fatalf("no json_schema.name — OpenAI answers 400 'Missing required parameter': %s", out)
	}
	// OpenAI constrains the value to ^[a-zA-Z0-9_-]+$; a name it rejects is the
	// same 400 by another route.
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			t.Errorf("json_schema.name = %q contains %q, which OpenAI rejects", name, r)
		}
	}
}
