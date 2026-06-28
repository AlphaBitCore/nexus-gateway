package codecs

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// LooksLikeAnthropicSSE reports whether raw opens with the canonical
// first frame of an Anthropic /v1/messages event stream: an
// `event: message_start` line, or a `data:` line whose JSON payload
// begins with {"type":"message_start". The sniff lets the codec claim
// SSE bodies even when the producer lost the stream flag and the
// Content-Type header, and is cheap enough to run on every response
// body. OpenAI-style streams (first data payload has no
// message_start type) and plain JSON bodies do not match.
func LooksLikeAnthropicSSE(raw []byte) bool {
	s := bytes.TrimLeft(raw, " \t\r\n")
	if rest, ok := bytes.CutPrefix(s, []byte("event:")); ok {
		return bytes.HasPrefix(bytes.TrimLeft(rest, " "), []byte("message_start"))
	}
	if rest, ok := bytes.CutPrefix(s, []byte("data:")); ok {
		return bytes.HasPrefix(bytes.TrimLeft(rest, " "), []byte(`{"type":"message_start"`))
	}
	return false
}

// streamBlockState accumulates one content block as deltas arrive.
type streamBlockState struct {
	blockType string
	text      strings.Builder
	tool      *core.ToolUse
	toolJSON  strings.Builder
}

