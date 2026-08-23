package codec

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A caller may ask for BOTH a schema-constrained answer and reasoning. Both land
// in Anthropic's `output_config`, and both writers used to assign the WHOLE map:
// the response_format branch wrote {"format": …} and applyReasoningIntent then
// wrote {"effort": …} over it. Go map assignment replaces, and the reasoning
// walk runs second, so the caller's schema left the process.
//
// Confirmed live on prod-20260819d before this test existed —
// claude-opus-5 + reasoning_effort:high + json_schema returned HTTP 200 with no
// JSON object and `x-nexus-coerced: reasoning_effort→thinking.adaptive+
// output_config.effort=high`, a header that names the coercion while saying
// nothing about the schema it destroyed. That is the 200-and-prose failure the
// routing dimension exists to prevent, manufactured by this encoder.
//
// The models that take the ADAPTIVE path are the ones missing from
// claudeModelsOnEnabledThinking — claude-opus-5, claude-fable-5,
// claude-sonnet-5, claude-opus-4-8, claude-opus-4-7 — which is exactly the set
// this branch tagged `structured_outputs`, so the router now prefers them for a
// reasoning+schema request. Every pre-existing test used an ENABLED-contract
// model, so none of them ever executed this path.
const canonSchemaPlusEffort = `{
	"model": "claude-opus-5",
	"max_tokens": 400,
	"reasoning_effort": "high",
	"messages": [{"role": "user", "content": "should I respond?"}],
	"response_format": {
		"type": "json_schema",
		"json_schema": {"name": "verdict", "strict": true, "schema": {
			"type": "object",
			"properties": {"respond": {"type": "boolean"}, "reason": {"type": "string"}},
			"required": ["respond", "reason"],
			"additionalProperties": false
		}}
	}
}`

func TestEncodeRequest_schemaSurvivesReasoningEffort(t *testing.T) {
	out := encodeAnthropic(t, canonSchemaPlusEffort)

	if got := gjson.GetBytes(out, "output_config.format.type").String(); got != "json_schema" {
		t.Errorf("the schema was dropped when reasoning_effort was also set: %s", out)
	}
	if !gjson.GetBytes(out, "output_config.format.schema.properties.respond").Exists() {
		t.Errorf("output_config.format.schema lost its properties: %s", out)
	}
	// The reasoning intent must survive too — this is a merge, not a contest.
	if got := gjson.GetBytes(out, "output_config.effort").String(); got == "" {
		t.Errorf("reasoning effort was dropped instead: %s", out)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Errorf("thinking.type = %q, want adaptive on an adaptive-contract model", got)
	}
}

// The same collision through the other door: nexus.ext.anthropic.thinking with
// an enabled+budget shape on an adaptive-contract model. That branch is guarded
// (`if _, has := out["output_config"]; !has`), so before this fix it lost the
// EFFORT rather than the schema — while still appending the rewrite that claims
// the effort was written. A header that names a coercion the body does not carry
// is worse than either loss alone.
func TestEncodeRequest_schemaAndExtThinkingBothSurvive(t *testing.T) {
	out := encodeAnthropic(t, `{
		"model": "claude-opus-5",
		"max_tokens": 16000,
		"messages": [{"role": "user", "content": "hi"}],
		"nexus": {"ext": {"anthropic": {"thinking": {"type": "enabled", "budget_tokens": 8000}}}},
		"response_format": {"type": "json_schema",
			"json_schema": {"schema": {"type": "object", "properties": {"a": {"type": "string"}}}}}
	}`)

	if !gjson.GetBytes(out, "output_config.format.schema.properties.a").Exists() {
		t.Errorf("the schema was lost on the ext-thinking path: %s", out)
	}
	if gjson.GetBytes(out, "output_config.effort").String() == "" {
		t.Errorf("output_config.effort was skipped because the format key was already there, "+
			"while the coerced header still claims it: %s", out)
	}
}

