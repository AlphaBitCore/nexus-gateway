package streaming

// The four per-provider usage accumulators. The contract they implement, the
// factory that dispatches to them, and the tokenizer fallback they share live in
// usage.go; this file is only the provider-specific extraction, which is where
// every wire quirk lands and where the file therefore grows.
//
// A byte-level "the key literal is absent, so the key is absent" gate was added
// here and REVERTED as unsound: gjson decodes \uXXXX in KEY names before matching a
// path, so `{"\u0075sage":…}` is found by gjson.Get(data, "usage") while the raw
// bytes contain no "usage". The gate skipped the walk and silently lost tokens on
// VALID JSON, which is a billing defect because these counters feed cost
// estimation. Full write-up in docs/handoffs/perf-compliance-agent-program.md.
//
// What survives is the cheap discriminator that does NOT come from the JSON:
// anthropicAccumulator switches on evt.Event, which the SSE parser read from the
// `event:` line, before spending a validity scan. That carries the whole measured
// win and cannot be defeated by anything inside the payload.
//
// gjson.Valid stays in all three: on malformed input gjson still returns plausible
// typed results, and trusting one would corrupt the token counts.

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// openaiAccumulator extracts tier-1 usage from OpenAI-compatible streaming
// chunks. The provider sends `data: {..., "usage": {...}}` as the last JSON
// frame before `data: [DONE]` when `stream_options.include_usage` is set.
// The accumulator also buffers `choices[*].delta.content` for tokenizer
// fallback when usage is not included.
type openaiAccumulator struct {
	tokenizer  Tokenizer
	model      string
	prompt     *int
	completion *int
	textBuf    strings.Builder
	promptText string // captured from the first echo of `messages` if present
}

func (a *openaiAccumulator) Feed(evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	data := evt.Data
	if !gjson.Valid(data) {
		return
	}
	if u := gjson.Get(data, "usage"); u.Exists() {
		if p := u.Get("prompt_tokens"); p.Exists() && p.Type == gjson.Number {
			v := int(p.Int())
			a.prompt = &v
		}
		if c := u.Get("completion_tokens"); c.Exists() && c.Type == gjson.Number {
			v := int(c.Int())
			a.completion = &v
		}
	}
	gjson.Get(data, "choices").ForEach(func(_, choice gjson.Result) bool {
		if t := choice.Get("delta.content"); t.Exists() && t.Type == gjson.String {
			a.textBuf.WriteString(t.Str)
		}
		return true
	})
}

func (a *openaiAccumulator) Finalize(ctx context.Context) traffic.UsageMeta {
	if a.prompt != nil || a.completion != nil {
		return traffic.UsageMeta{
			PromptTokens:     a.prompt,
			CompletionTokens: a.completion,
			Status:           traffic.UsageStatusStreamingReported,
		}
	}
	return estimateWithTokenizer(ctx, a.tokenizer, a.promptText, a.textBuf.String())
}

// anthropicAccumulator extracts tier-1 usage from Anthropic Messages streaming.
// The first `message_start` frame carries `message.usage.input_tokens`; each
// `message_delta` frame carries a cumulative `usage.output_tokens`. The final
// value wins. `content_block_delta.delta.text` is captured for fallback.
type anthropicAccumulator struct {
	tokenizer  Tokenizer
	model      string
	prompt     *int
	completion *int
	textBuf    strings.Builder
	promptText string
}

