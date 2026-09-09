package codecs

import (
	"fmt"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// RewriteCanonicalResponseContent writes edited canonical CONTENT back into the
// canonical response body it was decoded from, and touches nothing else.
//
// There is no per-provider sibling of this function and there must not be one.
// The canonical response body is the OpenAI chat-completion shape by definition,
// every codec already encodes canonical to its own wire, and a redaction is
// applied to the canonical payload — so one rewriter here serves every provider.
// A reader who finds themselves about to write RewriteAnthropicResponseContent
// is solving the problem below the waist; the answer is upstream of this file.
//
// Content is what compliance scans and therefore all it may change: assistant
// text, reasoning text, refusal text, and tool-call arguments. The envelope —
// id, created, model, usage, system_fingerprint, service_tier, finish_reason —
// carries no user content, is never scanned, and stays exactly as the upstream
// sent it because this function never writes to it. That is why a redaction
// cannot lose it.
//
// It walks the canonical body in the SAME order that decode produced blocks,
// per choice:
//
//	reasoning_content | reasoning   (decodeOpenAIContent prepends it)
//	content                          (the string form)
//	tool_calls[].function.arguments
//	refusal
//	audio.transcript
//
// One walk, two directions. The alternative — an extractor and a rewriter each
// keeping their own idea of which channels occupy which slot — is what put a
// redacted chain-of-thought into `message.content` with every later segment
// shifted by one.
//
// A block count that does not match the wire is fail-closed: it means the
// payload being written back did not come from this body, and writing a prefix
// would put one channel's redaction onto another channel's text.
func RewriteCanonicalResponseContent(body []byte, edited core.NormalizedPayload) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, fmt.Errorf("openai-chat rewrite: body is not valid JSON")
	}
	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() {
		return nil, 0, fmt.Errorf("openai-chat rewrite: no choices[] in body")
	}

	out := body
	written := 0
	msgIdx := 0
	for ci, ch := range choices.Array() {
		if !ch.Get("message").Exists() {
			continue
		}
		if msgIdx >= len(edited.Messages) {
			return nil, written, fmt.Errorf(
				"openai-chat rewrite: body has more choices than the payload has messages "+
					"(choice %d, payload %d) — the payload did not come from this body", ci, len(edited.Messages))
		}
		blocks := edited.Messages[msgIdx].Content
		msgIdx++

		base := fmt.Sprintf("choices.%d.message", ci)
		next := 0
		take := func(want core.ContentType) (core.ContentBlock, bool) {
			if next < len(blocks) && blocks[next].Type == want {
				b := blocks[next]
				next++
				return b, true
			}
			return core.ContentBlock{}, false
		}

		// Reasoning, on whichever of the two alias spellings the wire used.
		reasoningPath := ""
		if r := ch.Get("message.reasoning_content"); r.Type == gjson.String && r.Str != "" {
			reasoningPath = base + ".reasoning_content"
		} else if r := ch.Get("message.reasoning"); r.Type == gjson.String && r.Str != "" {
			reasoningPath = base + ".reasoning"
		}
		if reasoningPath != "" {
			b, ok := take(core.ContentReasoning)
			if !ok {
				return nil, written, blockMismatch(reasoningPath, blocks, next)
			}
			before := written
			var err error
			if out, err = setIfChanged(out, reasoningPath, b.Text, &written); err != nil {
				return nil, written, err
			}
			if written != before {
				if out, err = dropReplayCarrierAfterReasoningEdit(out, base); err != nil {
					return nil, written, err
				}
			}
		}

		if c := ch.Get("message.content"); c.Type == gjson.String && c.Str != "" {
			b, ok := take(core.ContentText)
			if !ok {
				return nil, written, blockMismatch(base+".content", blocks, next)
			}
			var err error
			if out, err = setIfChanged(out, base+".content", b.Text, &written); err != nil {
				return nil, written, err
			}
		}

		// Tool-call arguments are caller/model text as much as any message is;
		// they are scanned, so they are rewritable. The arguments field is a
		// JSON *string* on the wire, so the edited Input is re-marshalled.
		for ti, call := range ch.Get("message.tool_calls").Array() {
			if call.Get("type").Str != "function" {
				continue
			}
			b, ok := take(core.ContentToolUse)
			if !ok {
				return nil, written, blockMismatch(
					fmt.Sprintf("%s.tool_calls.%d", base, ti), blocks, next)
			}
			if b.ToolUse == nil || b.ToolUse.Input == nil {
				continue
			}
			args, err := json.Marshal(b.ToolUse.Input)
			if err != nil {
				return nil, written, fmt.Errorf("openai-chat rewrite: tool-call arguments: %w", err)
			}
			p := fmt.Sprintf("%s.tool_calls.%d.function.arguments", base, ti)
			if out, err = setIfChanged(out, p, string(args), &written); err != nil {
				return nil, written, err
			}
		}

		if rf := ch.Get("message.refusal"); rf.Type == gjson.String && rf.Str != "" {
			b, ok := take(core.ContentRefusal)
			if !ok {
				return nil, written, blockMismatch(base+".refusal", blocks, next)
			}
			var err error
			if out, err = setIfChanged(out, base+".refusal", b.Text, &written); err != nil {
				return nil, written, err
			}
		}

		if tr := ch.Get("message.audio.transcript"); tr.Type == gjson.String && tr.Str != "" {
			b, ok := take(core.ContentText)
			if !ok {
				return nil, written, blockMismatch(base+".audio.transcript", blocks, next)
			}
			var err error
			if out, err = setIfChanged(out, base+".audio.transcript", b.Text, &written); err != nil {
				return nil, written, err
			}
		}
	}
	return out, written, nil
}

