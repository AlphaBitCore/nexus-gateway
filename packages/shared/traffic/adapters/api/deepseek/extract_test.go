package deepseek

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// ID is the stable wire identifier — load-bearing across audit / metrics /
// catalog lookups; rename = downstream column drift.
func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if got := a.ID(); got != "deepseek" {
		t.Errorf("ID()=%q want deepseek", got)
	}
}

// Configure is a no-op delegate to the inner openai adapter. Lock both the
// nil-map and populated-map paths so a future inner refactor that adds
// validation surfaces here.
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractRequest must delegate to the inner openai adapter; deepseek's
// canonical endpoint is /v1/chat/completions and the request must parse
// through the openai-chat path.
func TestExtractRequest_ChatCompletions(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-chat",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hello deepseek!"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("ExtractRequest err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[1] != "Hello deepseek!" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "deepseek-chat" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// ExtractRequest on an unknown path returns ErrUnknownSchema — the
// dispatcher relies on this sentinel to demote to a generic spec instead of
// silently emitting empty content.
func TestExtractRequest_UnknownPath(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{}`), "/v1/unknown")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractRequest on a malformed body surfaces ErrMalformed for downstream
// observability — must NOT silently return empty content.
func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractResponse on a chat/completions response extracts assistant
// content from choices[i].message.content.
func TestExtractResponse_ChatCompletions(t *testing.T) {
	body := []byte(`{
		"id":"resp_1",
		"model":"deepseek-chat",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"Hi from deepseek."},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":4}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("ExtractResponse err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hi from deepseek." {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse on an unknown path returns ErrUnknownSchema — same
// dispatcher contract as the request path.
func TestExtractResponse_UnknownPath(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{}`), "/v1/unknown")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractStreamChunk decodes a single SSE delta frame: content streamed
// chunk-by-chunk lands in Segments so the accumulator can stitch text.
func TestExtractStreamChunk_ContentDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("ExtractStreamChunk err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v want [Hello]", nc.Segments)
	}
}

// ExtractStreamChunk on a malformed chunk surfaces ErrMalformed — broken
// SSE frames must not be silently swallowed.
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// RewriteResponseBody must round-trip through the inner openai
// rewriter — assistant content in choices[i].message.content is
// replaced with the supplied Segments.
func TestRewriteResponseBody_ChatCompletions(t *testing.T) {
	body := []byte(`{
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"secret SSN 123-45-6789"}}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"secret [REDACTED]"}})
	if err != nil {
		t.Fatalf("RewriteResponseBody err=%v", err)
	}
	if n != 1 {
		t.Errorf("patches=%d want 1", n)
	}
	if !strings.Contains(string(out), "secret [REDACTED]") {
		t.Errorf("body did not contain redacted content: %s", out)
	}
	if strings.Contains(string(out), "123-45-6789") {
		t.Errorf("body still contained original PII: %s", out)
	}
}

// RewriteResponseBody on /v1/embeddings is intentionally unsupported (the
// inner openai adapter returns ErrRewriteUnsupported); deepseek inherits
// the same contract.
func TestRewriteResponseBody_EmbeddingsUnsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{}`), "/v1/embeddings",
		traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// DetectRequestMeta_NoAuth: missing Authorization header MUST leave
// ApiKeyClass / Fingerprint empty — stale "sk-" stamping on an
// unauthenticated request would poison downstream attribution.
func TestDetectRequestMeta_NoAuth(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat","messages":[]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "deepseek" {
		t.Errorf("Provider=%q want deepseek", meta.Provider)
	}
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty when no auth", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint=%q want empty when no auth", meta.ApiKeyFingerprint)
	}
	if meta.Model != "deepseek-chat" {
		t.Errorf("Model=%q want deepseek-chat", meta.Model)
	}
	if meta.Path != "/v1/chat/completions" {
		t.Errorf("Path=%q want /v1/chat/completions", meta.Path)
	}
}

// DetectRequestMeta_NilRequest: defensive — body-only call must still
// surface Provider + Model. Provider override must fire even on a nil *http.Request.
func TestDetectRequestMeta_NilRequest(t *testing.T) {
	body := []byte(`{"model":"deepseek-reasoner"}`)
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, body)
	if meta.Provider != "deepseek" {
		t.Errorf("Provider=%q want deepseek", meta.Provider)
	}
	if meta.Model != "deepseek-reasoner" {
		t.Errorf("Model=%q want deepseek-reasoner", meta.Model)
	}
}

// DetectRequestMeta_MalformedBody: a non-JSON body must NOT poison the
// Model field but the header parsing path (Provider, ApiKeyClass) stays
// independent.
func TestDetectRequestMeta_MalformedBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-deepseek-demo")
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "deepseek" {
		t.Errorf("Provider=%q want deepseek", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty for malformed body", meta.Model)
	}
	if meta.ApiKeyClass != "sk-" {
		t.Errorf("ApiKeyClass=%q want sk- (independent of body)", meta.ApiKeyClass)
	}
}

// DetectResponseUsage_NoBody: zero-length body returns the dedicated
// NoBody status so observability can tell "we never saw a body" from
// "we saw garbage".
func TestDetectResponseUsage_NoBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// DetectResponseUsage_MalformedJSON: non-empty but non-JSON returns
// ParseFailed — distinct from NoBody for cost-analytics visibility.
func TestDetectResponseUsage_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`not json`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

// DetectResponseUsage_PromptCacheHit: deepseek's chat.completions response
// pre-dates OpenAI's prompt_tokens_details rollout and uses a flat
// `prompt_cache_hit_tokens`. The inner openai adapter treats this as a
// synonym for cached_tokens — pin the flow through the deepseek wrapper.
func TestDetectResponseUsage_DeepSeekPromptCacheHit(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":80}}`)
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("Status=%q want ok", um.Status)
	}
	if um.CacheReadTokens == nil || *um.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens=%v want 80 (DeepSeek prompt_cache_hit_tokens)", um.CacheReadTokens)
	}
}
