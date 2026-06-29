package core

import (
	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// PayloadFromExtracted builds a hookable NormalizedPayload from a traffic
// adapter's flat extraction outputs: the text Segments AND the raw tool-call
// JSON (ToolCallSegments). It is the tool-aware sibling of
// PayloadFromTextSegments — used on the AI-gateway hook path, where the
// adapter (not the full normalize codec) feeds the hooks, so that tool-call
// arguments enter the projection/redaction pipeline like any other content.
//
// Text segments become ContentText blocks (preserving order). Each
// FUNCTION-type tool call becomes a ContentToolUse block whose Input is the
// parsed `function.arguments` object, in ToolCallSegments order — the same
// order the wire `tool_calls[]` appear and the OpenAI codec emits canonical
// blocks, so ToolCallArgsFromPayload + the wire rewriter stay index-aligned.
//
// Disclosed gaps (kept identical to the canonical codec for cross-path
// alignment): non-function tool calls and legacy top-level `function_call`
// items are skipped (no canonical ContentToolUse, hence no arg masking); a
// tool call whose `arguments` is not a JSON object yields an empty Input
// (fail-open — nothing to scan or mask).
func PayloadFromExtracted(segments []string, toolCallSegments []string) *normalize.NormalizedPayload {
	content := make([]normalize.ContentBlock, 0, len(segments)+len(toolCallSegments))
	for _, s := range segments {
		content = append(content, normalize.ContentBlock{Type: normalize.ContentText, Text: s})
	}
	for _, raw := range toolCallSegments {
		if !gjson.Valid(raw) {
			continue
		}
		tc := gjson.Parse(raw)
		// Only modern function-type tool calls carry a `function` object with
		// a JSON-string `arguments`; mirror decodeOpenAIContent's filter.
		if tc.Get("type").Str != "function" {
			continue
		}
		fn := tc.Get("function")
		if !fn.Exists() {
			continue
		}
		var input map[string]any
		if args := fn.Get("arguments"); args.Exists() && args.Str != "" {
			// Fail-open: an arguments string that is not a JSON object leaves
			// Input nil (nothing to scan/mask), never an error.
			_ = json.Unmarshal([]byte(args.Str), &input)
		}
		content = append(content, normalize.ContentBlock{
			Type: normalize.ContentToolUse,
			ToolUse: &normalize.ToolUse{
				CallID: tc.Get("id").Str,
				Name:   fn.Get("name").Str,
				Input:  input,
			},
		})
	}
	if len(content) == 0 {
		return &normalize.NormalizedPayload{Kind: normalize.KindAIChat, NormalizeVersion: normalize.SchemaVersion}
	}
	return &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Protocol:         "synthetic",
		Messages:         []normalize.Message{{Role: normalize.RoleUser, Content: content}},
	}
}
