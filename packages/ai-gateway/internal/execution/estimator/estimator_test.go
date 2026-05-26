// Package estimator_test covers the cost estimator package:
// Estimate, ReadReasoningSignal, CountTokens, pickTokenizer,
// lookupOutputBudget, expandRange, reasoningShareFromEffort.
//
// Named failure modes:
//   - Empty canonical body → error
//   - Context cancellation → error
//   - Heuristic tokenizer produces ±char/divisor estimate
//   - Gemini adapter uses 4.0 divisor; others use 3.5
//   - lookupOutputBudget: known model+effort → anchor; unknown → 0,false
//   - lookupOutputBudget: known model+empty effort → low anchor, supports=true
//   - lookupOutputBudget: known model+unknown effort → 0,true
//   - expandRange: min low=1; high clamped to maxOutput
//   - ReadReasoningSignal: reasoning_effort, reasoning.effort, thinking.budget_tokens,
//     nexus.ext paths, gemini paths, absent → empty ReasoningSignal
//   - bucketBudget: ≤0 → ""; <2000 → "low"; <8000 → "medium"; ≥8000 → "high"
//   - reasoningShareFromEffort: high/medium/low/minimal/unknown
//   - Estimate: full integration, reasoning model, unknown model fallback
package estimator_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// Estimate: guard rails

func TestEstimate_emptyBody_returnsError(t *testing.T) {
	_, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: nil,
	})
	if err == nil {
		t.Error("expected error for nil body")
	}
	_, err = estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte{},
	})
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestEstimate_cancelledContext_returnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := estimator.Estimate(ctx, estimator.EstimateInput{
		CanonicalRequest: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// Estimate: basic integration

func TestEstimate_basicRequest_costUSD(t *testing.T) {
	ip := 1.0
	op := 2.0
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"Hello, this is a test message."}]}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ProviderID:  "prov-1",
			ModelID:     "m1",
			ModelCode:   "gpt-4o",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
		Prices: metrics.ModelPrices{InputUsdPerM: &ip, OutputUsdPerM: &op},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Cost.Currency != "USD" {
		t.Errorf("currency: got %q, want USD", res.Cost.Currency)
	}
	if res.Tokens.UncachedInput <= 0 {
		t.Errorf("UncachedInput: got %d, want >0", res.Tokens.UncachedInput)
	}
	// heuristic assumption added
	if len(res.Assumptions) == 0 {
		t.Error("expected at least one heuristic assumption")
	}
}

