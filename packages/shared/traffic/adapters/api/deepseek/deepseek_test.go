package deepseek

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"model":"deepseek-chat","messages":[]}`)
	r, _ := http.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-deepseek-demo")

	got := a.DetectRequestMeta(r, body)
	if got.Provider != "deepseek" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "deepseek-chat" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "sk-" {
		t.Errorf("class = %q, want sk-", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectResponseUsage(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":5,"completion_tokens":9}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 5 || *um.CompletionTokens != 9 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}
