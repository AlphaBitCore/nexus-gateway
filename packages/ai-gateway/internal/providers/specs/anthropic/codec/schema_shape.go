package codec

import (
	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
)

// jsonSchemaKeywords are the keys whose presence makes a JSON object a schema
// rather than something wrapping one. Kept to the constructs a structured-output
// schema actually opens with — an object declaring none of them constrains
// nothing, so treating it as no schema loses no caller intent.
//
// `$schema` is on the list because it appeared in two of the three real payloads
// the replay corpus captured (draft-07), a key no synthetic fixture had.
var jsonSchemaKeywords = []string{
	"type", "properties", "items", "enum", "const",
	"$ref", "$schema", "allOf", "anyOf", "oneOf",
}

// looksLikeJSONSchema distinguishes a bare schema from OpenAI's
// {name, strict, schema} envelope, for the case where the envelope is present
// and its `schema` key is not.
//
// The codec falls back to treating the whole `json_schema` value as the schema
// so that a caller who puts one there directly is served. Unconditionally, that
// fallback shipped the WRAPPER: probed 2026-08-19, `{"name":"verdict",
// "strict":true}` reaches Anthropic as output_config.format.schema and comes
// back as 400 "Invalid schema: Schema type is missing for schema: {'name':
// 'verdict', 'strict': True}" — the upstream quoting the caller's own envelope
// keys under a field name they never wrote.
//
// Judged by SHAPE rather than by the absence of envelope keys: `name` is a
// perfectly legal schema property name, so "has name, therefore an envelope"
// would reject a real schema describing an object that has one.
func looksLikeJSONSchema(v gjson.Result) bool {
	if !v.IsObject() {
		return false
	}
	for _, k := range jsonSchemaKeywords {
		if v.Get(k).Exists() {
			return true
		}
	}
	return false
}

// applyResponseFormat translates the caller's `response_format` onto the
// Anthropic Messages wire.
//
// Lifted out of EncodeRequest so the two answers it gives — a system
// instruction for `json_object`, a native `output_config.format` for
// `json_schema` — sit beside looksLikeJSONSchema, which is the only thing that
// decides which bytes are the schema. Keeping the decision and its rationale in
// one file is what stops the next reader from re-deriving it in the other.
func applyResponseFormat(root gjson.Result, out map[string]any, rewrites *[]string) {
	if rf := root.Get("response_format"); rf.Exists() {
		switch rf.Get("type").String() {
		case "json_object":
			// The Anthropic Messages API has no native json_object mode.
			// The widely-used "prefill" trick (append an assistant turn
			// whose content is a bare "{") forces JSON but is silently
			// broken across this gateway: Anthropic completes the object
			// WITHOUT re-emitting the prefilled "{", and neither the
			// non-streaming DecodeResponse nor the SSE stream path can
			// re-prepend it — the SchemaCodec/StreamDecoder interfaces are
			// stateless and never see the originating request, so they
			// cannot know a "{" was prefilled. The caller therefore
			// received content beginning mid-object ("k":1}) that fails
			// JSON.parse 100% of the time.
			//
			// Instead we force JSON via a system instruction. Anthropic
			// emits the complete object (including the opening "{"), so the
			// decode/stream paths pass it through unchanged and the caller
			// gets parseable JSON. The instruction is appended to whatever
			// system content already exists (none / string / text blocks).
			out["system"] = appendSystemInstruction(out["system"], anthropicJSONObjectInstruction)
		case "json_schema":
			// Anthropic has native structured outputs, under
			// `output_config.format`. We used to refuse this outright, which
			// made Claude the only target in the fleet where an OpenAI-shaped
			// structured-output request failed — probed 2026-08-19, all four
			// wires serve it.
			//
			// The spelling is the one Anthropic's own 400 names: plain
			// `response_format` answers "Extra inputs are not permitted", and
			// `output_format` answers "This field is deprecated. Use
			// 'output_config.format' instead". No beta header is needed —
			// anthropic-version 2023-06-01 accepts it.
			//
			// Nothing downstream changes: the answer comes back as an ordinary
			// text content block, not a tool_use, so DecodeResponse and the SSE
			// path carry it unmodified.
			format := map[string]any{"type": "json_schema"}
			carried := false
			if js := rf.Get("json_schema"); js.Exists() {
				// OpenAI wraps the schema in a {name, strict, schema}
				// envelope; Anthropic wants the bare schema and does not know
				// the envelope keys. `strict` needs no counterpart —
				// Anthropic's structured outputs IS constrained decoding.
				node := js.Get("schema")
				if !node.Exists() && looksLikeJSONSchema(js) {
					// Bare form: the caller put the schema directly under
					// json_schema. Accepted only when it LOOKS like one — the
					// unconditional fallback shipped `{name, strict}` as the
					// schema, and Anthropic answered 400 quoting the caller's
					// own envelope keys back under a field they never wrote.
					node = js
				}
				var schema map[string]any
				if err := json.Unmarshal([]byte(node.Raw), &schema); err == nil && len(schema) > 0 {
					format["schema"] = schema
					carried = true
				}
			}
			// A schema we could not carry is a field we changed, so it goes in
			// the coerced header. Probed 2026-08-19: Anthropic rejects both
			// shapes loudly ("output_config.format.schema: Field required" /
			// "Empty schema ({}) ... is not supported"), so the caller does get
			// an error — but they wrote `response_format.json_schema` and the
			// 400 names a field they never sent. Forwarded rather than refused
			// here, because the upstream's verdict on its own field stays true
			// as its validation moves and ours would not (§3a).
			if !carried {
				*rewrites = append(*rewrites,
					"response_format.json_schema→output_config.format with no schema (the caller's schema was empty or unparseable)")
			}
			setOutputConfig(out, "format", format)
		}
	}
}