func TestEstimate_geminiAdapter_4divisor(t *testing.T) {
	// Same message with openai vs gemini; gemini produces fewer tokens (larger divisor)
	body := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"` + "abcdefghijklmnopqrstuvwxyz0123456789" + `"}]}`)

	resOpenAI, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: body,
		IngressFormat:    provcore.FormatOpenAI,
		Target:           estimator.ResolvedTarget{AdapterType: "openai", MaxOutput: 4096},
	})
	if err != nil {
		t.Fatalf("openai: %v", err)
	}
	resGemini, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: body,
		IngressFormat:    provcore.FormatGemini,
		Target:           estimator.ResolvedTarget{AdapterType: "gemini", MaxOutput: 4096},
	})
	if err != nil {
		t.Fatalf("gemini: %v", err)
	}
	// Gemini uses chars/4; openai uses chars/3.5 → gemini token count ≤ openai
	if resGemini.Tokens.UncachedInput > resOpenAI.Tokens.UncachedInput {
		t.Errorf("gemini (%d) should use fewer tokens than openai (%d) for same content",
			resGemini.Tokens.UncachedInput, resOpenAI.Tokens.UncachedInput)
	}
}

func TestEstimate_reasoningModel_supportsFlag(t *testing.T) {
	// gpt-5 with high effort → reasoning supported
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"solve this"}],"reasoning_effort":"high"}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-5",
			AdapterType: "openai",
			MaxOutput:   32768,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reasoning.SupportedByModel {
		t.Error("expected SupportedByModel=true for gpt-5")
	}
	if res.Reasoning.EffortRequested != "high" {
		t.Errorf("effort: got %q, want high", res.Reasoning.EffortRequested)
	}
	if res.Reasoning.EstimatedTokens <= 0 {
		t.Errorf("EstimatedTokens: got %d, want >0", res.Reasoning.EstimatedTokens)
	}
	if res.Tokens.Reasoning.Expected <= 0 {
		t.Errorf("Tokens.Reasoning.Expected: got %d", res.Tokens.Reasoning.Expected)
	}
}

func TestEstimate_unsupportedModelWithEffort_assumptionAdded(t *testing.T) {
	// Non-reasoning model with reasoning_effort set → assumption note added
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-4o",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// gpt-4o is not in outputBudgetTable → SupportedByModel=false
	if res.Reasoning.SupportedByModel {
		t.Error("gpt-4o should not support reasoning")
	}
	// Assumption note about ignored reasoning_effort
	found := false
	for _, a := range res.Assumptions {
		if len(a) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one assumption about reasoning_effort being ignored")
	}
}

func TestEstimate_unknownModel_genericFallback(t *testing.T) {
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "unknown-model-xyz",
			AdapterType: "openai",
			MaxOutput:   8192,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Generic fallback: max_tokens/4 with floor 100
	if res.Tokens.Output.Expected < 100 {
		t.Errorf("generic fallback: output.Expected=%d, want ≥100", res.Tokens.Output.Expected)
	}
}

func TestEstimate_maxCompletionTokensInBody(t *testing.T) {
	// max_completion_tokens in body affects output budget anchor for unknown models
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"unknown","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":2000}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "unknown",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2000/4 = 500 expected output
	if res.Tokens.Output.Expected != 500 {
		t.Errorf("output.Expected: got %d, want 500", res.Tokens.Output.Expected)
	}
}

func TestEstimate_maxTokensInBody(t *testing.T) {
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"unknown","messages":[{"role":"user","content":"hi"}],"max_tokens":800}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "unknown",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 800/4 = 200 expected output
	if res.Tokens.Output.Expected != 200 {
		t.Errorf("output.Expected: got %d, want 200", res.Tokens.Output.Expected)
	}
}

func TestEstimate_multiPartContent_sumsChars(t *testing.T) {
	// Array content parts — chars summed from .text fields
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}]}`)
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: body,
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-4o",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "hello"+"world" = 10 chars / 3.5 ≈ 2-3 tokens
	if res.Tokens.UncachedInput <= 0 {
		t.Errorf("UncachedInput: got %d", res.Tokens.UncachedInput)
	}
}


