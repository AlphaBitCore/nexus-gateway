package openai

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// rewriteToolCallArgsItems overwrites each function-type tool call's
// `function.arguments` with the masked args[i]. It walks the container items
// (response choices, or request messages) in document order and, within each,
// the tool_calls[] at toolCallsKey ("message.tool_calls" for a response choice,
// "tool_calls" for a request message). A non-function tool call is skipped
// WITHOUT consuming an args slot, mirroring decodeOpenAIContent's function-only
// canonical walk so the index alignment with ToolCallArgs holds. Returns once
// args is exhausted; args==nil is a no-op (zero churn on benign traffic).
//
// An EMPTY-STRING args entry is a sentinel for an untouched sibling call: the
// arg slot is still consumed (to keep index alignment) but the wire arguments
// are left byte-for-byte intact — no sjson write, no re-marshal — so an
// innocent call's exact bytes survive.
func rewriteToolCallArgsItems(body []byte, items []gjson.Result, basePath, toolCallsKey string, args []string) ([]byte, int, error) {
	if len(args) == 0 {
		return body, 0, nil
	}
	out := body
	written := 0
	argIdx := 0
	var err error
	for iIdx := range items {
		tc := items[iIdx].Get(toolCallsKey)
		if !tc.IsArray() {
			continue
		}
		calls := tc.Array()
		for kIdx := range calls {
			if calls[kIdx].Get("type").Str != "function" {
				continue // mirrors the canonical function-only walk
			}
			if argIdx >= len(args) {
				return out, written, nil
			}
			if args[argIdx] == "" {
				// Untouched sibling — leave the wire arguments intact.
				argIdx++
				continue
			}
			p := fmt.Sprintf("%s.%d.%s.%d.function.arguments", basePath, iIdx, toolCallsKey, kIdx)
			out, err = sjson.SetBytes(out, p, args[argIdx])
			if err != nil {
				return nil, written, fmt.Errorf("openai: rewrite %s: %w", p, err)
			}
			argIdx++
			written++
		}
	}
	return out, written, nil
}

// rewriteResponsesToolArgs masks the `arguments` of each Responses-API
// function_call item (where the JSON-string arguments live directly on the
// item, not nested under "function"). Walks items in document order, the same
// order extractResponsesCreate / extractResponsesResponse emit them.
func rewriteResponsesToolArgs(body []byte, items []gjson.Result, basePath string, args []string) ([]byte, int, error) {
	if len(args) == 0 {
		return body, 0, nil
	}
	out := body
	written := 0
	argIdx := 0
	var err error
	for iIdx := range items {
		if items[iIdx].Get("type").Str != "function_call" {
			continue
		}
		if argIdx >= len(args) {
			return out, written, nil
		}
		if args[argIdx] == "" {
			// Untouched sibling — leave the wire arguments intact.
			argIdx++
			continue
		}
		p := fmt.Sprintf("%s.%d.arguments", basePath, iIdx)
		out, err = sjson.SetBytes(out, p, args[argIdx])
		if err != nil {
			return nil, written, fmt.Errorf("openai: rewrite %s: %w", p, err)
		}
		argIdx++
		written++
	}
	return out, written, nil
}
