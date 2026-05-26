package mistral

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "mistral" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestExtractRequest_OpenAICompatDelegation(t *testing.T) {
	body := []byte(`{
		"model":"mistral-large-latest",
		"messages":[{"role":"user","content":"hi from mistral"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi from mistral" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ToolCallsDelegation(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"search","arguments":"{}"}}
	]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"search"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestDetectRequestMeta_MistralKeyClass(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.mistral.ai/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer mistral_api_key_abc")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "mistral" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "mistral-bearer" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
}

// Configure is a no-op for mistral (delegates to openai inner which is also
// a no-op). Pin both nil and populated forms so future config additions stay
// error-free.
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractResponse delegation: chat/completions response shape is decoded
// by the openai-compat inner. Segments + finish_reason metadata must
// surface end-to-end through the mistral wrapper.
func TestExtractResponse_ChatCompletionsDelegation(t *testing.T) {
	body := []byte(`{
		"id":"resp_1",
		"model":"mistral-large-latest",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":"hello from mistral"},
			"finish_reason":"stop"
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello from mistral" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

// ExtractResponse on a malformed body must surface the same
// ErrMalformed sentinel the inner adapter raises — otherwise the
// dispatcher cannot distinguish "garbage" from "no body".
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractResponse on an unknown path routes to the default branch of the
// inner adapter which returns ErrUnknownSchema — the wrapper must not
// rewrite this to nil.
func TestExtractResponse_UnknownPath(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{}`), "/v1/garbage")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractStreamChunk delegation: a single SSE chunk containing a content
// delta must produce a single Segment. The mistral wrapper does no
// transform here.
func TestExtractStreamChunk_ContentDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractStreamChunk on malformed JSON must surface ErrMalformed — a
// silently-empty NormalizedContent would hide stream corruption.
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// RewriteRequestBody on /chat/completions delegates to the openai-compat
// rewriter and substitutes per-message text — pin that the mistral
// wrapper does not lose the rewritten output or the patch count.
func TestRewriteRequestBody_ChatCompletionsDelegation(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"original"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions", traffic.NormalizedContent{
		Segments: []string{"REDACTED"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("patches=%d want 1", n)
	}
	if !strings.Contains(string(out), `"REDACTED"`) || strings.Contains(string(out), `"original"`) {
		t.Errorf("rewrite did not apply: %s", string(out))
	}
}

// RewriteRequestBody on /embeddings returns ErrRewriteUnsupported — the
// inner openai adapter rejects rewriting on this surface.
func TestRewriteRequestBody_EmbeddingsUnsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"input":"hi"}`), "/v1/embeddings", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// RewriteResponseBody on /chat/completions delegates to the openai-compat
// response rewriter, which substitutes assistant text. Pin that the
// mistral wrapper preserves the rewritten output and patch count.
func TestRewriteResponseBody_ChatCompletionsDelegation(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"raw"}}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions", traffic.NormalizedContent{
		Segments: []string{"REDACTED"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("patches=%d want 1", n)
	}
	if !strings.Contains(string(out), `"REDACTED"`) || strings.Contains(string(out), `"raw"`) {
		t.Errorf("rewrite did not apply: %s", string(out))
	}
}

// RewriteResponseBody on /embeddings is unsupported — pin the sentinel
// the inner adapter exposes for unrewritable surfaces.
func TestRewriteResponseBody_EmbeddingsUnsupported(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"data":[]}`), "/v1/embeddings", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// DetectRequestMeta with no Authorization header: ApiKeyClass and
// ApiKeyFingerprint must stay empty. The inner openai adapter's
// classifier returns "" for unknown prefixes too, so leaving them blank
// confirms the wrapper does not invent a stale "mistral-bearer" tag.
func TestDetectRequestMeta_NoAuth(t *testing.T) {
	body := []byte(`{"model":"mistral-large-latest","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.mistral.ai/v1/chat/completions", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint=%q want empty", meta.ApiKeyFingerprint)
	}
	if meta.Provider != "mistral" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "mistral-large-latest" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// DetectRequestMeta with a non-Bearer Authorization scheme: the wrapper
// must NOT stamp "mistral-bearer" — non-Bearer headers are not API
// keys and tagging them would poison attribution.
func TestDetectRequestMeta_NonBearerAuth(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.mistral.ai/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Basic xyz")
	meta := a.DetectRequestMeta(r, nil)
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty for non-Bearer scheme", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint=%q want empty", meta.ApiKeyFingerprint)
	}
}

// DetectRequestMeta with "Bearer " followed by only whitespace: after
// TrimSpace the token is empty so neither ApiKeyClass nor Fingerprint
// must be set — blank fingerprints would collide across every request.
func TestDetectRequestMeta_BearerEmptyToken(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.mistral.ai/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer    ")
	meta := a.DetectRequestMeta(r, nil)
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty for blank token", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint != "" {
		t.Errorf("ApiKeyFingerprint=%q want empty for blank token", meta.ApiKeyFingerprint)
	}
}

// DetectRequestMeta with nil request: defensive path — body-only callers
// must still get Provider stamped. The wrapper's r != nil guard means no
// ApiKey fields are stamped.
func TestDetectRequestMeta_NilRequest(t *testing.T) {
	body := []byte(`{"model":"mistral-large-latest"}`)
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, body)
	if meta.Provider != "mistral" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "mistral-large-latest" {
		t.Errorf("Model=%q", meta.Model)
	}
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty for nil request", meta.ApiKeyClass)
	}
}

// DetectResponseUsage parses the standard OpenAI-shape usage block. The
// mistral wrapper passes through unchanged, so prompt/completion split
// and Status=OK must surface end-to-end.
func TestDetectResponseUsage_OK(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("Status=%q want ok", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 11 {
		t.Errorf("PromptTokens=%v", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 7 {
		t.Errorf("CompletionTokens=%v", um.CompletionTokens)
	}
}

// DetectResponseUsage zero-length body returns NoBody — distinct from
// ParseFailed so observability can tell "no body seen" from "garbage".
func TestDetectResponseUsage_NoBody(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, nil).Status != traffic.UsageStatusNoBody {
		t.Errorf("want no_body for nil body")
	}
}

// DetectResponseUsage on non-JSON returns ParseFailed.
func TestDetectResponseUsage_ParseFailed(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`not json`)).Status != traffic.UsageStatusParseFailed {
		t.Errorf("want parse_failed")
	}
}

// Normalize: mistral speaks the openai-chat wire shape. A canonical
// chat-completions body must claim Tier-1 with DetectedSpec=mistral
// and surface the user prompt + model on the normalized payload.
func TestNormalize_OpenAIChatShape(t *testing.T) {
	body := []byte(`{
		"model":"mistral-large-latest",
		"messages":[{"role":"user","content":"hello mistral"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1/chat/completions",
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
	if payload.Model != "mistral-large-latest" {
		t.Errorf("Model=%q", payload.Model)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(payload.Messages))
	}
	if payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("role=%v want user", payload.Messages[0].Role)
	}
}

// Normalize on a body that is not openai-chat shaped must error so the
// coordinator advances to Tier 2 / Tier 3 — never silently succeed with
// an empty payload, which would block lower-tier detectors.
func TestNormalize_NonChatBody(t *testing.T) {
	body := []byte(`{"foo":"bar","count":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected error for non-chat body")
	}
}
