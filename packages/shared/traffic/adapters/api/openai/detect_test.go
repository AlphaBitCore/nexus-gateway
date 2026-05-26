package openai

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	r, _ := http.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-proj-example")

	got := a.DetectRequestMeta(r, body)
	if got.Provider != "openai" {
		t.Errorf("provider = %q, want openai", got.Provider)
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", got.Model)
	}
	if got.Path != "/v1/chat/completions" {
		t.Errorf("path = %q", got.Path)
	}
	if got.ApiKeyClass != "sk-proj-" {
		t.Errorf("apiKeyClass = %q, want sk-proj-", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("sk-proj-example") {
		t.Errorf("apiKeyFingerprint mismatch: %q", got.ApiKeyFingerprint)
	}
}

func TestDetectRequestMetaEmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodGet, "http://api.openai.com/v1/models", nil)
	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "openai" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "" {
		t.Errorf("model should be empty, got %q", got.Model)
	}
}

func TestDetectResponseUsageOK(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46},"choices":[]}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("status = %q, want ok", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 12 {
		t.Errorf("prompt tokens = %v", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 34 {
		t.Errorf("completion tokens = %v", um.CompletionTokens)
	}
}

func TestDetectResponseUsageResponsesAPI(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 100 || *um.CompletionTokens != 50 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

func TestDetectResponseUsageNoBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("status = %q, want no_body", um.Status)
	}
}

func TestDetectResponseUsageParseFailed(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"choices":[]}`)) // no usage block
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("status = %q, want parse_failed", um.Status)
	}
}

// DetectResponseUsage on a malformed body must surface ParseFailed.
func TestDetectResponseUsage_Malformed(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{bad`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

// TestDetectResponseUsage_OpenAICacheAndReasoningTokens pins that the
// canonical OpenAI splits (prompt_tokens_details.cached_tokens for
// prompt-cache, completion_tokens_details.reasoning_tokens for o1/o3)
// land on UsageMeta. Same wiring as spec_openai.IdentityCodec.
func TestDetectResponseUsage_OpenAICacheAndReasoningTokens(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":15}}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.CacheReadTokens == nil || *um.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens=%v want 40", um.CacheReadTokens)
	}
	if um.ReasoningTokens == nil || *um.ReasoningTokens != 15 {
		t.Errorf("ReasoningTokens=%v want 15", um.ReasoningTokens)
	}
}

// TestDetectResponseUsage_DeepSeekCacheHitFallback confirms DeepSeek's
// flat prompt_cache_hit_tokens is treated as a CacheReadTokens synonym
// when the OpenAI shape is missing.
func TestDetectResponseUsage_DeepSeekCacheHitFallback(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.CacheReadTokens == nil || *um.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens fallback=%v want 80", um.CacheReadTokens)
	}
}