// setIfChanged writes value at path only when it differs from what is there,
// so an untouched channel leaves the bytes byte-identical.
func setIfChanged(body []byte, path, value string, written *int) ([]byte, error) {
	if gjson.GetBytes(body, path).Str == value {
		return body, nil
	}
	out, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return nil, fmt.Errorf("openai-chat rewrite %s: %w", path, err)
	}
	*written++
	return out, nil
}

// setRawIfChanged writes a raw JSON value (not a string) at path when it differs
// from what is there. The string form would quote an object, which is a
// different value on the wire.
func setRawIfChanged(body []byte, path, raw string, written *int) ([]byte, error) {
	if gjson.GetBytes(body, path).Raw == raw {
		return body, nil
	}
	out, err := sjson.SetRawBytes(body, path, []byte(raw))
	if err != nil {
		return nil, fmt.Errorf("canonical rewrite %s: %w", path, err)
	}
	*written++
	return out, nil
}

// blockMismatch names the slot the walk was on and what the payload offered
// instead, because "rewrite failed" alone sends a reader to the wrong file.
func blockMismatch(path string, blocks []core.ContentBlock, at int) error {
	got := "no further blocks"
	if at < len(blocks) {
		got = string(blocks[at].Type)
	}
	return fmt.Errorf(
		"openai-chat rewrite: wire slot %s has no matching canonical block (payload offered %s at "+
			"position %d of %d) — the payload was not decoded from this body",
		path, got, at, len(blocks))
}

// canonicalContentChannel is one text-bearing slot on a canonical response
// message, in the order the decode produces blocks for it.
//
// Declared once and consumed twice — by the rewrite that writes redactions back,
// and by the presence check that decides whether an undecodable body had
// anything to scan. Two lists would drift, and the direction they drift matters:
// a presence check that knows about fewer channels than the rewrite reports "no
// content" for a body that does carry some, and a response nobody could scan
// goes out.
type canonicalContentChannel struct {
	// suffix is appended to `choices.<i>.message`.
	suffix string
	// kind is the canonical block the decode produces for this slot.
	kind core.ContentType
}

// canonicalContentChannels is the ordered list. Reasoning first because
// decodeOpenAIContent prepends it; tool-call arguments are absent because they
// travel as structured spans rather than flat text.
var canonicalContentChannels = []canonicalContentChannel{
	{".reasoning_content", core.ContentReasoning},
	{".reasoning", core.ContentReasoning},
	{".content", core.ContentText},
	{".refusal", core.ContentRefusal},
	{".audio.transcript", core.ContentText},
}

// CanonicalResponseHasContent reports whether a canonical response body carries
// any text a compliance hook would scan.
//
// It exists for the case where the canonical DECODE failed: the caller then has
// no payload to ask, and needs to distinguish "nothing to scan" from "something
// to scan that nothing scanned". Failing closed on the second is the point;
// failing closed on the first would refuse empty and tool-only responses for no
// gain.
func CanonicalResponseHasContent(body []byte) bool {
	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() {
		return false
	}
	found := false
	choices.ForEach(func(_, ch gjson.Result) bool {
		for _, c := range canonicalContentChannels {
			if v := ch.Get("message" + c.suffix); v.Type == gjson.String && v.Str != "" {
				found = true
				return false
			}
		}
		if tc := ch.Get("message.tool_calls"); tc.IsArray() && len(tc.Array()) > 0 {
			// Tool-call arguments are caller/model text and are scanned, so a
			// tool-only turn is not "nothing to scan".
			found = true
			return false
		}
		return true
	})
	return found
}

