// Package canonicalbridge_test — same-format identity across the canonical waist.
//
// Named failure modes:
//   - the re-encoded stream carries LESS than the upstream sent (a channel was dropped)
//   - the re-encoded stream carries MORE than the upstream sent (a channel was invented,
//     or one channel's text was duplicated into another)
package canonicalbridge_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestOpenAIStreamIsIdentityAcrossCanonical asserts the strong property for the
// case where it is actually available: ingress wire == egress wire.
//
//	upstream frames → decoder → canonical chunk → encoder → frames
//
// Byte identity is not the bar and never can be — the encoder mints its own id and
// created stamp and is free to re-split frames. What must hold is that each
// CHANNEL accumulates to exactly the same text. Dropping a channel and inventing
// one are both failures here, and the second matters as much as the first: routing
// a refusal into the content channel, say, would keep every byte and still tell the
// client the model answered when it declined.
//
// Cross-format pairs get a different (weaker) criterion in
// TestCrossSpecStreamFidelity: two different wires cannot be identical, so there
// the question is only whether content survives.
func TestOpenAIStreamIsIdentityAcrossCanonical(t *testing.T) {
	for _, corpus := range []string{
		"openai_tools",
		"openai_reasoning",
		"openai_multichoice",
		"openai_refusal",
		"deepseek_reasoning",
		"moonshot_tools",
	} {
		t.Run(corpus, func(t *testing.T) {
			raw := readCorpus(t, corpus)
			tc := fidelityCase{
				corpus: corpus, ingress: provcore.FormatOpenAI,
				shape: typology.WireShapeOpenAIChat, open: openaiOpen,
			}
			_, wire := runChain(t, tc, raw)

			before := accumulateOpenAIChannels(raw)
			after := accumulateOpenAIChannels(wire)

			for _, ch := range []string{"content", "reasoning_content", "refusal", "tool_names", "tool_arguments", "finish_reason"} {
				got, want := after[ch], before[ch]
				if got == want {
					continue
				}
				switch {
				case want != "" && got == "":
					t.Errorf("%s: upstream sent %d bytes, re-encode carries none — the channel is dropped between the decoder and the encoder",
						ch, len(want))
				case want == "" && got != "":
					t.Errorf("%s: upstream sent nothing, re-encode invents %q — a channel the model never used must not appear",
						ch, clip(got))
				default:
					t.Errorf("%s: re-encode differs\n  upstream:  %q\n  re-encode: %q", ch, clip(want), clip(got))
				}
			}
		})
	}
}

// TestAnthropicStreamIsIdentityAcrossCanonical is the same identity property for
// the Anthropic wire, and it is the one that catches DUPLICATION.
//
// Anthropic's decoder publishes the same thinking text twice — once per
// thinking_delta on the canonical reasoning channel, and again in full through
// the signed nexus_thinking carrier when signature_delta closes the block. An
// encoder that emits both sends the reasoning twice, which a containment check
// cannot see: every value is still present. Accumulating each channel and
// comparing the whole string is what makes a doubled transcript fail.
func TestAnthropicStreamIsIdentityAcrossCanonical(t *testing.T) {
	for _, corpus := range []string{
		"anthropic_thinking_tools",
		"anthropic_thinking_text",
		"anthropic_thinking_block",
	} {
		t.Run(corpus, func(t *testing.T) {
			raw := readCorpus(t, corpus)
			tc := fidelityCase{
				corpus: corpus, ingress: provcore.FormatAnthropic,
				shape: typology.WireShapeAnthropicMessages, open: anthropicOpen,
			}
			_, wire := runChain(t, tc, raw)

			before := accumulateAnthropicChannels(raw)
			after := accumulateAnthropicChannels(wire)

			for _, ch := range []string{"text", "thinking", "tool_names", "tool_arguments"} {
				got, want := after[ch], before[ch]
				if got == want {
					continue
				}
				switch {
				case want != "" && got == "":
					t.Errorf("%s: upstream sent %d bytes, re-encode carries none", ch, len(want))
				case strings.Contains(got, want) && len(got) > len(want):
					t.Errorf("%s: re-encode DUPLICATES the channel — upstream %d bytes, re-encode %d",
						ch, len(want), len(got))
				default:
					t.Errorf("%s: re-encode differs\n  upstream:  %q\n  re-encode: %q", ch, clip(want), clip(got))
				}
			}
		})
	}
}

