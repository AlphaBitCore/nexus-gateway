package anthropic

import (
	"context"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMetaXApiKey(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[]}`)
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://api.anthropic.com/v1/messages", nil)
	r.Header.Set("x-api-key", "sk-ant-api03-demo")

	got := a.DetectRequestMeta(r, body)
	if got.Provider != "anthropic" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "sk-ant-" {
		t.Errorf("class = %q, want sk-ant-", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("sk-ant-api03-demo") {
		t.Errorf("fingerprint mismatch")
	}
}

func TestDetectRequestMetaBearerFallback(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://api.anthropic.com/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer sk-ant-oauth-demo")
	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyClass != "sk-ant-" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectResponseUsageOK(t *testing.T) {
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

func TestDetectResponseUsageNoUsage(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"content":[]}`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("status = %q, want parse_failed", um.Status)
	}
}

// TestDetectResponseUsage_PromptCacheTokens pins that prompt-cache hits
// surface on the typed UsageMeta envelope (mirrors what spec_anthropic
// codec writes to providers.Usage). Cost analytics reads CacheReadTokens
// without re-parsing the canonical body.
func TestDetectResponseUsage_PromptCacheTokens(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":384,"cache_creation_input_tokens":1024}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if um.CacheReadTokens == nil || *um.CacheReadTokens != 384 {
		t.Errorf("CacheReadTokens=%v want 384", um.CacheReadTokens)
	}
	// cache_creation_input_tokens stays on nexus.ext (spec codec only),
	// not on the typed envelope — confirm it was NOT mistakenly aliased.
	if um.ReasoningTokens != nil {
		t.Errorf("ReasoningTokens unexpectedly set for Anthropic: %v", um.ReasoningTokens)
	}
}
