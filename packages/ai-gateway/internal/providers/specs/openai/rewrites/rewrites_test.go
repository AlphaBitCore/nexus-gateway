// Package rewrites_test covers OpenAI reasoning-model passthrough rewrites.
// Named failure modes per provider-adapter-architecture.md §3a Rule 3:
//   - gpt-5* family: max_tokens→max_completion_tokens, temperature→removed, top_p→removed
//   - o-series (o1, o3, o4-mini ...): same rewrite set
//   - classic models (gpt-4o, gpt-3.5-turbo): no rewrite
//   - Empty payload: no rewrite
//   - max_completion_tokens already set: max_tokens deleted without overwrite
package rewrites_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/rewrites"
)

func TestIsReasoningModel_gpt5Variants(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.5"} {
		if !rewrites.IsReasoningModel(model) {
			t.Errorf("IsReasoningModel(%q) = false, want true", model)
		}
	}
}

func TestIsReasoningModel_oSeriesVariants(t *testing.T) {
	for _, model := range []string{"o1", "o1-mini", "o3", "o3-mini", "o4-mini"} {
		if !rewrites.IsReasoningModel(model) {
			t.Errorf("IsReasoningModel(%q) = false, want true", model)
		}
	}
}

func TestIsReasoningModel_classicModels_returnFalse(t *testing.T) {
	for _, model := range []string{"gpt-4o", "gpt-3.5-turbo", "gpt-4o-mini", "claude-sonnet-4-6", "", "openai-gpt4"} {
		if rewrites.IsReasoningModel(model) {
			t.Errorf("IsReasoningModel(%q) = true, want false", model)
		}
	}
}

func TestIsReasoningModel_emptyString_false(t *testing.T) {
	if rewrites.IsReasoningModel("") {
		t.Error("IsReasoningModel('') should return false")
	}
}

func TestApplyReasoningRewrites_nonReasoningModel_noOp(t *testing.T) {
	payload := map[string]any{
		"model":       "gpt-4o",
		"max_tokens":  1024,
		"temperature": 0.7,
		"top_p":       0.9,
	}
	rewrites := rewrites.ApplyReasoningRewrites(payload, "gpt-4o")
	if len(rewrites) != 0 {
		t.Errorf("non-reasoning model: expected no rewrites, got %v", rewrites)
	}
	// Original fields untouched.
	if payload["max_tokens"] != 1024 {
		t.Error("max_tokens must not be touched for non-reasoning model")
	}
}

func TestApplyReasoningRewrites_gpt5_maxTokensRenamed(t *testing.T) {
	payload := map[string]any{"max_tokens": 512}
	got := rewrites.ApplyReasoningRewrites(payload, "gpt-5.4")
	if _, hasOld := payload["max_tokens"]; hasOld {
		t.Error("max_tokens should have been deleted")
	}
	if payload["max_completion_tokens"] != 512 {
		t.Errorf("max_completion_tokens should be 512, got %v", payload["max_completion_tokens"])
	}
	if len(got) == 0 {
		t.Error("expected at least one rewrite")
	}
	// Check rewrite string format.
	found := false
	for _, r := range got {
		if r == "max_tokens→max_completion_tokens" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected rewrite 'max_tokens→max_completion_tokens' in %v", got)
	}
}

func TestApplyReasoningRewrites_gpt5_temperatureRemoved(t *testing.T) {
	payload := map[string]any{"temperature": 0.7}
	got := rewrites.ApplyReasoningRewrites(payload, "gpt-5.4")
	if _, hasTemp := payload["temperature"]; hasTemp {
		t.Error("temperature should be removed for gpt-5")
	}
	found := false
	for _, r := range got {
		if r == "temperature→removed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'temperature→removed' rewrite, got %v", got)
	}
}

func TestApplyReasoningRewrites_gpt5_topPRemoved(t *testing.T) {
	payload := map[string]any{"top_p": 0.95}
	got := rewrites.ApplyReasoningRewrites(payload, "gpt-5")
	if _, hasTopP := payload["top_p"]; hasTopP {
		t.Error("top_p should be removed for gpt-5")
	}
	found := false
	for _, r := range got {
		if r == "top_p→removed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'top_p→removed' rewrite, got %v", got)
	}
}

func TestApplyReasoningRewrites_oSeries_allThreeParams(t *testing.T) {
	payload := map[string]any{
		"max_tokens":  2000,
		"temperature": 0.5,
		"top_p":       0.8,
	}
	got := rewrites.ApplyReasoningRewrites(payload, "o3")
	if _, ok := payload["max_tokens"]; ok {
		t.Error("max_tokens should be removed for o-series")
	}
	if payload["max_completion_tokens"] != 2000 {
		t.Errorf("max_completion_tokens should be 2000, got %v", payload["max_completion_tokens"])
	}
	if _, ok := payload["temperature"]; ok {
		t.Error("temperature should be removed for o-series")
	}
	if _, ok := payload["top_p"]; ok {
		t.Error("top_p should be removed for o-series")
	}
	if len(got) != 3 {
		t.Errorf("expected 3 rewrites, got %d: %v", len(got), got)
	}
}

func TestApplyReasoningRewrites_maxCompletionTokensAlreadySet_maxTokensDeletedWithoutOverwrite(t *testing.T) {
	// Rule: when max_completion_tokens already present, max_tokens deleted but NOT overwritten.
	payload := map[string]any{
		"max_tokens":            512,
		"max_completion_tokens": 1024,
	}
	rewrites.ApplyReasoningRewrites(payload, "gpt-5")
	if _, ok := payload["max_tokens"]; ok {
		t.Error("max_tokens should be deleted")
	}
	if payload["max_completion_tokens"] != 1024 {
		t.Errorf("max_completion_tokens must not be overwritten: got %v", payload["max_completion_tokens"])
	}
}

func TestApplyReasoningRewrites_emptyPayload_reasoningModel_returnsNil(t *testing.T) {
	payload := map[string]any{}
	got := rewrites.ApplyReasoningRewrites(payload, "gpt-5")
	// No params to strip → nil rewrites (not empty slice).
	if got != nil {
		t.Errorf("empty payload: expected nil rewrites, got %v", got)
	}
}

func TestApplyReasoningRewrites_nonReasoningModel_returnsNil(t *testing.T) {
	payload := map[string]any{"temperature": 0.7}
	got := rewrites.ApplyReasoningRewrites(payload, "gpt-4o")
	if got != nil {
		t.Errorf("non-reasoning model must return nil, got %v", got)
	}
}
