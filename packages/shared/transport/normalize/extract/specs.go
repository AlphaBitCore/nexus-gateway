package extract

// KnownChatSpecs lists every request-body chat shape the Tier-2 probe
// recognises. Order matters: probes iterate in this order and the
// first to score >= threshold wins. Order from most-specific to
// most-permissive so a body with mixed signals lands on the tighter
// spec.
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
	// Anthropic Messages API (`/v1/messages`). Top-level `model` +
	// `max_tokens` + `messages[].content` as block array.
	{
		ID:          "anthropic-messages",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeBlockArray,
		ModelPath:   "model",
		SystemPath:  "system",
		ToolsPath:   "tools",
		SignatureFields: []string{
			"max_tokens",
			"anthropic_version",
			"stop_sequences",
		},
	},
	// Gemini generateContent. Uses `contents` (plural), `role` per
	// content, `parts[].text` shape.
	{
		ID:          "gemini-generate",
		Locator:     "contents",
		RolePath:    "role",
		ContentPath: "parts",
		Shape:       ContentShapeNestedTextArray,
		ModelPath:   "model",
		SystemPath:  "systemInstruction.parts.0.text",
		ToolsPath:   "tools",
		SignatureFields: []string{
			"generationConfig",
			"safetySettings",
			"systemInstruction",
		},
	},
	// OpenAI Chat Completions — the canonical shape: messages[]
	// with `role` + `content` (string). Most permissive recogniser,
	// so it sits AFTER the more specific consumer specs.
	{
		ID:          "openai-chat",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeString,
		ModelPath:   "model",
		ToolsPath:   "tools",
		SignatureFields: []string{
			"temperature",
			"max_tokens",
			"top_p",
			"frequency_penalty",
			"presence_penalty",
			"response_format",
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
	// These signatures keep it apart from the anthropic-completions-legacy
	// API shape (which uses `max_tokens_to_sample` + `\n\nHuman:` prefix
	// in the prompt and lacks every signature field above). Captured
	// against the /api/organizations/.../chat_conversations/.../completion
	// endpoint in prod (see traffic_event b35c8508-ff77-449e-8d42-cf35a5348a9b).
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
	// Anthropic legacy completion API (`/v1/complete`). No messages
	// array — flat `prompt` field. Kept for older clients.
	{
		ID:          "anthropic-completions-legacy",
		Locator:     "", // no array — sentinel signals "single-prompt"
		RolePath:    "",
		ContentPath: "prompt",
		Shape:       ContentShapeString,
		ModelPath:   "model",
		SignatureFields: []string{
			"max_tokens_to_sample",
			"prompt",
		},
	},
	// OpenAI legacy completions API (`/v1/completions`). Flat prompt
	// without `\n\nHuman:` framing — distinguished from Anthropic
	// legacy by the absence of `max_tokens_to_sample`.
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
// recognises. Stream-aware: AccumulatorRule tells the probe how to
// fold an SSE stream into a single document tree before applying
// AssistantTextPath.
//
// SignatureFields on response specs probe the assembled state (or
// non-stream body) for shape-unique field names that tip confidence
// when locator/text paths alone are ambiguous.
var KnownResponseSpecs = []ChatResponseSpec{
	// ChatGPT-web SSE: JSON-Patch-flavoured deltas folded into a
	// message tree. Assistant text lives under message.content.parts[0].
	{
		ID:                "chatgpt-web",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "json-patch",
		AssistantTextPath: "message.content.parts.0",
		FinishReasonPath:  "message.end_turn",
		SignatureFields:   []string{"conversation_id", "message_id"},
	},
	// Anthropic Messages SSE: `event: content_block_delta` frames with
	// `delta.text`. Concat-text accumulation.
	{
		ID:                "anthropic-messages-sse",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "concat-text",
		AssistantTextPath: "_accumulated", // synthetic key holding concatenated text
		FinishReasonPath:  "stop_reason",
		UsagePath:         "usage",
		ModelPath:         "model",
		StreamDeltaPath:   "delta.text",
		SignatureFields:   []string{"content_block_delta", "message_delta"},
	},
	// OpenAI Chat Completions SSE: `data:` frames each carrying a
	// chunk with `choices[0].delta.content`. Concat-text accumulation.
	{
		ID:                "openai-chat-sse",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "concat-text",
		AssistantTextPath: "_accumulated",
		FinishReasonPath:  "choices.0.finish_reason",
		UsagePath:         "usage",
		ModelPath:         "model",
		StreamDeltaPath:   "choices.0.delta.content",
		SignatureFields:   []string{"choices", "object"},
	},
	// Gemini generateContent SSE / NDJSON: each frame carries a full
	// candidate update with `candidates[0].content.parts[0].text`.
	{
		ID:                "gemini-generate-sse",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "concat-text",
		AssistantTextPath: "_accumulated",
		FinishReasonPath:  "candidates.0.finishReason",
		UsagePath:         "usageMetadata",
		ModelPath:         "modelVersion",
		StreamDeltaPath:   "candidates.0.content.parts.0.text",
		SignatureFields:   []string{"candidates", "promptFeedback"},
	},
	// Non-stream OpenAI: single JSON document.
	{
		ID:                "openai-chat-nonstream",
		StreamFraming:     "single-json",
		AccumulatorRule:   "none",
		AssistantTextPath: "choices.0.message.content",
		FinishReasonPath:  "choices.0.finish_reason",
		UsagePath:         "usage",
		ModelPath:         "model",
		SignatureFields:   []string{"id", "choices", "created"},
	},
	// Non-stream Anthropic: single JSON document.
	{
		ID:                "anthropic-messages-nonstream",
		StreamFraming:     "single-json",
		AccumulatorRule:   "none",
		AssistantTextPath: "content.0.text",
		FinishReasonPath:  "stop_reason",
		UsagePath:         "usage",
		ModelPath:         "model",
		SignatureFields:   []string{"stop_reason", "content"},
	},
	// Non-stream Gemini.
	{
		ID:                "gemini-generate-nonstream",
		StreamFraming:     "single-json",
		AccumulatorRule:   "none",
		AssistantTextPath: "candidates.0.content.parts.0.text",
		FinishReasonPath:  "candidates.0.finishReason",
		UsagePath:         "usageMetadata",
		ModelPath:         "modelVersion",
		SignatureFields:   []string{"candidates"},
	},
}
