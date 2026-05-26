package minimax

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// MiniMax retired the legacy chatcompletion_pro path and the abab*
// model family in favour of the OpenAI-compatible /v1/chat/completions
// endpoint at api.minimax.io/v1 with the M2 model family. Test pinned
// to the current surface; if a customer brings a legacy deployment we
// add a separate test case rather than re-introducing the old default.
func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"model":"MiniMax-M2.7","messages":[]}`)
	r, _ := http.NewRequest(http.MethodPost, "https://api.minimax.io/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer opaque-minimax-jwt-token")

	got := a.DetectRequestMeta(r, body)
	if got.Provider != "minimax" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "MiniMax-M2.7" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "" {
		t.Errorf("class = %q, want empty (opaque JWT)", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("opaque-minimax-jwt-token") {
		t.Errorf("fingerprint mismatch")
	}
}

func TestDetectResponseUsageOpenAICompat(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 7 || *um.CompletionTokens != 3 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

func TestDetectResponseUsageNativeTotalOnly(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"total_tokens":25}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 25 {
		t.Errorf("completion (from total) = %v", um.CompletionTokens)
	}
}
