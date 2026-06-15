// openai_chat_stream.go — the SSE stream fold for OpenAI Chat
// Completions: per-choice delta accumulation (text / reasoning /
// tool-call fragments), usage-frame merging, and frame-coverage
// confidence. Split from openai_chat.go, which keeps the non-stream
// decode paths; the layout mirrors anthropic_stream.go.
package codecs

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// streamChoiceState accumulates a single OpenAI streaming choice across
// SSE events. text holds the concatenated delta.content; reasoning
// holds the concatenated delta.reasoning_content (DeepSeek/Moonshot
// chain-of-thought stream); toolCallsBuf stitches together
// delta.tool_calls[].function.arguments fragments keyed by tool-call
// index.
type streamChoiceState struct {
	role         string
	text         strings.Builder
	reasoning    strings.Builder
	finishReason string
	toolCalls    map[int]*openAIToolCall
	toolOrder    []int
}

func (n *OpenAIChatNormalizer) normalizeStreamResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "openai-chat",
		Model:            meta.Model,
		Stream:           true,
	}

	// SSE events are separated by blank lines. Each event has one or more
	// `data: <payload>` lines; the value is JSON (or the literal [DONE]).
	choices := map[int]*streamChoiceState{}
	choiceOrder := []int{}
	var usage *core.Usage
	var sawAny bool
	// Frame coverage: recognized / total data frames ([DONE] and blank
	// data lines are protocol sentinels, counted in neither). A frame is
	// recognized when it carries the chat-completion chunk ENVELOPE —
	// any of the id / object / created / model / choices / usage keys
	// being present counts, value-empty or not, so a content-filter
	// prologue frame ({"id":"","choices":[],...}) or an empty-choices
	// heartbeat does not drag a real stream below the registry claim
	// threshold. Unparseable JSON — typically the cut-off final frame of
	// a truncated capture — counts toward the total only, so a truncated
	// stream folds to its decodable prefix with proportionally lower
	// confidence instead of erroring.
	var totalFrames, recognizedFrames int

	pumpChunk := func(_, payload string) {
		if payload == "" || payload == "[DONE]" {
			return
		}
		totalFrames++
		var probe struct {
			ID      *string         `json:"id"`
			Object  *string         `json:"object"`
			Created *int64          `json:"created"`
			Model   string          `json:"model"`
			Choices json.RawMessage `json:"choices"`
			Usage   *openAIUsage    `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			// Counts toward total only — the truncated tail of a cut-off
			// capture must not error away the decodable prefix.
			return
		}
		if probe.ID != nil || probe.Object != nil || probe.Created != nil ||
			probe.Model != "" || probe.Usage != nil || probe.Choices != nil {
			recognizedFrames++
		}
		chunk := struct {
			Model   string
			Choices []openAIChatChoice
			Usage   *openAIUsage
		}{Model: probe.Model, Usage: probe.Usage}
		if len(probe.Choices) > 0 && string(probe.Choices) != "null" {
			if err := json.Unmarshal(probe.Choices, &chunk.Choices); err != nil {
				// Malformed choices tree: envelope already counted; fold
				// whatever other fields decoded and move on.
				chunk.Choices = nil
			}
		}
		if out.Model == "" && chunk.Model != "" {
			out.Model = chunk.Model
		}
		if chunk.Usage != nil {
			if u := chunk.Usage.extractCanonicalUsage(); u != nil {
				usage = u
				// A usage-only terminal frame (stream_options'
				// include_usage final flush carrying no choices) is a
				// valid stream by itself; without marking sawAny the
				// parse bails to ErrUnsupported and the row falls to the
				// generic-http fallback. Mirrors the gemini fold's
				// usageMetadata-only handling.
				sawAny = true
			}
		}
		for _, ch := range chunk.Choices {
			st, ok := choices[ch.Index]
			if !ok {
				st = &streamChoiceState{toolCalls: map[int]*openAIToolCall{}}
				choices[ch.Index] = st
				choiceOrder = append(choiceOrder, ch.Index)
			}
			if ch.Delta != nil {
				if ch.Delta.Role != "" && st.role == "" {
					st.role = ch.Delta.Role
					// A role delta is itself an unambiguous signal that
					// this is a real chat-completion stream — even when the
					// assistant turn carries no visible text (reasoning
					// models like o3 / gpt-5.5 that spent the whole budget
					// on hidden reasoning the provider did not echo emit a
					// role delta + a finish_reason chunk and nothing else).
					// Without marking sawAny here, such a turn parsed to
					// zero blocks, tripped the "no chunks decoded"
					// ErrUnsupported below, and fell through to the Tier-3
					// generic-http verbatim fallback — producing a payload
					// with NO messages[], so response_normalized.messages[0]
					// .content was absent (NULL) instead of an empty array.
					sawAny = true
				}
				if len(ch.Delta.Content) > 0 && string(ch.Delta.Content) != "null" {
					var s string
					if err := json.Unmarshal(ch.Delta.Content, &s); err == nil {
						st.text.WriteString(s)
						sawAny = true
					}
				}
				if r := firstNonEmptyString(ch.Delta.ReasoningContent, ch.Delta.Reasoning); r != "" {
					st.reasoning.WriteString(r)
					sawAny = true
				}
				for pos, tc := range ch.Delta.ToolCalls {
					idx := indexOfToolCall(tc, pos)
					existing, ok := st.toolCalls[idx]
					if !ok {
						existing = &openAIToolCall{}
						st.toolCalls[idx] = existing
						st.toolOrder = append(st.toolOrder, idx)
					}
					if tc.ID != "" {
						existing.ID = tc.ID
					}
					if tc.Type != "" {
						existing.Type = tc.Type
					}
					if tc.Function.Name != "" {
						existing.Function.Name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						existing.Function.Arguments += tc.Function.Arguments
					}
					sawAny = true
				}
			}
			if ch.FinishReason != "" {
				st.finishReason = ch.FinishReason
				// A finish_reason on a recognized chat-completion choice is
				// also a definitive structural signal — covers providers
				// that stream an empty assistant turn as a single chunk
				// carrying only choices[].finish_reason (no role delta).
				sawAny = true
			}
		}
	}

	if scanErr := walkSSEFrames(raw, pumpChunk); scanErr != nil {
		// The scanner stopped before the end of the capture (oversized
		// line); the unread tail is at least one frame we could not
		// decode, so it weighs on the coverage like one. Mirrors the
		// anthropic fold — the decodable prefix still folds.
		totalFrames++
	}

	if !sawAny {
		return out, fmt.Errorf("openai-chat: no chunks decoded: %w", core.ErrUnsupported)
	}
	if totalFrames > 0 {
		out.Confidence = float64(recognizedFrames) / float64(totalFrames)
	}

	for _, idx := range choiceOrder {
		st := choices[idx]
		role := st.role
		if role == "" {
			role = string(core.RoleAssistant)
		}
		msg := core.Message{Role: roleFromString(role), FinishReason: st.finishReason}
		if r := st.reasoning.String(); r != "" {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentReasoning, Text: r})
		}
		if t := st.text.String(); t != "" {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentText, Text: t})
		}
		for _, tidx := range st.toolOrder {
			// toolOrder indices are appended exactly when the map entry
			// is created with a non-nil value, so the lookup always hits.
			tc := st.toolCalls[tidx]
			var input map[string]any
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			msg.Content = append(msg.Content, core.ContentBlock{
				Type: core.ContentToolUse,
				ToolUse: &core.ToolUse{
					CallID: tc.ID,
					Name:   tc.Function.Name,
					Input:  input,
				},
			})
		}
		out.Messages = append(out.Messages, msg)
		if out.FinishReason == "" {
			out.FinishReason = st.finishReason
		}
	}
	out.Usage = usage
	// Moonshot reasoning derivation (stream variant): same rationale as
	// the non-stream path — when the wire usage block omitted reasoning
	// _tokens but we accumulated reasoning_content deltas, surface a
	// chars/3.5 heuristic so the canonical Usage carries a non-zero
	// value. Sum across all choices.
	reasoningChars := 0
	for _, st := range choices {
		reasoningChars += st.reasoning.Len()
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
	return out, nil
}

// indexOfToolCall returns the aggregation-map key for a streaming tool-call
// delta. The OpenAI spec places the key in tool_calls[i].index on the delta
// object; when that field is present it takes precedence because it is stable
// across chunks (a single tool call may span many SSE events, each carrying
// only an arguments fragment with the same index). When the field is absent
// (non-compliant or synthetic streams) we fall back to pos — the ordinal
// position of this item within the current chunk's tool_calls slice — which
// preserves the original intent of "position in the slice as it appears in
// this chunk".
func indexOfToolCall(tc openAIToolCall, pos int) int {
	if tc.Index != nil {
		return *tc.Index
	}
	return pos
}
