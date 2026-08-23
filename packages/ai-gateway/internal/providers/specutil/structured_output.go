package specutil

// CanonicalSchemaName is the label a canonical `response_format.json_schema`
// carries when the ingress wire had none to give.
//
// The canonical body IS an OpenAI body (§3a), and OpenAI REQUIRES
// `response_format.json_schema.name` — it answers 400 "Missing required
// parameter: 'response_format.json_schema.name'" without it, measured live on
// prod 2026-08-19 when a `/v1/messages` request carrying a schema was routed to
// an OpenAI target. Neither of the two non-OpenAI ingress wires has the field:
// Anthropic's `output_config.format` and Gemini's
// `generationConfig.responseSchema` are both bare schemas.
//
// So it is filled rather than left out — the same adapter auto-fill as the
// Anthropic `max_tokens` default: a protocol-required field supplied from what
// the caller omitted, so the request works, instead of a knob or a refusal.
//
// The value is a label OpenAI echoes back; nothing routes, caches or bills on
// it. It stays constant on purpose: a name derived from the schema would change
// with every unrelated edit to the caller's properties, and a name derived from
// the request would put caller text in a field OpenAI validates against
// ^[a-zA-Z0-9_-]+$.
const CanonicalSchemaName = "response"

// CanonicalJSONSchema wraps a bare schema in the OpenAI `json_schema` envelope,
// filling the name the wire requires. A nil or empty schema yields an envelope
// with no `schema` key: an absent schema is the caller's own omission and
// fabricating one would answer for them.
func CanonicalJSONSchema(schema map[string]any) map[string]any {
	env := map[string]any{"name": CanonicalSchemaName}
	if len(schema) > 0 {
		env["schema"] = schema
	}
	return env
}