// Reasoning alone must keep behaving exactly as before — the fix is a merge, and
// a merge must not invent a `format` key for a caller who asked for no schema.
func TestEncodeRequest_reasoningWithoutSchemaIsUnchanged(t *testing.T) {
	out := encodeAnthropic(t, `{
		"model": "claude-opus-5",
		"max_tokens": 400,
		"reasoning_effort": "high",
		"messages": [{"role": "user", "content": "think"}]
	}`)
	if got := gjson.GetBytes(out, "output_config.effort").String(); got == "" {
		t.Errorf("effort missing: %s", out)
	}
	if gjson.GetBytes(out, "output_config.format").Exists() {
		t.Errorf("a caller who asked for no schema must not get a format key: %s", out)
	}
}

// encodeAnthropicFull returns the rewrites alongside the body — the coerced
// header is the half these cases are about.
func encodeAnthropicFull(t *testing.T, canon string) ([]byte, []string) {
	t.Helper()
	var c Codec
	res, err := c.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(canon), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return res.Body, res.Rewrites
}

// A schema we could not carry is a field we changed, so it belongs in the
// coerced header.
//
// Anthropic answers both shapes with a loud 400 — probed 2026-08-19: no schema
// key at all gives "output_config.format.schema: Field required", and an empty
// object gives "Empty schema ({}) that accepts any JSON value is not
// supported". So the caller does get an error. The problem is WHOSE error it
// looks like: they wrote a `response_format.json_schema` envelope, we dropped
// its contents, and the upstream then complains about
// `output_config.format.schema` — a field name that appears nowhere in what
// they sent. Every other coercion in this codec announces itself; this one left
// the operator reading a 400 about a field the gateway invented.
//
// §3a: a field we coerced is a field we own.
func TestEncodeRequest_droppedSchemaIsAnnounced(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"the envelope carries no schema the wire can use", `{
			"model": "claude-opus-5", "max_tokens": 300,
			"messages": [{"role": "user", "content": "hi"}],
			"response_format": {"type": "json_schema", "json_schema": {"name": "v", "schema": {}}}
		}`},
		{"the envelope is not an object at all", `{
			"model": "claude-opus-5", "max_tokens": 300,
			"messages": [{"role": "user", "content": "hi"}],
			"response_format": {"type": "json_schema", "json_schema": "verdict"}
		}`},
		{"json_schema asked for with no envelope", `{
			"model": "claude-opus-5", "max_tokens": 300,
			"messages": [{"role": "user", "content": "hi"}],
			"response_format": {"type": "json_schema"}
		}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, rewrites := encodeAnthropicFull(t, tc.body)
			if gjson.GetBytes(out, "output_config.format.schema").Exists() {
				t.Fatalf("a schema reached the wire from a body that carries none: %s", out)
			}
			var announced bool
			for _, r := range rewrites {
				if strings.Contains(r, "json_schema") {
					announced = true
				}
			}
			if !announced {
				t.Errorf("the schema was dropped and x-nexus-coerced says nothing: rewrites=%v\n"+
					"  the caller will read a 400 naming output_config.format.schema, a field "+
					"they never wrote, with no record that we are the ones who emptied it", rewrites)
			}
		})
	}
}

// The counter-case: a schema we DID carry must not be announced as dropped. A
// header reporting a coercion that did not happen is the same defect in the
// other direction, and this file already fixed one of those.
func TestEncodeRequest_carriedSchemaIsNotAnnouncedAsDropped(t *testing.T) {
	_, rewrites := encodeAnthropicFull(t, `{
		"model": "claude-opus-5", "max_tokens": 300,
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {"type": "json_schema",
			"json_schema": {"schema": {"type": "object", "properties": {"a": {"type": "string"}}}}}
	}`)
	// Asserted against the rewrite the code ACTUALLY emits, not a word that
	// sounds like it. The first version of this test looked for "dropped",
	// which appears nowhere in that string, so it survived the mutant it was
	// written to catch — `carried` never being set, which announces a loss on
	// every well-formed schema. A counter-case that cannot fail is not one.
	for _, r := range rewrites {
		if strings.Contains(r, "no schema") {
			t.Errorf("the schema was carried, yet x-nexus-coerced reports it lost: %v", rewrites)
		}
	}
}
