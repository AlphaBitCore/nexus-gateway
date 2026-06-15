package extract

// KnownChatSpecs lists every request-body chat shape the Tier-2 probe
// recognises. Order matters: probes iterate in this order and the
// first to score >= threshold wins. Order from most-specific to
// most-permissive so a body with mixed signals lands on the tighter
// spec.
//
// Tier 2 owns ONLY consumer-web shapes (plus the one legacy flat-prompt
// shape below). Standard-API wires — OpenAI Chat, Anthropic Messages,
// Gemini generateContent, OpenAI Responses — are decoded by the Tier-1
// codecs in transport/normalize/codecs, which the Registry reaches by
// adapter key, endpoint path, or content sniff; duplicating them here
// as patterns would only produce a lower-fidelity second answer.
//
// Each spec is intentionally short — the probe in probe.go does the
// heavy lifting. SignatureFields are existence-only checks (presence
// of an unusual field name unique to that producer) used to break
// ties between specs that share locator/role/content paths.
var KnownChatSpecs = []ChatSpec{
	// ChatGPT-web (consumer surface). Distinctive: `author.role` plus
	// `content.parts[]`, plus `suggestion_type` / `chosen_suggestion`
	// / `client_contextual_info` fields the official OpenAI API
	// never ships.
	{
		ID:          "chatgpt-web",
		Locator:     "messages",
		RolePath:    "author.role",
		ContentPath: "content.parts",
		Shape:       ContentShapeStringArray,
		ModelPath:   "model",
		SignatureFields: []string{
			"chosen_suggestion",
			"client_contextual_info",
			"suggestion_type",
			"force_parallel_switch",
			"parent_message_id",
		},
	},
	// claude.ai web (consumer surface). Single-prompt request shape with
	// server-side conversation history — the body carries ONLY the
	// latest user message in `prompt` plus a `parent_message_uuid`
	// pointer to the prior turn. The full conversation lives on
	// Anthropic's servers and is never shipped wire-side.
	//
	// Distinctive top-level fields:
	//   - prompt (string)              — the new user message
	//   - parent_message_uuid (string) — server-side thread link
	//   - rendering_mode (string)      — "messages" / "raw"
	//   - turn_message_uuids (array)   — sibling-message refs
	//   - personalized_styles (object) — claude.ai style customisations
	//   - sync_sources (array)         — connected app context refs
	//   - timezone (string)            — client timezone
	//   - locale (string)              — client locale
	//
	// These signatures keep it apart from the flat-prompt legacy
	// completions shape below (which lacks every signature field above).
	// Captured against the /api/organizations/.../chat_conversations/
	// .../completion endpoint in prod.
	{
		ID:          "claude-web",
		Locator:     "", // single-prompt sentinel
		RolePath:    "",
		ContentPath: "prompt",
		Shape:       ContentShapeString,
		ModelPath:   "model",
		ToolsPath:   "tools",
		SignatureFields: []string{
			"parent_message_uuid",
			"rendering_mode",
			"turn_message_uuids",
			"personalized_styles",
			"sync_sources",
			"timezone",
			"locale",
		},
	},
	// Flat-prompt legacy completions (`/v1/completions` shape): no
	// messages array — a single top-level `prompt` string. This is the
	// ONE surviving non-consumer spec: the OpenAI Chat codec rejects
	// bodies without a messages[] array (its request decode requires
	// them), and character.ai's roleplay surface ships exactly this
	// flat-prompt shape — the character-web adapter routes its request
	// direction here.
	{
		ID:          "openai-completions-legacy",
		Locator:     "",
		RolePath:    "",
		ContentPath: "prompt",
		Shape:       ContentShapeString,
		ModelPath:   "model",
		SignatureFields: []string{
			"prompt",
			"max_tokens",
			"temperature",
		},
	},
}

// KnownResponseSpecs lists every response-body chat shape the probe
// recognises. One spec survives: the ChatGPT-web JSON-Patch SSE stream.
// Standard-API response wires (OpenAI / Anthropic / Gemini SSE and
// non-stream JSON) are folded by the Tier-1 codecs.
var KnownResponseSpecs = []ChatResponseSpec{
	// ChatGPT-web SSE: JSON-Patch-flavoured deltas folded into a
	// message tree. Assistant text lives under message.content.parts[0].
	// The stream's `message.end_turn` terminal marker is a boolean, not
	// a finish-reason string, so no FinishReason is extracted. The
	// model identifier ships as frame metadata, not as a top-level
	// document field: message-seeding patch frames nest `model_slug`
	// under the patch value envelope (`v.message.metadata.model_slug`),
	// and the telemetry frames carry it at `metadata.model_slug` — the
	// second path covers stream variants whose seed frames were lost.
	{
		ID:                "chatgpt-web",
		AssistantTextPath: "message.content.parts.0",
		SignatureFields:   []string{"conversation_id", "message_id"},
		ModelFramePaths: []string{
			"v.message.metadata.model_slug",
			"metadata.model_slug",
		},
	},
}
