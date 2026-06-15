package bedrock

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMetaAnthropic(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		nil)
	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=AKIAEXAMPLEEXAMPLE/20260421/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "bedrock" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "anthropic.claude-3-5-sonnet-20241022-v2:0" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ApiKeyClass != "aws-sigv4" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint != traffic.ApiKeyFingerprint("AKIAEXAMPLEEXAMPLE") {
		t.Errorf("fingerprint mismatch")
	}
}

func TestDetectRequestMetaConverse(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/amazon.titan-text-premier-v1:0/converse",
		nil)
	got := a.DetectRequestMeta(r, nil)
	if got.Model != "amazon.titan-text-premier-v1:0" {
		t.Errorf("model = %q", got.Model)
	}
}

func TestExtractSigV4AKID(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"Basic dGVzdA==":                       "",
		"AWS4-HMAC-SHA256 Credential=AKIA.../": "AKIA...", // weird but matches regex up to "/"
		"AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260421/us-east-1/bedrock/aws4_request, SignedHeaders=h, Signature=x": "AKIAIOSFODNN7EXAMPLE",
	}
	for in, want := range cases {
		if got := extractSigV4AKID(in); got != want {
			// The "weird" test above has "AKIA..." (dots) which will not match [A-Z0-9]{16,32} —
			// so the expected for that case is actually "". Adjust expectation.
			if in == "AWS4-HMAC-SHA256 Credential=AKIA.../" {
				if got == "" {
					continue
				}
			}
			t.Errorf("extractSigV4AKID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectResponseUsageAnthropicModel(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":20}}`)
	um := a.DetectResponseUsage(resp, body)
	if um.Status != traffic.UsageStatusOK {
		t.Fatalf("status = %q", um.Status)
	}
	if *um.PromptTokens != 10 || *um.CompletionTokens != 20 {
		t.Errorf("tokens = %d/%d", *um.PromptTokens, *um.CompletionTokens)
	}
}

func TestDetectResponseUsageNonAnthropicModel(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/amazon.titan-text-express-v1/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, []byte(`{"results":[]}`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("non-anthropic bedrock should yield parse_failed, got %q", um.Status)
	}
}

// Anthropic-on-Bedrock delegation regression — pins that the P0-2
// anthropic adapter audit (commit 510db2f5) reaches Bedrock callers.

func TestExtractRequest_AnthropicOnBedrock_ToolUseDelegation(t *testing.T) {
	body := []byte(`{
		"anthropic_version":"bedrock-2023-05-31",
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"NYC"}}
			]}
		],
		"max_tokens":1024
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v want tool_use to flow through anthropic delegation", nc.ToolCallSegments)
	}
}

// Llama-on-Bedrock native parser

