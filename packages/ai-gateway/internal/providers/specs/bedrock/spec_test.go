package bedrock_test

import (
	"context"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/bedrock"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestBedrock_BuildURL(t *testing.T) {
	tr := bedrock.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "anthropic.claude-3-sonnet-20240229-v1:0",
			Extras:          map[string]string{"aws.region": "us-east-1"},
		},
		typology.WireShapeBedrockConverse, false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, "bedrock-runtime.us-east-1.amazonaws.com") {
		t.Errorf("host: %s", got)
	}
	if !strings.HasSuffix(got, "/invoke") {
		t.Errorf("action: %s", got)
	}
}

func TestBedrock_BuildURL_Stream(t *testing.T) {
	tr := bedrock.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "anthropic.claude-3-sonnet-20240229-v1:0",
			Extras:          map[string]string{"aws.region": "us-east-1"},
		},
		typology.WireShapeBedrockConverse, true,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.HasSuffix(got, "/invoke-with-response-stream") {
		t.Errorf("stream action: %s", got)
	}
}

func TestBedrock_Codec_Encode(t *testing.T) {
	codec := bedrock.NewCodec(slog.Default())
	encRes, err := codec.EncodeRequest(
		typology.WireShapeBedrockConverse,
		[]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":32}`),
		provcore.CallTarget{ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0"},
	)
	out := encRes.Body
	if err != nil {
		t.Fatalf("%v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if parsed["anthropic_version"] != "bedrock-2023-05-31" {
		t.Errorf("missing anthropic_version: %v", parsed)
	}
	if parsed["max_tokens"].(float64) != 32 {
		t.Errorf("max_tokens: %v", parsed["max_tokens"])
	}
	if _, ok := parsed["model"]; ok {
		t.Errorf("model must be stripped for Bedrock body")
	}
}

// TestBedrock_Codec_EncodeWithTools confirms the Anthropic-codec delegation
// preserves canonical OpenAI tools[] / tool_choice on the way out — the
// previous local Bedrock codec silently dropped both, so this is the
// regression test for the audit finding (Bedrock spec has the same shape
// as native Anthropic for tool use).
func TestBedrock_Codec_EncodeWithTools(t *testing.T) {
	codec := bedrock.NewCodec(slog.Default())
	canon := []byte(`{
		"messages":[{"role":"user","content":"weather?"}],
		"max_tokens":64,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"x","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}}
	}`)
	encRes, err := codec.EncodeRequest(
		typology.WireShapeBedrockConverse,
		canon,
		provcore.CallTarget{ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0"},
	)
	out := encRes.Body
	if err != nil {
		t.Fatalf("%v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	tools, ok := parsed["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools dropped: %v", parsed)
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "get_weather" {
		t.Errorf("tools[0].name=%v want get_weather", first["name"])
	}
	tc, _ := parsed["tool_choice"].(map[string]any)
	if tc["type"] != "tool" {
		t.Errorf("tool_choice.type=%v want tool", tc["type"])
	}
}

// TestBedrock_Codec_DecodeToolUse covers the audit's "tool-use response not
// decoded" finding: Bedrock returns Anthropic-shape responses with tool_use
// content blocks; canonical OpenAI tool_calls[] must be populated.
func TestBedrock_Codec_DecodeToolUse(t *testing.T) {
	codec := bedrock.NewCodec(slog.Default())
	native := []byte(`{
		"id":"msg_x","type":"message","role":"assistant",
		"content":[{"type":"tool_use","id":"toolu_42","name":"get_weather","input":{"location":"SF"}}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	decRes, err := codec.DecodeResponse(typology.WireShapeBedrockConverse, native, "", provcore.DecodeContext{})
	canon := decRes.CanonicalBody
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(canon, &parsed); err != nil {
		t.Fatal(err)
	}
	choices, _ := parsed["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices: %s", string(canon))
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	tcalls, ok := msg["tool_calls"].([]any)
	if !ok || len(tcalls) == 0 {
		t.Fatalf("tool_calls missing: %v", msg)
	}
	first, _ := tcalls[0].(map[string]any)
	if first["id"] != "toolu_42" {
		t.Errorf("tool_call id=%v want toolu_42", first["id"])
	}
	fn, _ := first["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name=%v", fn["name"])
	}
	finish, _ := choices[0].(map[string]any)["finish_reason"].(string)
	if finish != "tool_calls" {
		t.Errorf("finish_reason=%q want tool_calls", finish)
	}
}

func TestBedrock_Execute_SignsRequest(t *testing.T) {
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer server.Close()

	a := provdispatch.NewSpecAdapter(bedrock.NewSpec(slog.Default()), slog.Default())
	resp, err := a.Execute(context.Background(), provcore.Request{
		WireShape:  typology.WireShapeBedrockConverse,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Target: provcore.CallTarget{
			ProviderModelID: "anthropic.claude-3-sonnet-20240229-v1:0",
			BaseURL:         server.URL,
			Extras: map[string]string{
				"aws.region":    "us-east-1",
				"aws.accessKey": "AKIA",
				"aws.secretKey": "secret",
			},
		},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	if !strings.HasPrefix(sawAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("expected SigV4 Authorization, got %q", sawAuth)
	}
	// SigV4 Credential clause: Credential=<access-key>/<date>/<region>/<service>/aws4_request.
	// Bedrock runtime endpoints (InvokeModel) MUST sign with service name
	// `bedrock-runtime`; using `bedrock` (control-plane scope) makes AWS
	// reject every call with SignatureDoesNotMatch. Pin it explicitly.
	credIdx := strings.Index(sawAuth, "Credential=")
	if credIdx < 0 {
		t.Fatalf("Authorization missing Credential clause: %q", sawAuth)
	}
	rest := sawAuth[credIdx+len("Credential="):]
	if comma := strings.Index(rest, ","); comma >= 0 {
		rest = rest[:comma]
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 5 {
		t.Fatalf("Credential clause malformed: %q", rest)
	}
	if parts[3] != "bedrock-runtime" {
		t.Errorf("SigV4 service=%q want bedrock-runtime (control-plane `bedrock` scope rejects InvokeModel)", parts[3])
	}
}
