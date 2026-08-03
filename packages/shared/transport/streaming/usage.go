package streaming

import (
	"context"
	"strings"

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
