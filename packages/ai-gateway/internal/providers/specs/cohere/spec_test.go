package cohere

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestCohere_Spec_Valid(t *testing.T) {
	s := NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatCohere {
		t.Errorf("format=%q", s.Format)
	}
}

func TestCohere_Transport_BuildURL(t *testing.T) {
	s := NewSpec(slog.Default())
	got, err := s.Transport.BuildURL(
		provcore.CallTarget{BaseURL: "https://api.cohere.com"},
		typology.WireShapeCohereChat, false,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "https://api.cohere.com/v2/chat" {
		t.Errorf("got=%q", got)
	}
}

func TestCohere_Transport_Auth(t *testing.T) {
	s := NewSpec(slog.Default())
	r, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	if err := s.Transport.ApplyAuth(r, provcore.CallTarget{APIKey: "co_xxx"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer co_xxx" {
		t.Errorf("Authorization=%q", got)
	}
}

func TestCohere_Codec_EncodeRequest_Passthrough(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{"model":"command-r-plus","messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := s.SchemaCodec.EncodeRequest(typology.WireShapeCohereChat, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected pass-through, got %s", out)
	}
}

func TestCohere_Codec_EncodeRequest_InjectsModel(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := s.SchemaCodec.EncodeRequest(
		typology.WireShapeCohereChat, body,
		provcore.CallTarget{ProviderModelID: "command-r-plus"},
	)
	out := encRes.Body
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(string(out), `"command-r-plus"`) {
		t.Errorf("expected injected model: %s", out)
	}
}

func TestCohere_Codec_DecodeResponse_TextAndUsage(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{
		"id":"resp_1",
		"model":"command-r-plus",
		"message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},
		"finish_reason":"COMPLETE",
		"usage":{"tokens":{"input_tokens":42,"output_tokens":13}}
	}`)
	decRes, err := s.SchemaCodec.DecodeResponse(typology.WireShapeCohereChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	str := string(out)
	if !strings.Contains(str, `"choices"`) || !strings.Contains(str, `"hello"`) {
		t.Errorf("decoded missing choices/content: %s", str)
	}
	if !strings.Contains(str, `"finish_reason":"COMPLETE"`) {
		t.Errorf("decoded missing finish_reason: %s", str)
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 42 {
		t.Errorf("PromptTokens=%v", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 13 {
		t.Errorf("CompletionTokens=%v", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 55 {
		t.Errorf("TotalTokens=%v", usage.TotalTokens)
	}
}

func TestCohere_Codec_DecodeResponse_ToolPlanAndCalls(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{
		"id":"resp_1",
		"message":{
			"role":"assistant",
			"content":[{"type":"text","text":"calling tool"}],
			"tool_plan":"I will call get_weather",
			"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]
		},
		"finish_reason":"TOOL_CALL"
	}`)
	decRes, err := s.SchemaCodec.DecodeResponse(typology.WireShapeCohereChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	str := string(out)
	if !strings.Contains(str, `"reasoning_content":"I will call get_weather"`) {
		t.Errorf("decoded missing tool_plan→reasoning_content: %s", str)
	}
	if !strings.Contains(str, `"get_weather"`) {
		t.Errorf("decoded missing tool_calls: %s", str)
	}
}

func TestCohere_Stream_ContentDelta(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := `data: {"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"Hello"}}}}` + "\n\n"
	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if c.Delta != "Hello" {
		t.Errorf("Delta=%q", c.Delta)
	}
	if !strings.Contains(string(c.RawBytes), `"content":"Hello"`) {
		t.Errorf("RawBytes missing canonical content: %s", c.RawBytes)
	}
}

func TestCohere_Stream_ToolPlanDelta(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := `data: {"type":"tool-plan-delta","delta":{"message":{"tool_plan":"thinking"}}}` + "\n\n"
	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if c.ReasoningDelta != "thinking" {
		t.Errorf("ReasoningDelta=%q", c.ReasoningDelta)
	}
}

func TestCohere_Stream_ToolCallStart(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := `data: {"type":"tool-call-start","index":0,"delta":{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}]}}}` + "\n\n"
	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(c.ToolCallDeltas) != 1 || c.ToolCallDeltas[0].Name != "get_weather" {
		t.Errorf("ToolCallDeltas=%+v", c.ToolCallDeltas)
	}
}

func TestCohere_Stream_MessageEnd(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := `data: {"type":"message-end","delta":{"finish_reason":"COMPLETE"},"usage":{"tokens":{"input_tokens":5,"output_tokens":2}}}` + "\n\n"
	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !c.Done {
		t.Errorf("expected Done=true")
	}
	if c.Usage == nil || c.Usage.PromptTokens == nil || *c.Usage.PromptTokens != 5 {
		t.Errorf("Usage=%+v", c.Usage)
	}
	if !strings.Contains(string(c.RawBytes), `"finish_reason":"stop"`) {
		t.Errorf("expected canonical finish_reason=stop in RawBytes: %s", c.RawBytes)
	}
	if c.FinishReason != "stop" {
		t.Errorf("FinishReason=%q, want stop (mapped from COMPLETE)", c.FinishReason)
	}
}

// TestCohere_Stream_FinishReason_MaxTokens proves the Cohere decoder surfaces
// the mapped canonical finish_reason on the Chunk so a re-encoder (buffer mode)
// can preserve it instead of collapsing to "stop".
func TestCohere_Stream_FinishReason_MaxTokens(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := `data: {"type":"message-end","delta":{"finish_reason":"MAX_TOKENS"}}` + "\n\n"
	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if c.FinishReason != "length" {
		t.Errorf("FinishReason=%q, want length", c.FinishReason)
	}
}

func TestCohere_ErrorNormalizer(t *testing.T) {
	s := NewSpec(slog.Default())
	pe := s.ErrorNormalizer.Normalize(401, http.Header{}, []byte(`{"message":"unauthorized"}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("Code=%q", pe.Code)
	}
	if pe.Message != "unauthorized" {
		t.Errorf("Message=%q", pe.Message)
	}
}

func TestCohere_FinishReasonMap(t *testing.T) {
	cases := map[string]string{
		"COMPLETE":      "stop",
		"MAX_TOKENS":    "length",
		"STOP_SEQUENCE": "stop",
		"TOOL_CALL":     "tool_calls",
		"ERROR":         "error",
		"unknown":       "unknown",
	}
	for in, want := range cases {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q)=%q want %q", in, got, want)
		}
	}
}