// foldAnthropicSSE folds a complete or truncated Anthropic /v1/messages
// event stream into the single assistant turn it carries.
//
// Wire vocabulary folded: message_start (model + input-side usage),
// content_block_start (block type, tool id/name), content_block_delta
// (text_delta / thinking_delta / input_json_delta / signature_delta),
// message_delta (stop_reason + output-side usage), and the no-op control
// frames content_block_stop / message_stop / ping. Content blocks are
// emitted in stream order: text_delta runs become a text block,
// thinking_delta runs a reasoning block, input_json_delta runs a
// tool_use block whose accumulated JSON is decoded into Input.
//
// Usage follows the canonical convention: PromptTokens is the TOTAL
// input (input_tokens + cache_read_input_tokens +
// cache_creation_input_tokens), CompletionTokens is output_tokens, and
// the two cache counters are surfaced separately; the input side
// arrives on message_start and the output side on message_delta, so
// both frames merge into one running Usage.
//
// Confidence is frame coverage: recognized frames / total data frames.
// A frame is recognized when its type (and, for content_block_delta,
// its delta type) is in the vocabulary above and carries the expected
// payload field; unparseable JSON — typically the cut-off final frame
// of a truncated capture — and unknown types count toward the total
// only. A truncated stream therefore folds to its decodable prefix
// with proportionally lower confidence instead of an error. When NO
// frame is recognized the body is not an Anthropic stream and the
// codec declines with core.ErrUnsupported so the registry walk
// continues.
func foldAnthropicSSE(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "anthropic-messages",
		Model:            meta.Model,
		Stream:           true,
	}

	var (
		blocks       = map[int]*streamBlockState{}
		order        []int
		usage        *core.Usage
		finishReason string
		total        int
		recognized   int
	)

	blockAt := func(idx int) *streamBlockState {
		st, ok := blocks[idx]
		if !ok {
			st = &streamBlockState{}
			blocks[idx] = st
			order = append(order, idx)
		}
		return st
	}

	// foldFrame applies one decoded frame to the running state and
	// reports whether the frame was recognized.
	foldFrame := func(chunk map[string]any) bool {
		etype, _ := chunk["type"].(string)
		switch etype {
		case "message_start":
			if msg, ok := chunk["message"].(map[string]any); ok {
				if m, _ := msg["model"].(string); m != "" {
					out.Model = m
				}
				if u, ok := msg["usage"].(map[string]any); ok {
					usage = mergeAnthropicUsage(usage, u)
				}
			}
			return true
		case "content_block_start":
			st := blockAt(intFromAny(chunk["index"]))
			if cb, ok := chunk["content_block"].(map[string]any); ok {
				st.blockType, _ = cb["type"].(string)
				if st.blockType == "tool_use" {
					tu := &core.ToolUse{}
					tu.CallID, _ = cb["id"].(string)
					tu.Name, _ = cb["name"].(string)
					st.tool = tu
				}
			}
			return true
		case "content_block_delta":
			d, _ := chunk["delta"].(map[string]any)
			if d == nil {
				return false
			}
			st := blockAt(intFromAny(chunk["index"]))
			dtype, _ := d["type"].(string)
			switch dtype {
			case "text_delta":
				if s, ok := d["text"].(string); ok {
					st.text.WriteString(s)
					return true
				}
			case "thinking_delta":
				// Some SDK captures carry the reasoning text under
				// "text" instead of "thinking".
				for _, key := range []string{"thinking", "text"} {
					if s, ok := d[key].(string); ok {
						st.text.WriteString(s)
						st.blockType = "thinking"
						return true
					}
				}
			case "input_json_delta":
				if s, ok := d["partial_json"].(string); ok {
					// A head-truncated capture can open with input_json_delta
					// frames whose content_block_start was lost, leaving the
					// block typeless. Open an implicit tool_use block (empty
					// CallID/Name — the identity rode on the lost start frame)
					// rather than dropping the accumulated input at stitch
					// time: the tool INPUT is the audit-critical half ("what
					// did the agent run on the host"), and counting the frame
					// as recognized while silently emitting no content would
					// overstate fidelity. The decoded-but-anonymous block
					// states honestly what survived the truncation.
					if st.blockType == "" {
						st.blockType = "tool_use"
						st.tool = &core.ToolUse{}
					}
					st.toolJSON.WriteString(s)
					return true
				}
			case "signature_delta":
				// Thinking-block integrity signature — carries no audit
				// content but is part of the recognized vocabulary.
				return true
			}
			return false
		case "message_delta":
			if d, ok := chunk["delta"].(map[string]any); ok {
				if r, _ := d["stop_reason"].(string); r != "" {
					finishReason = r
				}
			}
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = mergeAnthropicUsage(usage, u)
				// The wire breaks the thinking share of output_tokens out
				// under output_tokens_details; the explicit count beats the
				// char-length heuristic applied after stitching.
				if det, ok := u["output_tokens_details"].(map[string]any); ok {
					if v := intFromAny(det["thinking_tokens"]); v > 0 {
						usage.ReasoningTokens = &v
					}
				}
			}
			return true
		case "content_block_stop", "message_stop", "ping":
			// Recognized control frames with nothing to fold.
			return true
		}
		return false
	}

	scanErr := walkSSEFrames(raw, func(_, data string) {
		// Protocol sentinels count in neither numerator nor denominator —
		// the wire form here is Anthropic's, but key-missed streams that
		// transited an OpenAI-compat proxy can carry a trailing [DONE].
		if data == "" || data == "[DONE]" {
			return
		}
		total++
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Counts toward total only, so coverage confidence drops.
			return
		}
		if foldFrame(chunk) {
			recognized++
		}
	})
	if scanErr != nil {
		// The scanner stopped before the end of the capture (oversized
		// line); the unread tail is at least one frame we could not
		// decode, so it weighs on the coverage like one.
		total++
	}
	if recognized == 0 {
		return out, fmt.Errorf("anthropic-messages: no recognized stream events: %w", core.ErrUnsupported)
	}

	// Stitch the assembled blocks into a single assistant message.
	msg := core.Message{Role: core.RoleAssistant, FinishReason: finishReason}
	for _, idx := range order {
		st := blocks[idx]
		switch st.blockType {
		case "thinking":
			if t := st.text.String(); t != "" {
				msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentReasoning, Text: t})
			}
		case "tool_use":
			// blockType "tool_use" is set by content_block_start or by a
			// head-truncated input_json_delta run; both allocate st.tool
			// in the same frame that sets the type.
			tu := st.tool
			if js := st.toolJSON.String(); js != "" {
				var input map[string]any
				if err := json.Unmarshal([]byte(js), &input); err == nil {
					tu.Input = input
				}
			}
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentToolUse, ToolUse: tu})
		default:
			if t := st.text.String(); t != "" {
				msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentText, Text: t})
			}
		}
	}
	out.Messages = []core.Message{msg}
	out.FinishReason = finishReason
	out.Usage = usage

	// The API counts thinking tokens inside output_tokens; when the wire
	// did not break them out explicitly, derive a heuristic from the
	// reasoning text length (chars / 3.5, the estimator's default
	// Anthropic-family tokenizer, ±15%) so dashboards and the
	// reasoning_ratio widget see a non-zero signal instead of
	// misclassifying the row as "no reasoning happened". Same rule as the
	// non-streaming response path.
	reasoningChars := 0
	for _, b := range msg.Content {
		if b.Type == core.ContentReasoning {
			reasoningChars += len(b.Text)
		}
	}
	if reasoningChars > 0 {
		if out.Usage == nil {
			out.Usage = &core.Usage{}
		}
		if out.Usage.ReasoningTokens == nil {
			est := reasoningChars * 2 / 7
			if est < 1 {
				est = 1
			}
			out.Usage.ReasoningTokens = &est
		}
	}

	out.Confidence = float64(recognized) / float64(total)
	out.DetectedSpec = "anthropic-messages"
	return out, nil
}
