package githubcopilot

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "github-copilot" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
}

// Path normalisation

func TestNormalisePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/chat/completions", "/v1/chat/completions"},
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/embeddings", "/v1/embeddings"},
		{"/v1/embeddings", "/v1/embeddings"},
		{"/v1/engines/copilot-codex/completions", "/v1/engines/copilot-codex/completions"},
		{"/unknown", "/unknown"},
	}
	for _, c := range cases {
		got := normalisePath(c.in)
		if got != c.want {
			t.Errorf("normalisePath(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// Delegation tests — Copilot Chat is OpenAI-compatible, so audit code
// paths added in the openai-compat audit (commit 6e7a61de) reach
// Copilot users automatically.

func TestExtractRequest_CopilotChat(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o-copilot",
		"messages":[
			{"role":"user","content":"refactor this function"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "refactor this function" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "gpt-4o-copilot" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_CopilotToolCallsDelegation(t *testing.T) {
	// Copilot Chat surfaces tool_calls in conversation history; the
	// delegation chain must carry them to ToolCallSegments.
	body := []byte(`{
		"messages":[
			{"role":"user","content":"send email to alice"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"send_email","arguments":"{\"to\":\"alice@x.com\"}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractResponse_CopilotChat(t *testing.T) {
	body := []byte(`{
		"choices":[{"message":{"role":"assistant","content":"Here's the refactor:"}, "finish_reason":"stop"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Here's the refactor:" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

func TestExtractStreamChunk_CopilotChat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// Legacy completions endpoint isn't supported by openai-compat; it
// should return ErrUnknownSchema cleanly without panicking.
func TestExtractRequest_LegacyCompletionsEndpointUnknownSchema(t *testing.T) {
	body := []byte(`{"prompt":"hint","engine":"copilot-codex"}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/v1/engines/copilot-codex/completions")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// API key classification

func TestClassifyGitHubKey(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer tid_eyJhbGciOiJIUzI1NiJ9...", "github-copilot-tid"},
		{"Bearer gho_abc123def456ghi789jkl0mn", "github-oauth"},
		{"Bearer ghs_serverappsecret123456789", "github-app"},
		{"Bearer ghu_userToServer123456789012345", "github-user-to-server"},
		{"Bearer ghp_personalAccessToken12345678", "github-personal-access"},
		{"Bearer github_pat_11ABC_xyz", "github-fine-grained-pat"},
		{"Bearer unknown_prefix_token", ""},
		{"Basic Zm9vOmJhcg==", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := classifyGitHubKey(c.header); got != c.want {
			t.Errorf("classifyGitHubKey(%q)=%q want %q", c.header, got, c.want)
		}
	}
}

func TestDetectRequestMeta_ProviderAndKeyClass(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.githubcopilot.com/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer tid_eyJhbGciOiJIUzI1NiJ9.fake.fake")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "github-copilot" {
		t.Errorf("Provider=%q want github-copilot", meta.Provider)
	}
	if meta.ApiKeyClass != "github-copilot-tid" {
		t.Errorf("ApiKeyClass=%q want github-copilot-tid", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Errorf("ApiKeyFingerprint should be set when token is present")
	}
	if meta.Model != "gpt-4o" {
		t.Errorf("Model=%q want gpt-4o (delegation)", meta.Model)
	}
}

func TestDetectRequestMeta_GhoToken(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.githubcopilot.com/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer gho_abcdef0123456789abcdef")
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.ApiKeyClass != "github-oauth" {
		t.Errorf("ApiKeyClass=%q want github-oauth", meta.ApiKeyClass)
	}
}

func TestDetectRequestMeta_NoAuth(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.githubcopilot.com/chat/completions", nil)
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.Provider != "github-copilot" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty when no auth header", meta.ApiKeyClass)
	}
}

// Rewrite delegation — Copilot Chat uses OpenAI-compatible bodies so the
// rewrite walk must reach openai-compat through the wrapper. These tests
// pin the wrapper's `path → normalisePath` plumbing for both directions.

func TestRewriteRequestBody_CopilotChat_StringContent(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-copilot","messages":[{"role":"user","content":"secret token"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/chat/completions",
		traffic.NormalizedContent{Segments: []string{"[REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "[REDACTED]" {
		t.Errorf("content=%q want [REDACTED]", got)
	}
}

// Path normalisation must apply in rewrite too: `/chat/completions`
// without the v1 prefix routes the inner openai-compat rewriter.
func TestRewriteRequestBody_CopilotChat_PathNormalised(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"y"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 || gjson.GetBytes(out, "messages.0.content").String() != "y" {
		t.Errorf("rewrite v1 path failed: n=%d body=%s", n, out)
	}
}

// Legacy /completions has no rewrite support in openai-compat — the
// wrapper must surface the underlying ErrRewriteUnsupported cleanly.
func TestRewriteRequestBody_LegacyCompletions_Unsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{}`),
		"/v1/engines/copilot-codex/completions",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

func TestRewriteResponseBody_CopilotChat(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hello secret"}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/chat/completions",
		traffic.NormalizedContent{Segments: []string{"hello [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello [REDACTED]" {
		t.Errorf("content=%q", got)
	}
}

// Legacy completions response rewrite must surface unsupported (the inner
// openai-compat falls through default-case to ErrRewriteUnsupported).
func TestRewriteResponseBody_LegacyCompletions_Unsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{}`),
		"/v1/engines/copilot-codex/completions",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// DetectResponseUsage delegation — Copilot responses include the standard
// OpenAI usage block.

func TestDetectResponseUsage_DelegatesToOpenAI(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("Status=%q want ok", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 10 {
		t.Errorf("PromptTokens=%v want 10", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 20 {
		t.Errorf("CompletionTokens=%v want 20", um.CompletionTokens)
	}
}

func TestDetectResponseUsage_EmptyBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// Normalize — Tier-1 codec delegation. Copilot Chat advertises the
// openai-chat wire format so an openai-shape body must claim Tier-1 with
// DetectedSpec=github-copilot.

func TestNormalize_OpenAIChatShape(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o-copilot",
		"messages":[{"role":"user","content":"refactor this"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != adapterID {
		t.Errorf("DetectedSpec=%q want %q", payload.DetectedSpec, adapterID)
	}
}

// A non-chat-shape body must fall through with an error so the Coordinator
// advances to Tier 2 — the safe degradation path for Copilot IDE binary
// protocols that aren't OpenAI-compatible.
func TestNormalize_NonChatBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), []byte(`{"foo":"bar"}`), normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected error for non-chat body")
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Bearer abc123", "abc123"},
		{"Bearer  spaced  ", "spaced"},
		{"Basic abc123", ""},
		{"abc123", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractBearer(c.in); got != c.want {
			t.Errorf("extractBearer(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