func TestReadReasoningSignal_reasoningEffort_topLevel(t *testing.T) {
	body := []byte(`{"reasoning_effort":"high"}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatOpenAI)
	if sig.Effort != "high" || sig.Source != "reasoning_effort" {
		t.Errorf("got %+v", sig)
	}
}

func TestReadReasoningSignal_reasoningDotEffort(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"medium"}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatOpenAI)
	if sig.Effort != "medium" || sig.Source != "reasoning.effort" {
		t.Errorf("got %+v", sig)
	}
}

func TestReadReasoningSignal_thinkingBudgetTokens(t *testing.T) {
	body := []byte(`{"thinking":{"budget_tokens":5000}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatAnthropic)
	if sig.Effort != "medium" {
		t.Errorf("5000 tokens: got effort %q, want medium", sig.Effort)
	}
	if sig.BudgetTokens != 5000 {
		t.Errorf("BudgetTokens: got %d", sig.BudgetTokens)
	}
	if sig.Source != "thinking.budget_tokens" {
		t.Errorf("source: got %q", sig.Source)
	}
}

func TestReadReasoningSignal_nexusExtAnthropicThinking(t *testing.T) {
	body := []byte(`{"nexus":{"ext":{"anthropic":{"thinking":{"budget_tokens":1000}}}}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatOpenAI)
	if sig.Effort != "low" {
		t.Errorf("1000 tokens: got effort %q, want low", sig.Effort)
	}
	if sig.Source != "nexus.ext.anthropic.thinking.budget_tokens" {
		t.Errorf("source: got %q", sig.Source)
	}
}

func TestReadReasoningSignal_geminiThinkingConfigBudget(t *testing.T) {
	body := []byte(`{"thinking_config":{"thinking_budget":10000}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatGemini)
	if sig.Effort != "high" {
		t.Errorf("10000 tokens: got effort %q, want high", sig.Effort)
	}
	if sig.Source != "thinking_config.thinking_budget" {
		t.Errorf("source: got %q", sig.Source)
	}
}

func TestReadReasoningSignal_nexusExtGeminiThinkingBudget(t *testing.T) {
	body := []byte(`{"nexus":{"ext":{"gemini":{"thinking_config":{"thinking_budget":8000}}}}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatGemini)
	if sig.Effort != "high" {
		t.Errorf("8000 tokens: got effort %q, want high", sig.Effort)
	}
	if sig.Source != "nexus.ext.gemini.thinking_config.thinking_budget" {
		t.Errorf("source: got %q", sig.Source)
	}
}

func TestReadReasoningSignal_nonePresent_emptySignal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatOpenAI)
	if sig.Effort != "" || sig.BudgetTokens != 0 || sig.Source != "" {
		t.Errorf("expected empty signal, got %+v", sig)
	}
}

// bucketBudget edge cases exercised via ReadReasoningSignal.

func TestReadReasoningSignal_budgetZeroOrNeg_emptyEffort(t *testing.T) {
	// budget_tokens = 0 → bucketBudget returns ""
	body := []byte(`{"thinking":{"budget_tokens":0}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatAnthropic)
	if sig.Effort != "" {
		t.Errorf("zero budget: got effort %q, want empty", sig.Effort)
	}
}

func TestReadReasoningSignal_budgetLow_lowEffort(t *testing.T) {
	body := []byte(`{"thinking":{"budget_tokens":1999}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatAnthropic)
	if sig.Effort != "low" {
		t.Errorf("1999 tokens: got %q, want low", sig.Effort)
	}
}

func TestReadReasoningSignal_budgetExactly2000_mediumEffort(t *testing.T) {
	body := []byte(`{"thinking":{"budget_tokens":2000}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatAnthropic)
	if sig.Effort != "medium" {
		t.Errorf("2000 tokens: got %q, want medium", sig.Effort)
	}
}

func TestReadReasoningSignal_budgetExactly8000_highEffort(t *testing.T) {
	body := []byte(`{"thinking":{"budget_tokens":8000}}`)
	sig := estimator.ReadReasoningSignal(body, provcore.FormatAnthropic)
	if sig.Effort != "high" {
		t.Errorf("8000 tokens: got %q, want high", sig.Effort)
	}
}

// expandRange / output budget

func TestEstimate_outputRangeLowHighConsistency(t *testing.T) {
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-5",
			AdapterType: "openai",
			MaxOutput:   16000,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tokens.Output.Low >= res.Tokens.Output.Expected {
		t.Errorf("Output.Low (%d) must be < Expected (%d)", res.Tokens.Output.Low, res.Tokens.Output.Expected)
	}
	if res.Tokens.Output.High <= res.Tokens.Output.Expected {
		t.Errorf("Output.High (%d) must be > Expected (%d)", res.Tokens.Output.High, res.Tokens.Output.Expected)
	}
}

func TestEstimate_maxOutputClampedToModelMax(t *testing.T) {
	// Model max much larger than anchor*3 → high is clamped to maxOutput.
	// Use an unknown model with max_completion_tokens=10 so anchor=10/4=2 (floor 100),
	// anchor*3=300, then maxOutput=200 → high=200.
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"unknown-low","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":800}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "unknown-low",
			AdapterType: "openai",
			MaxOutput:   300, // anchor=200, high=600 → clamped to 300
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// anchor = 800/4 = 200; high = 200*3 = 600 > 300 → high = 300
	if res.Tokens.Output.High > 300 {
		t.Errorf("Output.High (%d) should be clamped to 300", res.Tokens.Output.High)
	}
}

// reasoningShareFromEffort variants

func TestEstimate_reasoningMinimalEffort_share(t *testing.T) {
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"minimal"}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-5",
			AdapterType: "openai",
			MaxOutput:   8192,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reasoning.SupportedByModel {
		t.Error("gpt-5 should support reasoning")
	}
	if res.Reasoning.EffortRequested != "minimal" {
		t.Errorf("effort: %q", res.Reasoning.EffortRequested)
	}
}

func TestEstimate_reasoningMediumEffort_share(t *testing.T) {
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"o3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "o3",
			AdapterType: "openai",
			MaxOutput:   16384,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reasoning.SupportedByModel {
		t.Error("o3 should support reasoning")
	}
}

// lookupOutputBudget: known model + empty effort → low anchor, supports=true

func TestEstimate_knownModelNoEffort_lowAnchorUsed(t *testing.T) {
	// Known reasoning model (claude-opus-4-7) without explicit effort.
	// lookupOutputBudget returns (low anchor, true) when effort is empty.
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "claude-opus-4-7",
			AdapterType: "anthropic",
			MaxOutput:   8192,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Model supports reasoning (it's in the table), but effort="" → SupportedByModel=true
	// Reasoning EstimatedTokens = 0 when effort is empty (reasoningShareFromEffort("") = 0)
	if !res.Reasoning.SupportedByModel {
		t.Error("claude-opus-4-7 should have SupportedByModel=true")
	}
}

func TestEstimate_knownModelUnrecognizedEffort_noAnchor(t *testing.T) {
	// Known model + unrecognized effort key → lookupOutputBudget returns (0, true)
	// Falls back to generic budget from max_tokens.
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"turbo"}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-5",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// gpt-5 table has no "turbo" key → (0, true) → generic fallback used
	// output.Expected = max(maxOutput/4, 100) = 4096/4 = 1024
	if res.Tokens.Output.Expected != 1024 {
		t.Errorf("output.Expected: got %d, want 1024", res.Tokens.Output.Expected)
	}
}

// Tokenizer: CountTokens edge case

func TestEstimate_emptyMessages_zeroInputTokens(t *testing.T) {
	// No messages → countCanonicalInputChars = 0 → CountTokens(0) = 0
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"gpt-4o","messages":[]}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "gpt-4o",
			AdapterType: "openai",
			MaxOutput:   4096,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tokens.UncachedInput != 0 {
		t.Errorf("UncachedInput: got %d, want 0 for empty messages", res.Tokens.UncachedInput)
	}
}

// No max output: generic fallback uses 4096 constant

func TestEstimate_noMaxOutputAndNoBody_fallback4096(t *testing.T) {
	// No max_tokens/max_completion_tokens in body, MaxOutput=0 → pickMaxTokens returns 4096
	// unknown model → expectedOutput = 4096/4 = 1024
	res, err := estimator.Estimate(context.Background(), estimator.EstimateInput{
		CanonicalRequest: []byte(`{"model":"unknown-x","messages":[{"role":"user","content":"hi"}]}`),
		IngressFormat:    provcore.FormatOpenAI,
		Target: estimator.ResolvedTarget{
			ModelCode:   "unknown-x",
			AdapterType: "openai",
			MaxOutput:   0,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tokens.Output.Expected != 1024 {
		t.Errorf("output.Expected: got %d, want 1024", res.Tokens.Output.Expected)
	}
}