// RewriteCanonicalRequestContent writes edited canonical CONTENT back into the
// canonical REQUEST body it was decoded from, and touches nothing else.
//
// It is the request-direction sibling of RewriteCanonicalResponseContent and
// obeys the same rule: one rewriter, no per-provider version. A request reaching
// here is the canonical OpenAI chat shape — the caller canonicalized it — so the
// ingress format the client actually spoke is already somebody else's problem,
// solved once by the codec that encodes this body back onto a wire.
//
// What may change is what compliance scans: message text, the text of a text
// content part, reasoning text carried on an assistant turn being replayed, a
// tool result's output, and tool-call arguments. Everything else — role, name,
// tools[], sampling params, stream, model — is envelope and is never written.
//
// The walk follows decodeOpenAIContent exactly, per message:
//
//	reasoning_content | reasoning   (prepended by the decode)
//	content                          (string form, or one block per array part)
//	tool_calls[].function.arguments
//
// For an array content, the block the decode WOULD produce is re-derived per
// part through the same openAIContentPart the decode uses, so the two walks
// cannot drift apart. A part with no text slot on the wire (an image, a file, an
// unknown part serialized into a text block) whose text was nevertheless edited
// is fail-closed: the redaction has nowhere to land, and forwarding the original
// bytes would send upstream exactly the content a policy just masked.
func RewriteCanonicalRequestContent(body []byte, edited core.NormalizedPayload) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, fmt.Errorf("canonical request rewrite: body is not valid JSON")
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil, 0, fmt.Errorf("canonical request rewrite: no messages[] in body")
	}
	arr := msgs.Array()
	// normalizeRequest emits exactly one core.Message per wire message with no
	// skipping, so a count difference means the payload came from another body
	// and every index below would address the wrong turn.
	if len(arr) != len(edited.Messages) {
		return nil, 0, fmt.Errorf(
			"canonical request rewrite: body has %d messages, payload has %d — "+
				"the payload was not decoded from this body", len(arr), len(edited.Messages))
	}

	out := body
	written := 0
	for mi, m := range arr {
		blocks := edited.Messages[mi].Content
		base := fmt.Sprintf("messages.%d", mi)
		next := 0
		take := func(want core.ContentType) (core.ContentBlock, bool) {
			if next < len(blocks) && blocks[next].Type == want {
				b := blocks[next]
				next++
				return b, true
			}
			return core.ContentBlock{}, false
		}

		// Reasoning, on whichever of the two alias spellings the wire used.
		reasoningPath := ""
		if r := m.Get("reasoning_content"); r.Type == gjson.String && r.Str != "" {
			reasoningPath = base + ".reasoning_content"
		} else if r := m.Get("reasoning"); r.Type == gjson.String && r.Str != "" {
			reasoningPath = base + ".reasoning"
		}
		if reasoningPath != "" {
			b, ok := take(core.ContentReasoning)
			if !ok {
				return nil, written, blockMismatch(reasoningPath, blocks, next)
			}
			before := written
			var err error
			if out, err = setIfChanged(out, reasoningPath, b.Text, &written); err != nil {
				return nil, written, err
			}
			if written != before {
				if out, err = dropReplayCarrierAfterReasoningEdit(out, base); err != nil {
					return nil, written, err
				}
			}
		}

		c := m.Get("content")
		switch {
		case c.Type == gjson.String && c.Str != "":
			// A string content on a role=tool turn is a tool RESULT, which the
			// decode models as ToolResult.Output rather than as text — writing
			// b.Text there would blank the result on every redaction.
			if m.Get("tool_call_id").Str != "" {
				b, ok := take(core.ContentToolResult)
				if !ok {
					return nil, written, blockMismatch(base+".content", blocks, next)
				}
				if b.ToolResult == nil {
					return nil, written, fmt.Errorf(
						"canonical request rewrite %s.content: tool-result block carries no result", base)
				}
				var err error
				if out, err = setIfChanged(out, base+".content", b.ToolResult.Output, &written); err != nil {
					return nil, written, err
				}
				break
			}
			b, ok := take(core.ContentText)
			if !ok {
				return nil, written, blockMismatch(base+".content", blocks, next)
			}
			var err error
			if out, err = setIfChanged(out, base+".content", b.Text, &written); err != nil {
				return nil, written, err
			}
		case c.IsArray():
			for pi, part := range c.Array() {
				var pm map[string]any
				if err := json.Unmarshal([]byte(part.Raw), &pm); err != nil {
					return nil, written, fmt.Errorf(
						"canonical request rewrite %s.content.%d: part is not an object: %w", base, pi, err)
				}
				// Re-derive the block the decode produced for this part through
				// the decode's own function, so the two walks cannot disagree
				// about which slot holds which block.
				want := openAIContentPart(pm, "")
				b, ok := take(want.Type)
				if !ok {
					return nil, written, blockMismatch(
						fmt.Sprintf("%s.content.%d", base, pi), blocks, next)
				}
				if t, _ := pm["type"].(string); t == "text" {
					var err error
					p := fmt.Sprintf("%s.content.%d.text", base, pi)
					if out, err = setIfChanged(out, p, b.Text, &written); err != nil {
						return nil, written, err
					}
					continue
				}
				// No text slot on this part. Equal text means nothing was
				// redacted here and there is nothing to write; different text
				// means a redaction would be silently dropped.
				if b.Text != want.Text {
					return nil, written, fmt.Errorf(
						"canonical request rewrite %s.content.%d: this part has no text slot on the "+
							"wire but its content was edited — the redaction has nowhere to land",
						base, pi)
				}
			}
		}

		for ti, call := range m.Get("tool_calls").Array() {
			if call.Get("type").Str != "function" {
				continue
			}
			b, ok := take(core.ContentToolUse)
			if !ok {
				return nil, written, blockMismatch(
					fmt.Sprintf("%s.tool_calls.%d", base, ti), blocks, next)
			}
			if b.ToolUse == nil || b.ToolUse.Input == nil {
				continue
			}
			args, err := json.Marshal(b.ToolUse.Input)
			if err != nil {
				return nil, written, fmt.Errorf("canonical request rewrite: tool-call arguments: %w", err)
			}
			p := fmt.Sprintf("%s.tool_calls.%d.function.arguments", base, ti)
			if out, err = setIfChanged(out, p, string(args), &written); err != nil {
				return nil, written, err
			}
		}

		// Blocks the walk never reached would carry redactions nobody wrote.
		if next != len(blocks) {
			return nil, written, fmt.Errorf(
				"canonical request rewrite %s: payload has %d blocks but the wire offered %d slots — "+
					"the payload was not decoded from this body", base, len(blocks), next)
		}
	}
	return out, written, nil
}

