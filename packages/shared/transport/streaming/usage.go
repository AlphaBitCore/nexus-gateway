package streaming

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// UsageAccumulator aggregates LLM usage signals across the frames of a
// streaming response. The Live/Buffer pipelines feed each parsed SSE frame
// via Feed; at end of stream Finalize returns the extracted UsageMeta.
//
// Accumulators are single-use and not goroutine safe — the pipeline owns
// serial access.
type UsageAccumulator interface {
	// Feed ingests one decoded SSE frame. Idempotent on unrecognised frames.
	Feed(evt *SSEEvent)

	// Finalize returns the best-effort UsageMeta for the stream.
	// Tier 1 (`streaming_reported`) when the provider emitted usage in-band.
	// Tier 2 (`streaming_estimated`) when the accumulator fell back to a
	// tokenizer over the captured text.
	// Tier 3 (`streaming_unavailable`) when neither reporting nor estimation
	// succeeded within the pipeline's deadline.
	Finalize(ctx context.Context) traffic.UsageMeta
}

// UsageAccumulatorFactory constructs an accumulator for a (provider, model)
// pair. Unknown providers return nil so pipelines can skip wiring.
type UsageAccumulatorFactory func(providerID, model string) UsageAccumulator

// NewUsageAccumulator returns the built-in accumulator for the given
// provider, or nil if the provider has no streaming extractor.
//
// Provider IDs match the values written into `RequestMeta.Provider` by the
// traffic detect adapters ("openai", "anthropic", "gemini", "azure", "deepseek",
// "glm", "minimax", "bedrock", "vertex").
func NewUsageAccumulator(providerID, model string) UsageAccumulator {
	switch providerID {
	case "openai", "azure", "deepseek", "glm", "minimax":
		return &openaiAccumulator{tokenizer: tokenizerFor(providerID), model: model}
	case "anthropic":
		return &anthropicAccumulator{tokenizer: tokenizerFor(providerID), model: model}
	case "gemini":
		return &geminiAccumulator{tokenizer: tokenizerFor(providerID), model: model}
	case "bedrock":
		// Bedrock wraps provider-specific payloads in a Smithy envelope.
		// For `anthropic.*` models we reuse the anthropic accumulator on the
		// decoded inner chunk. Non-anthropic Bedrock families fall through
		// to the generic text-buffer tokenizer fallback below.
		if strings.HasPrefix(model, "anthropic.") {
			return &anthropicAccumulator{tokenizer: tokenizerFor("anthropic"), model: model}
		}
		return &bufferingAccumulator{tokenizer: tokenizerFor(providerID), model: model}
	case "vertex":
		// vertex Model is publisher-namespaced (e.g. "anthropic/claude-3-5-sonnet").
		if strings.HasPrefix(model, "anthropic/") {
			return &anthropicAccumulator{tokenizer: tokenizerFor("anthropic"), model: model}
		}
		if strings.HasPrefix(model, "google/") {
			return &geminiAccumulator{tokenizer: tokenizerFor("gemini"), model: model}
		}
		return nil
	}
	return nil
}

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

// estimateWithTokenizer runs the tokenizer with a bounded deadline. On
// success returns StreamingEstimated; on deadline/error returns
// StreamingUnavailable.
func estimateWithTokenizer(ctx context.Context, tok Tokenizer, prompt, completion string) traffic.UsageMeta {
	if tok == nil {
		return traffic.UsageMeta{Status: traffic.UsageStatusStreamingUnavailable}
	}
	pt, ptErr := countWithDeadline(ctx, tok, prompt)
	ct, ctErr := countWithDeadline(ctx, tok, completion)
	if ptErr != nil && ctErr != nil {
		return traffic.UsageMeta{Status: traffic.UsageStatusStreamingUnavailable}
	}
	var um traffic.UsageMeta
	um.Status = traffic.UsageStatusStreamingEstimated
	if ptErr == nil && prompt != "" {
		um.PromptTokens = &pt
	}
	if ctErr == nil {
		um.CompletionTokens = &ct
	}
	return um
}

// SetPromptText records the concatenated request text so Finalize can
// estimate prompt tokens when the provider omits them from the stream.
// Called by the pipeline immediately after constructing the accumulator.
func SetPromptText(acc UsageAccumulator, prompt string) {
	if prompt == "" {
		return
	}
	switch a := acc.(type) {
	case *openaiAccumulator:
		a.promptText = prompt
	case *anthropicAccumulator:
		a.promptText = prompt
	case *geminiAccumulator:
		a.promptText = prompt
	}
}
