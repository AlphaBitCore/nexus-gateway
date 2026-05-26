package gemini

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMetaPathModel(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"http://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent", nil)
	r.Header.Set("x-goog-api-key", "AIzaSyBsecretExample")

	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "gemini" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "gemini-1.5-pro" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "AIza" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectRequestMetaQueryKey(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"http://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=AIzaQueryExample", nil)
	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("AIzaQueryExample") {
		t.Errorf("fingerprint mismatch")
	}
}

func TestDetectResponseUsageOK(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":88,"totalTokenCount":130}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 42 || *um.CompletionTokens != 88 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

// TestDetectResponseUsage_ContextCacheAndThoughts pins that Gemini's
// context-cache + thinking-tokens splits land on UsageMeta. Same wiring
// as spec_gemini.codec writes onto providers.Usage.
func TestDetectResponseUsage_ContextCacheAndThoughts(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usageMetadata":{"promptTokenCount":1024,"candidatesTokenCount":6,"totalTokenCount":1030,"cachedContentTokenCount":768,"thoughtsTokenCount":4}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.CacheReadTokens == nil || *um.CacheReadTokens != 768 {
		t.Errorf("CacheReadTokens=%v want 768", um.CacheReadTokens)
	}
	if um.ReasoningTokens == nil || *um.ReasoningTokens != 4 {
		t.Errorf("ReasoningTokens=%v want 4", um.ReasoningTokens)
	}
}

// DetectRequestMeta body-model fallback: when the URL path doesn't
// embed the model (e.g. agent-side intercepts where the original URL was
// rewritten), the extractor must look at the body's `model` field.
func TestDetectRequestMeta_ModelFromBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"http://example.com/proxy/passthrough", nil)
	body := []byte(`{"model":"gemini-2.0-pro","contents":[]}`)
	got := a.DetectRequestMeta(r, body)
	if got.Model != "gemini-2.0-pro" {
		t.Errorf("Model=%q want gemini-2.0-pro (body fallback)", got.Model)
	}
}

// DetectRequestMeta Bearer-token fallback: when neither x-goog-api-key
// header nor ?key= query param is present, the extractor must try the
// standard Authorization: Bearer header (some intermediaries rewrite to
// it for compatibility).
func TestDetectRequestMeta_BearerFallback(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"http://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent", nil)
	r.Header.Set("Authorization", "Bearer AIzaBearerFallback")
	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("AIzaBearerFallback") {
		t.Errorf("ApiKeyFingerprint mismatch via Bearer fallback")
	}
}

// DetectResponseUsage on empty body → NoBody (distinct from missing
// usageMetadata which is ParseFailed).
func TestDetectResponseUsage_EmptyBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// DetectResponseUsage on malformed body → ParseFailed.
func TestDetectResponseUsage_Malformed(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{bad`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

// DetectResponseUsage when usageMetadata block is absent → ParseFailed
// (the body is JSON-valid but doesn't have the expected token block).
func TestDetectResponseUsage_MissingUsageMetadata(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"candidates":[]}`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

func TestModelFromGeminiPath(t *testing.T) {
	cases := map[string]string{
		"/v1beta/models/gemini-1.5-pro:generateContent":  "gemini-1.5-pro",
		"/v1beta/models/gemini-2.0-flash:streamGenerate": "gemini-2.0-flash",
		"/v1beta/models/gemini-1.5-flash/something":      "gemini-1.5-flash",
		"/v1beta/models/gemini-1.5-pro":                  "gemini-1.5-pro",
		"/v1beta/openai/chat/completions":                "",
	}
	for in, want := range cases {
		if got := modelFromGeminiPath(in); got != want {
			t.Errorf("modelFromGeminiPath(%q) = %q, want %q", in, got, want)
		}
	}
}