// dropReplayCarrierAfterReasoningEdit removes the provider-private exact-replay
// carrier from a message whose reasoning a redaction just changed.
//
// `nexus_thinking` holds Anthropic's thinking blocks WITH their signatures, so
// the same text exists twice in a canonical body: once in `reasoning_content`,
// which the decode reads and a redaction rewrites, and once inside the carrier,
// which no codec decodes and no rewriter touches. As long as nothing edits
// reasoning the two agree, and every consumer that prefers the carrier is
// right to.
//
// A redaction breaks that. The consumers still prefer the carrier — the request
// leg rebuilds native thinking blocks from it (reconstructThinkingBlocks), and
// the Anthropic stream encoder explicitly DISCARDS its reasoning buffer when a
// carrier is present, saying "the signed carrier republishes the same text this
// stream already put on ReasoningDelta". So the masked copy is dropped and the
// original is delivered, under an audit row that says redacted. That is worse
// than not scanning reasoning at all, which is what this codebase did before
// reasoning reached the hooks.
//
// The carrier cannot be redacted in place: its signature is computed over the
// original text, so masked text plus the old signature is either rejected
// upstream or, worse, accepted as a forged pairing. Dropping it is the honest
// outcome — the redaction already invalidated the signature, and what is lost
// is exact replay of a turn whose text is no longer exact.
func dropReplayCarrierAfterReasoningEdit(body []byte, base string) ([]byte, error) {
	if !gjson.GetBytes(body, base+".nexus_thinking").Exists() {
		return body, nil
	}
	out, err := sjson.DeleteBytes(body, base+".nexus_thinking")
	if err != nil {
		return nil, fmt.Errorf("canonical rewrite %s: dropping the replay carrier a reasoning "+
			"redaction invalidated: %w", base, err)
	}
	return out, nil
}
