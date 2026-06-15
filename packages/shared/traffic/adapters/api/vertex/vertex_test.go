package vertex

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "vertex" {
		t.Errorf("ID = %q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map) = %v", err)
	}
}

// ExtractRequest — unknown publisher branch + each publisher's success path

func TestExtractRequest_UnknownPublisher(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"x":"y"}`),
		"/v1/projects/p/locations/r/publishers/mistralai/models/m:predict")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_GeminiOnVertex(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello vertex"}]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body,
		"/v1/projects/p/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello vertex" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse — unknown publisher branch + anthropic success path

func TestExtractResponse_UnknownPublisher(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"x":"y"}`),
		"/v1/projects/p/locations/r/publishers/mistralai/models/m:predict")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_AnthropicOnVertex(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Paris"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body,
		"/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-3-5-sonnet@20240620:rawPredict")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) == 0 || !strings.Contains(strings.Join(nc.Segments, " "), "Paris") {
		t.Errorf("Segments=%v want contains Paris", nc.Segments)
	}
}

// ExtractStreamChunk — three branches (anthropic, gemini, unknown)

func TestExtractStreamChunk_AnthropicOnVertex(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk,
		"/v1/projects/p/locations/r/publishers/anthropic/models/claude-3-5-sonnet:streamRawPredict")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_GeminiOnVertex(t *testing.T) {
	chunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk,
		"/v1/projects/p/locations/r/publishers/google/models/gemini-1.5-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_UnknownPublisher(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`{"x":"y"}`),
		"/v1/projects/p/locations/r/publishers/mistralai/models/m:streamPredict")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// RewriteResponseBody — three branches (anthropic, gemini, unknown)

