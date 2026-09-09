package codecs

import (
	"fmt"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// RewriteCanonicalResponsesContent writes edited canonical CONTENT back into a
// /v1/responses body, in place, and touches nothing else.
//
// There are exactly TWO canonical response wire shapes — chat-completions
// `choices[]` and Responses `output[]` — and this is the second one's rewriter.
// That is not the same thing as two shapes at the waist: a hook still receives
// ONE NormalizedPayload, because both shapes decode into it. What differs is
// where the text lives on the wire, and writing bytes is the one job that has to
// know.
//
// The alternative, tried and reverted, was to decode the Responses body to
// canonical chat, redact there, and re-encode. That reconstructs the body, and
// reconstruction loses everything the intermediate shape does not model:
// previous_response_id, output[].content[].annotations (the citations a
// web-search answer must display), reasoning encrypted_content, the echoed
// request config, and the item ids — which were regenerated, so an
// item_reference named something the upstream had never seen. Editing in place
// loses none of it, because it writes only the text slots.
//
// The walk follows normalizeResponse exactly, per output item in order:
//
//	reasoning              → one block per non-empty summary[].text
//	message                → one block per output_text content part
//	image_generation_call  → one media block (no text slot; consumed, not written)
//	function_call/tool_call→ one tool-use block (arguments)
//
// A block that does not match the slot the walk is on is fail-closed: it means
// the payload was not decoded from this body, and writing a prefix would put one
// item's redaction onto another item's text.
func RewriteCanonicalResponsesContent(body []byte, edited core.NormalizedPayload) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, fmt.Errorf("canonical responses rewrite: body is not valid JSON")
	}
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return nil, 0, fmt.Errorf("canonical responses rewrite: no output[] in body")
	}
	// The decode folds every output item into ONE assistant message, so a
	// payload with a different message count did not come from this body.
	if len(edited.Messages) != 1 {
		return nil, 0, fmt.Errorf(
			"canonical responses rewrite: payload carries %d messages, and this shape decodes "+
				"to exactly one — the payload was not decoded from this body", len(edited.Messages))
	}
	blocks := edited.Messages[0].Content

	out := body
	written, next := 0, 0
	take := func(want core.ContentType) (core.ContentBlock, bool) {
		if next < len(blocks) && blocks[next].Type == want {
			b := blocks[next]
			next++
			return b, true
		}
		return core.ContentBlock{}, false
	}

	for oi, item := range output.Array() {
		base := fmt.Sprintf("output.%d", oi)
		switch item.Get("type").Str {
		case "reasoning":
			for si, s := range item.Get("summary").Array() {
				if s.Get("text").Str == "" {
					continue // the decode skipped it, so no block was produced
				}
				b, ok := take(core.ContentReasoning)
				if !ok {
					return nil, written, blockMismatch(fmt.Sprintf("%s.summary.%d.text", base, si), blocks, next)
				}
				var err error
				p := fmt.Sprintf("%s.summary.%d.text", base, si)
				if out, err = setIfChanged(out, p, b.Text, &written); err != nil {
					return nil, written, err
				}
			}
		case "message":
			for ci, c := range item.Get("content").Array() {
				t := c.Get("type").Str
				if t != "output_text" && t != "" {
					continue // the decode produced nothing for this part
				}
				if c.Get("text").Str == "" {
					continue
				}
				b, ok := take(core.ContentText)
				if !ok {
					return nil, written, blockMismatch(fmt.Sprintf("%s.content.%d.text", base, ci), blocks, next)
				}
				var err error
				p := fmt.Sprintf("%s.content.%d.text", base, ci)
				if out, err = setIfChanged(out, p, b.Text, &written); err != nil {
					return nil, written, err
				}
			}
		case "image_generation_call":
			if item.Get("result").Str == "" {
				continue
			}
			// A media block carries no text; it is consumed so the cursor stays
			// aligned with the decode, and nothing is written.
			if next < len(blocks) && blocks[next].MediaRef != nil {
				next++
				continue
			}
			return nil, written, blockMismatch(base+".result", blocks, next)
		case "function_call", "tool_call":
			b, ok := take(core.ContentToolUse)
			if !ok {
				return nil, written, blockMismatch(base+".arguments", blocks, next)
			}
			if b.ToolUse == nil || b.ToolUse.Input == nil {
				continue
			}
			args, err := json.Marshal(b.ToolUse.Input)
			if err != nil {
				return nil, written, fmt.Errorf("canonical responses rewrite: tool arguments: %w", err)
			}
			// The wire carries the call either as a JSON `arguments` STRING or as
			// a parsed `input` object, and the decode reads whichever is there.
			// Write back to the same one, or the edit lands in a field the
			// upstream and the client both ignore.
			if item.Get("arguments").Type == gjson.String {
				if out, err = setIfChanged(out, base+".arguments", string(args), &written); err != nil {
					return nil, written, err
				}
				continue
			}
			if item.Get("input").Exists() {
				before := gjson.GetBytes(out, base+".input").Raw
				if before == string(args) {
					continue
				}
				if out, err = setRawIfChanged(out, base+".input", string(args), &written); err != nil {
					return nil, written, err
				}
			}
		}
	}

	if next != len(blocks) {
		return nil, written, fmt.Errorf(
			"canonical responses rewrite: payload has %d blocks but the wire offered %d slots — "+
				"the payload was not decoded from this body", len(blocks), next)
	}
	return out, written, nil
}

// CanonicalResponsesHasContent reports whether a /v1/responses body carries any
// text a compliance hook would scan.
//
// The Responses sibling of CanonicalResponseHasContent, and it exists for the
// same reason: when the decode fails the caller has no payload to ask, and has
// to tell "nothing to scan" from "something to scan that nothing scanned".
// Failing closed on the second is the point.
//
// The channel set mirrors the walk above — same items, same slots — because a
// presence check that knows about fewer channels than the rewrite reports "no
// content" for a body that carries some, and a response nobody could scan then
// goes out.
func CanonicalResponsesHasContent(body []byte) bool {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return false
	}
	found := false
	output.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").Str {
		case "reasoning":
			item.Get("summary").ForEach(func(_, s gjson.Result) bool {
				if s.Get("text").Str != "" {
					found = true
				}
				return !found
			})
		case "message":
			item.Get("content").ForEach(func(_, c gjson.Result) bool {
				if t := c.Get("type").Str; (t == "output_text" || t == "") && c.Get("text").Str != "" {
					found = true
				}
				return !found
			})
		case "function_call", "tool_call":
			// Tool arguments are model-authored text and are scanned, so a
			// tool-only response is not "nothing to scan".
			found = true
		}
		return !found
	})
	return found
}