func (a *anthropicAccumulator) Feed(evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	data := evt.Data
	// Switch on the event name BEFORE validating. An Anthropic stream also carries
	// ping / content_block_start / content_block_stop / message_stop frames this
	// accumulator reads nothing from, and validating them was a full scan per frame
	// whose result the switch's default arm then discarded.
	//
	// This discriminator is sound where a byte-level one is not: evt.Event comes
	// from the SSE `event:` line, which the parser read outside the JSON, so
	// nothing inside the payload can disguise it. The ordering change is invisible
	// to behaviour by construction — a frame this switch ignores was ignored before
	// too — so it is pinned by BenchmarkAB_Feed_AnthropicIgnoredEvent, not by a
	// unit test. Removing the hoist cannot fail a correctness test, only that
	// benchmark.
	switch evt.Event {
	case "message_start", "message_delta", "content_block_delta":
	default:
		return
	}
	if !gjson.Valid(data) {
		return
	}
	switch evt.Event {
	case "message_start":
		if v := gjson.Get(data, "message.usage.input_tokens"); v.Exists() && v.Type == gjson.Number {
			val := int(v.Int())
			a.prompt = &val
		}
		if v := gjson.Get(data, "message.usage.output_tokens"); v.Exists() && v.Type == gjson.Number {
			val := int(v.Int())
			a.completion = &val
		}
	case "message_delta":
		if v := gjson.Get(data, "usage.output_tokens"); v.Exists() && v.Type == gjson.Number {
			val := int(v.Int())
			a.completion = &val
		}
	case "content_block_delta":
		if t := gjson.Get(data, "delta.text"); t.Exists() && t.Type == gjson.String {
			a.textBuf.WriteString(t.Str)
		}
	}
}

func (a *anthropicAccumulator) Finalize(ctx context.Context) traffic.UsageMeta {
	if a.prompt != nil || a.completion != nil {
		return traffic.UsageMeta{
			PromptTokens:     a.prompt,
			CompletionTokens: a.completion,
			Status:           traffic.UsageStatusStreamingReported,
		}
	}
	return estimateWithTokenizer(ctx, a.tokenizer, a.promptText, a.textBuf.String())
}

// geminiAccumulator extracts tier-1 usage from Gemini streaming chunks.
// Gemini emits `usageMetadata` in the final chunk (sometimes mid-stream).
// `candidates[*].content.parts[*].text` is captured for fallback.
type geminiAccumulator struct {
	tokenizer  Tokenizer
	model      string
	prompt     *int
	completion *int
	textBuf    strings.Builder
	promptText string
}

func (a *geminiAccumulator) Feed(evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	data := evt.Data
	if !gjson.Valid(data) {
		return
	}
	if u := gjson.Get(data, "usageMetadata"); u.Exists() {
		if p := u.Get("promptTokenCount"); p.Exists() && p.Type == gjson.Number {
			v := int(p.Int())
			a.prompt = &v
		}
		// completionTokens = candidatesTokenCount (text) + thoughtsTokenCount (reasoning)
		// so that total_tokens = prompt_tokens + completion_tokens holds.
		var candidates, thoughts int
		if c := u.Get("candidatesTokenCount"); c.Exists() && c.Type == gjson.Number {
			candidates = int(c.Int())
		}
		if t := u.Get("thoughtsTokenCount"); t.Exists() && t.Type == gjson.Number {
			thoughts = int(t.Int())
		}
		total := candidates + thoughts
		a.completion = &total
	}
	gjson.Get(data, "candidates").ForEach(func(_, cand gjson.Result) bool {
		cand.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() && t.Type == gjson.String {
				a.textBuf.WriteString(t.Str)
			}
			return true
		})
		return true
	})
}

func (a *geminiAccumulator) Finalize(ctx context.Context) traffic.UsageMeta {
	if a.prompt != nil || a.completion != nil {
		return traffic.UsageMeta{
			PromptTokens:     a.prompt,
			CompletionTokens: a.completion,
			Status:           traffic.UsageStatusStreamingReported,
		}
	}
	return estimateWithTokenizer(ctx, a.tokenizer, a.promptText, a.textBuf.String())
}

// bufferingAccumulator is the generic fallback: captures the concatenated
// `evt.Data` strings and runs the tokenizer at finalize time. Used for
// Bedrock non-anthropic model families and anywhere a provider-specific
// extractor is not yet written.
type bufferingAccumulator struct {
	tokenizer Tokenizer
	model     string
	textBuf   strings.Builder
}

func (a *bufferingAccumulator) Feed(evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	a.textBuf.WriteString(evt.Data)
}

func (a *bufferingAccumulator) Finalize(ctx context.Context) traffic.UsageMeta {
	return estimateWithTokenizer(ctx, a.tokenizer, "", a.textBuf.String())
}