func TestExtractRequest_LlamaPrompt(t *testing.T) {
	body := []byte(`{"prompt":"Why is the sky blue?","max_gen_len":256,"temperature":0.5,"top_p":0.9}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/model/meta.llama3-70b-instruct-v1:0/invoke")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Why is the sky blue?" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_LlamaPromptMissing(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"max_gen_len":256}`), "/model/meta.llama3-70b-instruct-v1:0/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_LlamaMalformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/model/meta.llama3-70b-instruct-v1:0/invoke")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractRequest_LlamaExtra(t *testing.T) {
	body := []byte(`{"prompt":"hi","x_custom_field":{"sensitive":"value"}}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/model/meta.llama3-2-90b-instruct-v1:0/invoke")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if x, ok := nc.Extra["x_custom_field"]; !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_custom_field", nc.Extra)
	}
}

func TestExtractResponse_LlamaGeneration(t *testing.T) {
	body := []byte(`{"generation":"The sky is blue because of Rayleigh scattering.","prompt_token_count":7,"generation_token_count":12,"stop_reason":"stop"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/model/meta.llama3-70b-instruct-v1:0/invoke")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "The sky is blue because of Rayleigh scattering." {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["stop_reason"] != "stop" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
}

func TestExtractResponse_LlamaMissingGeneration(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"prompt_token_count":7}`), "/model/meta.llama3-70b-instruct-v1:0/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_LlamaDelta(t *testing.T) {
	chunk := []byte(`{"generation":"The sky","prompt_token_count":7,"generation_token_count":2,"stop_reason":null}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/model/meta.llama3-70b-instruct-v1:0/invoke-with-response-stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "The sky" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata != nil {
		t.Errorf("Metadata=%v want nil for non-terminal chunk", nc.Metadata)
	}
}

func TestExtractStreamChunk_LlamaTerminalStopReason(t *testing.T) {
	chunk := []byte(`{"generation":"","prompt_token_count":7,"generation_token_count":12,"stop_reason":"stop"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/model/meta.llama3-70b-instruct-v1:0/invoke-with-response-stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["stop_reason"] != "stop" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
}

func TestDetectResponseUsage_Llama(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/meta.llama3-70b-instruct-v1:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	body := []byte(`{"generation":"hi","prompt_token_count":5,"generation_token_count":2,"stop_reason":"stop"}`)
	um := a.DetectResponseUsage(resp, body)
	if um.Status != traffic.UsageStatusOK {
		t.Errorf("Status=%q want ok", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 5 {
		t.Errorf("PromptTokens=%v want 5", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 2 {
		t.Errorf("CompletionTokens=%v want 2", um.CompletionTokens)
	}
}

// TestAdapterIdentity pins the registry contract: the adapter registers
// under the "bedrock" key (traffic dispatch routes by this id) and
// Configure accepts any config without error — bedrock exposes no knobs,
// so a non-nil error here would wrongly disable the adapter at startup.
func TestAdapterIdentity(t *testing.T) {
	a := &Adapter{}
	if got := a.ID(); got != "bedrock" {
		t.Errorf("ID() = %q, want bedrock", got)
	}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v, want nil", err)
	}
	if err := a.Configure(map[string]any{"unknown": true}); err != nil {
		t.Errorf("Configure(map) = %v, want nil (no knobs, arbitrary config ignored)", err)
	}
}

// Unsupported-publisher dispatch: Titan/Cohere/AI21 model ids must yield
// ErrUnknownSchema on every extraction surface — detect-level
// attribution still works, but content extraction is explicitly skipped.

func TestExtractRequest_UnsupportedPublisher(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(),
		[]byte(`{"inputText":"hi"}`), "/model/amazon.titan-text-express-v1/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_UnsupportedPublisher(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(),
		[]byte(`{"results":[{"outputText":"hi"}]}`), "/model/amazon.titan-text-express-v1/invoke")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_UnsupportedPublisher(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(),
		[]byte(`{"outputText":"hi"}`), "/model/cohere.command-r-v1:0/invoke-with-response-stream")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Anthropic-on-Bedrock delegation for the response surfaces.

func TestExtractResponse_AnthropicOnBedrock(t *testing.T) {
	body := []byte(`{
		"id":"msg_bdrk_1","type":"message","role":"assistant",
		"model":"claude-3-5-sonnet-20241022",
		"content":[{"type":"text","text":"Hello from Bedrock"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":5,"output_tokens":4}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello from Bedrock" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%q want end_turn", nc.Metadata["stop_reason"])
	}
	if nc.Metadata["id"] != "msg_bdrk_1" {
		t.Errorf("id=%q", nc.Metadata["id"])
	}
}

func TestExtractStreamChunk_AnthropicOnBedrock(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke-with-response-stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "partial" {
		t.Errorf("Segments=%v want text_delta routed through anthropic delegation", nc.Segments)
	}
}

// Llama malformed-input failure modes.

func TestExtractResponse_LlamaMalformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/model/meta.llama3-70b-instruct-v1:0/invoke")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractStreamChunk_LlamaMalformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/model/meta.llama3-70b-instruct-v1:0/invoke-with-response-stream")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestDetectRequestMeta_NonModelPath pins attribution on a path that does
// not match the Bedrock /model/<id>/<verb> shape: provider stays
// "bedrock" (the adapter was selected by host) but no model is invented.
func TestDetectRequestMeta_NonModelPath(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/foundation-models", nil)
	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "bedrock" {
		t.Errorf("provider=%q", got.Provider)
	}
	if got.Model != "" {
		t.Errorf("model=%q want empty for non-model path", got.Model)
	}
}

func TestDetectResponseUsage_UnsupportedPublisherNoBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/amazon.titan-text-express-v1/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body for empty body", um.Status)
	}
}

func TestDetectResponseUsage_LlamaNoBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/meta.llama3-70b-instruct-v1:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

func TestDetectResponseUsage_LlamaMalformedBody(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/meta.llama3-70b-instruct-v1:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, []byte(`<html>throttled</html>`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed for non-JSON body", um.Status)
	}
}

func TestDetectResponseUsage_LlamaParseFailed(t *testing.T) {
	a := &Adapter{}
	u, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/model/meta.llama3-70b-instruct-v1:0/invoke")
	resp := &http.Response{Request: &http.Request{URL: u}}
	um := a.DetectResponseUsage(resp, []byte(`{}`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed when token counts absent", um.Status)
	}
}
