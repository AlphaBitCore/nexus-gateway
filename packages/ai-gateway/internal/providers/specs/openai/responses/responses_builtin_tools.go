package responses

// IsResponsesBuiltinTool reports whether the given tool entry's `type`
// field names an OpenAI-native Responses-API built-in tool. These tools
// require server-side execution that only OpenAI's Responses runtime can
// perform; cross-format routes (target != spec_openai) reject them with
// a structured 400 error envelope.
//
// Caller-defined `function` tools are NOT in this list — those round-
// trip through canonical chat-completions function tool entries and
// are honoured by every adapter.
//
// Source: openai-python types.responses (ToolUnion variants) — kept in
// lockstep with builtinToolTypes in codec_responses.go (which the request
// codec uses to partition tools[]). If you add a new built-in tool here,
// also add it there.
//
// Per provider-adapter-architecture.md §3a Rule 6: any new entry must
// reach this list from observed traffic, not speculation. The runtime
// guard only fires on cross-format paths, so leaving a future built-in
// tool out of this list is non-fatal: it will be silently passed through
// to spec_openai (the only adapter that natively supports responses-api)
// and surface as an upstream 4xx if OpenAI does not yet support it.
func IsResponsesBuiltinTool(typeStr string) bool {
	_, ok := responsesBuiltinToolEntryTypes[typeStr]
	return ok
}

// responsesBuiltinToolEntryTypes mirrors codec_responses.go's
// builtinToolTypes private map; exported indirectly via the
// IsResponsesBuiltinTool predicate so the handler / cross-format guard
// can consult it without exposing the map for mutation.
var responsesBuiltinToolEntryTypes = map[string]struct{}{
	"web_search":           {},
	"web_search_preview":   {},
	"file_search":          {},
	"computer_use_preview": {},
	"image_generation":     {},
	"mcp":                  {},
	"code_interpreter":     {},
	"custom":               {},
	"apply_patch":          {},
	"tool_search":          {},
	"function_shell":       {},
}
