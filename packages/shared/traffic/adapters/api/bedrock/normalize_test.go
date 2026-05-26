package bedrock

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// ID / Configure — trivial constants pinned so an accidental rename or a
// future Configure becoming non-no-op surfaces in coverage.

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if got := a.ID(); got != "bedrock" {
		t.Errorf("ID=%q want bedrock", got)
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// Dispatch fallthrough — unknown model families must return ErrUnknownSchema
// on every Extract* path (Titan / Cohere / AI21 are intentionally unsupported).

func TestExtractRequest_UnknownModelFamily(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"inputText":"hi"}`),
		"/model/amazon.titan-text-express-v1/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_UnknownModelFamily(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"results":[]}`),
		"/model/amazon.titan-text-express-v1/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_UnknownModelFamily(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`{}`),
		"/model/amazon.titan-text-express-v1/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_AnthropicOnBedrockDelegation pins that an
// anthropic.* model path on the response side delegates to the
// anthropic adapter (not just the request side).
func TestExtractResponse_AnthropicOnBedrockDelegation(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[{"type":"text","text":"hello from bedrock"}],
		"stop_reason":"end_turn"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body,
		"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello from bedrock" {
		t.Errorf("Segments=%v want [hello from bedrock]", nc.Segments)
	}
}

// TestExtractStreamChunk_AnthropicOnBedrockDelegation pins the stream
// path also delegates to the anthropic adapter.
func TestExtractStreamChunk_AnthropicOnBedrockDelegation(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk,
		"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke-with-response-stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v want [hi]", nc.Segments)
	}
}

// Llama parser error paths — malformed bytes on Response + StreamChunk.

func TestExtractResponse_LlamaMalformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`),
		"/model/meta.llama3-70b-instruct-v1:0/invoke")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractStreamChunk_LlamaMalformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`),
		"/model/meta.llama3-70b-instruct-v1:0/invoke-with-response-stream")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// RewriteResponseBody — anthropic delegation + non-anthropic unsupported.

func TestRewriteResponseBody_DelegatesToAnthropic(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant",
		"content":[{"type":"text","text":"raw SSN 123-45-6789"}],
		"stop_reason":"end_turn"}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body,
		"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		traffic.NormalizedContent{Segments: []string{"raw SSN [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if !strings.Contains(string(out), `"raw SSN [REDACTED]"`) {
		t.Errorf("rewritten body missing redacted segment: %s", out)
	}
}

func TestRewriteResponseBody_NonAnthropicUnsupported(t *testing.T) {
	body := []byte(`{"results":[{"outputText":"hi"}]}`)
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), body,
		"/model/amazon.titan-text-express-v1/invoke",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// TestRewriteResponseBody_LlamaUnsupported pins that Llama-on-Bedrock
// responses cannot be rewritten — only the anthropic.* family has a
// canonical response shape the rewriter understands.
func TestRewriteResponseBody_LlamaUnsupported(t *testing.T) {
	body := []byte(`{"generation":"hi"}`)
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), body,
		"/model/meta.llama3-70b-instruct-v1:0/invoke",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
}

// DetectResponseUsage edge cases — empty body must return no_body even on
// the dispatch fallthrough; parse_failed when no recognised model id and
// no body recognised on the dispatch path.

func TestDetectResponseUsage_EmptyBodyOnUnknownModel(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/amazon.titan-text-express-v1/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// TestDetectResponseUsage_NilResponse pins safety: a nil *http.Response
// must not panic — the implementation must short-circuit before touching
// r.Request.URL.Path.
func TestDetectResponseUsage_NilResponse(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"generation":"hi","prompt_token_count":1,"generation_token_count":1}`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed (nil resp = no model dispatch)", um.Status)
	}
}

// TestDetectResponseUsage_NilResponseEmptyBody pins the no-body path
// when r is nil and the body is empty — must not panic.
func TestDetectResponseUsage_NilResponseEmptyBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// TestDetectLlamaUsage_MalformedBody covers the parse_failed branch on
// invalid JSON entering the Llama-specific usage parser.
func TestDetectLlamaUsage_MalformedBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/meta.llama3-70b-instruct-v1:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, []byte(`not json`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

// TestDetectLlamaUsage_EmptyBody covers the no-body branch for the
// Llama dispatch path.
func TestDetectLlamaUsage_EmptyBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/meta.llama3-70b-instruct-v1:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

// modelFromBedrockPath — non-matching paths must yield "".

func TestModelFromBedrockPath_NoMatch(t *testing.T) {
	if got := modelFromBedrockPath("/foundation-models"); got != "" {
		t.Errorf("modelFromBedrockPath(/foundation-models)=%q want empty", got)
	}
	if got := modelFromBedrockPath(""); got != "" {
		t.Errorf("modelFromBedrockPath(empty)=%q want empty", got)
	}
}

// Normalize — Tier-1 entry point routes through anthropic-messages spec.

// TestNormalize_AnthropicMessagesShape pins that a body shaped like the
// Anthropic Messages API (which is what Bedrock anthropic.* upstream
// accepts) claims Tier-1 with DetectedSpec=bedrock and surfaces the
// user prompt plus model.
func TestNormalize_AnthropicMessagesShape(t *testing.T) {
	body := []byte(`{
		"anthropic_version":"bedrock-2023-05-31",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello bedrock"}]}],
		"max_tokens":1024
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "bedrock",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "bedrock" {
		t.Errorf("DetectedSpec=%q want bedrock", payload.DetectedSpec)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(payload.Messages))
	}
	if payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("role=%v want user", payload.Messages[0].Role)
	}
}

// TestNormalize_NonMatchingBody pins that a body with no recognised
// shape fails through to Tier-2 by returning normalize.ErrUnsupported.
func TestNormalize_NonMatchingBody(t *testing.T) {
	body := []byte(`{"foo":"bar","count":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "bedrock",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for non-anthropic-messages body")
	}
}