// TestResponsesStreamIsIdentityAcrossCanonical holds the /v1/responses wire to
// the same bar as the other two.
//
// This grammar is the one where a containment check is weakest: the answer and
// the reasoning summary BOTH arrive as a bare `delta` string, told apart only by
// their event name. A re-encode that routed the reasoning into output_text would
// keep every byte and still pass a containment assertion, while showing the user
// the model's private reasoning as its answer. Splitting the accumulator by
// event name is what makes that visible.
func TestResponsesStreamIsIdentityAcrossCanonical(t *testing.T) {
	for _, corpus := range []string{
		"openai_responses_tools",
		"openai_responses_reasoning",
	} {
		t.Run(corpus, func(t *testing.T) {
			raw := readCorpus(t, corpus)
			tc := fidelityCase{
				corpus: corpus, ingress: provcore.FormatOpenAIResponses,
				shape: typology.WireShapeOpenAIResponses, open: openaiOpen,
			}
			_, wire := runChain(t, tc, raw)

			before := accumulateResponsesChannels(raw)
			after := accumulateResponsesChannels(wire)

			for _, ch := range []string{"output_text", "reasoning_summary", "function_call_arguments", "function_name"} {
				got, want := after[ch], before[ch]
				if got == want {
					continue
				}
				switch {
				case want != "" && got == "":
					t.Errorf("%s: upstream sent %d bytes, re-encode carries none", ch, len(want))
				case want == "" && got != "":
					t.Errorf("%s: upstream sent nothing, re-encode invents %q — text moved onto a channel "+
						"the model never used", ch, clip(got))
				default:
					t.Errorf("%s: re-encode differs\n  upstream:  %q\n  re-encode: %q", ch, clip(want), clip(got))
				}
			}
		})
	}
}

// TestGeminiStreamIsIdentityAcrossCanonical and its Cohere sibling close the
// last two chat ingresses.
//
// Containment is not enough for either. Gemini distinguishes reasoning from the
// answer with a boolean on the part rather than a separate field, and Cohere
// with the type of the content block — so on both wires a re-encode can move
// every byte onto the wrong channel and still contain them all. Reading the
// discriminator into the channel key is what makes that a failure.
func TestGeminiStreamIsIdentityAcrossCanonical(t *testing.T) {
	for _, corpus := range []string{"gemini_tools", "gemini_thinking"} {
		t.Run(corpus, func(t *testing.T) {
			raw := readCorpus(t, corpus)
			tc := fidelityCase{
				corpus: corpus, ingress: provcore.FormatGemini,
				shape: typology.WireShapeGeminiGenerateContent, open: geminiOpen,
			}
			_, wire := runChain(t, tc, raw)
			assertChannelsEqual(t, accumulateGeminiChannels(raw), accumulateGeminiChannels(wire),
				"answer_text", "thought_text", "function_name", "function_args")
		})
	}
}

func TestCohereStreamIsIdentityAcrossCanonical(t *testing.T) {
	for _, corpus := range []string{"cohere_tools", "cohere_reasoning"} {
		t.Run(corpus, func(t *testing.T) {
			raw := readCorpus(t, corpus)
			tc := fidelityCase{
				corpus: corpus, ingress: provcore.FormatCohere,
				shape: typology.WireShapeCohereChat, open: cohereOpen,
			}
			_, wire := runChain(t, tc, raw)
			assertChannelsEqual(t, accumulateCohereChannels(raw), accumulateCohereChannels(wire),
				"text", "thinking", "tool_plan", "tool_name", "tool_args")
		})
	}
}

// assertChannelsEqual reports the three ways a re-encode can differ: it dropped
// the channel, it invented one, or it changed the text.
func assertChannelsEqual(t *testing.T, before, after map[string]string, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		got, want := after[ch], before[ch]
		if got == want {
			continue
		}
		switch {
		case want != "" && got == "":
			t.Errorf("%s: upstream sent %d bytes, re-encode carries none", ch, len(want))
		case want == "" && got != "":
			t.Errorf("%s: upstream sent nothing, re-encode invents %q — text moved onto a channel "+
				"the model never used", ch, clip(got))
		case strings.Contains(got, want):
			t.Errorf("%s: re-encode DUPLICATES or pads the channel — upstream %d bytes, re-encode %d",
				ch, len(want), len(got))
		default:
			t.Errorf("%s: re-encode differs\n  upstream:  %q\n  re-encode: %q", ch, clip(want), clip(got))
		}
	}
}