func TestRewriteResponseBody_AnthropicOnVertex(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"email a@b.com"}]}`)
	a := &Adapter{}
	_, n, err := a.RewriteResponseBody(context.Background(), body,
		"/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-3-5-sonnet:rawPredict",
		traffic.NormalizedContent{Segments: []string{"email [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n < 1 {
		t.Errorf("n=%d want >=1", n)
	}
}

func TestRewriteResponseBody_GeminiOnVertex(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ssn 123"}]}}]}`)
	a := &Adapter{}
	_, n, err := a.RewriteResponseBody(context.Background(), body,
		"/v1/projects/p/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"ssn [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n < 1 {
		t.Errorf("n=%d want >=1", n)
	}
}

func TestRewriteResponseBody_UnknownPublisherUnsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"x":"y"}`),
		"/v1/projects/p/locations/us/publishers/mistralai/models/m:predict",
		traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// DetectResponseUsage — nil response branch + nil request URL parse_failed

func TestDetectResponseUsage_UnknownPublisherEmptyBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/r/publishers/mistral/models/mistral-large:rawPredict")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("unknown publisher + empty body → no_body, got %q", um.Status)
	}
}

func TestDetectResponseUsage_NilResponse(t *testing.T) {
	a := &Adapter{}
	if got := a.DetectResponseUsage(nil, nil); got.Status != traffic.UsageStatusNoBody {
		t.Errorf("nil resp + nil body status=%q want no_body", got.Status)
	}
	if got := a.DetectResponseUsage(nil, []byte(`{"x":1}`)); got.Status != traffic.UsageStatusParseFailed {
		t.Errorf("nil resp + body status=%q want parse_failed", got.Status)
	}
}

// DetectRequestMeta — non-bearer auth path leaves ApiKeyClass empty,
// nil request returns provider-only meta.

func TestDetectRequestMeta_NilRequest(t *testing.T) {
	a := &Adapter{}
	got := a.DetectRequestMeta(nil, nil)
	if got.Provider != "vertex" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.ApiKeyClass != "" || got.ApiKeyFingerprint != "" {
		t.Errorf("no auth → empty class/fp, got %q/%q", got.ApiKeyClass, got.ApiKeyFingerprint)
	}
}

func TestDetectRequestMeta_NoBearerHeader(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/r/publishers/google/models/gemini-1.5-pro:generateContent",
		nil)
	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyClass != "" {
		t.Errorf("no Authorization header → empty class, got %q", got.ApiKeyClass)
	}
	if got.Model != "google/gemini-1.5-pro" {
		t.Errorf("model = %q", got.Model)
	}
}

// publisherAndModel non-matching path branch.

func TestPublisherAndModel_NoMatch(t *testing.T) {
	pub, mdl := publisherAndModel("/no/match/here")
	if pub != "" || mdl != "" {
		t.Errorf("got (%q,%q) want empty", pub, mdl)
	}
}

// Normalize — Tier-1 dispatch via the unified extract helper.

func TestDetectRequestMetaAnthropicOnVertex(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://us-central1-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-central1/publishers/anthropic/models/claude-3-5-sonnet@20240620:rawPredict",
		nil)
	r.Header.Set("Authorization", "Bearer ya29.a0AfH6SMB-example-oauth-token")

	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "vertex" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "anthropic/claude-3-5-sonnet@20240620" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "gcp-oauth" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectRequestMetaGeminiOnVertex(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://us-central1-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
		nil)
	r.Header.Set("Authorization", "Bearer ya29.another-oauth")

	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "vertex" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "google/gemini-1.5-pro" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "gcp-oauth" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
}

func TestDetectRequestMetaUnknownPublisher(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://us-central1-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-central1/publishers/mistral/models/mistral-large:rawPredict",
		nil)

	got := a.DetectRequestMeta(r, nil)
	// Publisher is namespaced even when the sub-adapter isn't wired — detection
	// is best-effort, content extraction returns ErrUnknownSchema separately.
	if got.Provider != "vertex" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "mistral/mistral-large" {
		t.Errorf("model = %q", got.Model)
	}
}

func TestPublisherFromPath(t *testing.T) {
	cases := map[string]string{
		"/v1/projects/p/locations/r/publishers/anthropic/models/claude-3-5-sonnet:rawPredict": "anthropic",
		"/v1/projects/p/locations/r/publishers/GOOGLE/models/gemini-1.5-pro:generateContent":  "google",
		"/v1/projects/p/locations/r/publishers/mistral/models/mistral-large":                  "mistral",
		"/v1/projects/p/locations/r/something-else":                                           "",
		"/v1beta/models/gemini-1.5-pro:generateContent":                                       "",
	}
	for in, want := range cases {
		if got := publisherFromPath(in); got != want {
			t.Errorf("publisherFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectResponseUsageAnthropicOnVertex(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/r/publishers/anthropic/models/claude-3-5-sonnet@20240620:rawPredict")
	resp := &http.Response{Request: &http.Request{URL: u}}
	body := []byte(`{"usage":{"input_tokens":42,"output_tokens":17}}`)

	um := a.DetectResponseUsage(resp, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 42 || *um.CompletionTokens != 17 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

func TestDetectResponseUsageGeminiOnVertex(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/r/publishers/google/models/gemini-1.5-pro:generateContent")
	resp := &http.Response{Request: &http.Request{URL: u}}
	body := []byte(`{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50}}`)

	um := a.DetectResponseUsage(resp, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 100 || *um.CompletionTokens != 50 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

func TestDetectResponseUsageUnknownPublisher(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/r/publishers/mistral/models/mistral-large:rawPredict")
	resp := &http.Response{Request: &http.Request{URL: u}}
	body := []byte(`{"some":"payload"}`)

	um := a.DetectResponseUsage(resp, body)
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("unknown publisher should yield parse_failed, got %q", um.Status)
	}
}

func TestDetectResponseUsageNoBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/r/publishers/anthropic/models/claude-3-5-sonnet:rawPredict")
	resp := &http.Response{Request: &http.Request{URL: u}}

	um := a.DetectResponseUsage(resp, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("empty body should yield no_body, got %q", um.Status)
	}
}

// Delegation regression tests — pin that the P0-2 anthropic and P0-3
// gemini adapter audits flow through to Vertex callers.

func TestExtractRequest_AnthropicOnVertex_ToolUseDelegation(t *testing.T) {
	body := []byte(`{
		"anthropic_version":"vertex-2023-10-16",
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"NYC"}}
			]}
		],
		"max_tokens":1024
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/projects/p/locations/us-central1/publishers/anthropic/models/claude-3-5-sonnet:rawPredict")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v want tool_use through anthropic delegation", nc.ToolCallSegments)
	}
}

func TestExtractResponse_GeminiOnVertex_FunctionCallDelegation(t *testing.T) {
	body := []byte(`{
		"candidates":[{
			"content":{"role":"model","parts":[
				{"functionCall":{"name":"send_email","args":{"to":"x@y.com"}}}
			]},
			"finishReason":"STOP"
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-1.5-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v want functionCall through gemini delegation", nc.ToolCallSegments)
	}
	if nc.Metadata["finishReason"] != "STOP" {
		t.Errorf("finishReason=%q", nc.Metadata["finishReason"])
	}
}