// accumulateGeminiChannels splits parts by their `thought` flag, which is the
// only thing separating Gemini's reasoning from its answer.
func accumulateGeminiChannels(sse string) map[string]string {
	out := map[string]*strings.Builder{}
	add := func(k, v string) {
		if v == "" {
			return
		}
		b, ok := out[k]
		if !ok {
			b = &strings.Builder{}
			out[k] = b
		}
		b.WriteString(v)
	}
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var frame struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						Thought      bool   `json:"thought"`
						FunctionCall *struct {
							Name string         `json:"name"`
							Args map[string]any `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame) != nil {
			continue
		}
		for _, cand := range frame.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Thought {
					add("thought_text", part.Text)
				} else {
					add("answer_text", part.Text)
				}
				if part.FunctionCall != nil {
					add("function_name", part.FunctionCall.Name)
					if args, err := json.Marshal(part.FunctionCall.Args); err == nil && string(args) != "null" {
						add("function_args", string(args))
					}
				}
			}
		}
	}
	flat := make(map[string]string, len(out))
	for k, b := range out {
		flat[k] = b.String()
	}
	return flat
}

// accumulateCohereChannels keys off the content block's type, which is what
// separates a reasoning run from the answer on this wire.
func accumulateCohereChannels(sse string) map[string]string {
	out := map[string]*strings.Builder{}
	add := func(k, v string) {
		if v == "" {
			return
		}
		b, ok := out[k]
		if !ok {
			b = &strings.Builder{}
			out[k] = b
		}
		b.WriteString(v)
	}
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Message struct {
					ToolPlan string `json:"tool_plan"`
					Content  struct {
						Text     string `json:"text"`
						Thinking string `json:"thinking"`
					} `json:"content"`
					ToolCalls struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content-delta":
			add("text", ev.Delta.Message.Content.Text)
			add("thinking", ev.Delta.Message.Content.Thinking)
		case "tool-plan-delta":
			add("tool_plan", ev.Delta.Message.ToolPlan)
		case "tool-call-start":
			add("tool_name", ev.Delta.Message.ToolCalls.Function.Name)
		case "tool-call-delta":
			add("tool_args", ev.Delta.Message.ToolCalls.Function.Arguments)
		}
	}
	flat := make(map[string]string, len(out))
	for k, b := range out {
		flat[k] = b.String()
	}
	return flat
}

// accumulateResponsesChannels folds a /v1/responses SSE stream into one string
// per channel, keyed by EVENT NAME because the payload field does not identify
// the channel — output_text, the reasoning summary and tool arguments all arrive
// as `delta`.
func accumulateResponsesChannels(sse string) map[string]string {
	out := map[string]*strings.Builder{}
	add := func(k, v string) {
		if v == "" {
			return
		}
		b, ok := out[k]
		if !ok {
			b = &strings.Builder{}
			out[k] = b
		}
		b.WriteString(v)
	}
	byEvent := map[string]string{
		"response.output_text.delta":             "output_text",
		"response.reasoning_summary_text.delta":  "reasoning_summary",
		"response.reasoning_text.delta":          "reasoning_summary",
		"response.function_call_arguments.delta": "function_call_arguments",
	}
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				Name string `json:"name"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		if ch, ok := byEvent[ev.Type]; ok {
			add(ch, ev.Delta)
		}
		if ev.Type == "response.output_item.added" {
			add("function_name", ev.Item.Name)
		}
	}
	flat := make(map[string]string, len(out))
	for k, b := range out {
		flat[k] = b.String()
	}
	return flat
}

// accumulateAnthropicChannels folds an Anthropic Messages SSE stream into one
// string per channel. Block boundaries are erased for the same reason the OpenAI
// fold erases frame boundaries: re-splitting a run is a valid re-encode.
func accumulateAnthropicChannels(sse string) map[string]string {
	out := map[string]*strings.Builder{}
	add := func(k, v string) {
		if v == "" {
			return
		}
		b, ok := out[k]
		if !ok {
			b = &strings.Builder{}
			out[k] = b
		}
		b.WriteString(v)
	}
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Text  string `json:"text"`
				Think string `json:"thinking"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			add("tool_names", ev.ContentBlock.Name)
			add("text", ev.ContentBlock.Text)
			add("thinking", ev.ContentBlock.Think)
		case "content_block_delta":
			add("text", ev.Delta.Text)
			add("thinking", ev.Delta.Thinking)
			add("tool_arguments", ev.Delta.PartialJSON)
		}
	}
	flat := make(map[string]string, len(out))
	for k, b := range out {
		flat[k] = b.String()
	}
	return flat
}

// accumulateOpenAIChannels folds an OpenAI chat-completions SSE stream into one
// string per channel, in arrival order. Frame boundaries are deliberately erased:
// the encoder may re-split a run of deltas, and that is a valid re-encode.
func accumulateOpenAIChannels(sse string) map[string]string {
	out := map[string]*strings.Builder{}
	add := func(k, v string) {
		if v == "" {
			return
		}
		b, ok := out[k]
		if !ok {
			b = &strings.Builder{}
			out[k] = b
		}
		b.WriteString(v)
	}
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					Refusal          string `json:"refusal"`
					ToolCalls        []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		for _, c := range frame.Choices {
			add("content", c.Delta.Content)
			reasoning := c.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = c.Delta.Reasoning
			}
			add("reasoning_content", reasoning)
			add("refusal", c.Delta.Refusal)
			for _, tc := range c.Delta.ToolCalls {
				add("tool_names", tc.Function.Name)
				add("tool_arguments", tc.Function.Arguments)
			}
			add("finish_reason", c.FinishReason)
		}
	}
	flat := make(map[string]string, len(out))
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		flat[k] = out[k].String()
	}
	return flat
}
