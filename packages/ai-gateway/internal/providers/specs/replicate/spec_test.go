package replicate

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

func TestReplicate_Spec_Valid(t *testing.T) {
	s := NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
	if s.Format != provcore.FormatReplicate {
		t.Errorf("format=%q", s.Format)
	}
}

func TestReplicate_Transport_BuildURL(t *testing.T) {
	s := NewSpec(slog.Default())
	got, err := s.Transport.BuildURL(
		provcore.CallTarget{BaseURL: "https://api.replicate.com"},
		typology.WireShapeOpenAIChat, false,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "https://api.replicate.com/v1/predictions" {
		t.Errorf("got=%q", got)
	}
}

func TestReplicate_Transport_ApplyAuth(t *testing.T) {
	s := NewSpec(slog.Default())
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	if err := s.Transport.ApplyAuth(r, provcore.CallTarget{APIKey: "r8_xxx"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := r.Header.Get("Authorization"); got != "Token r8_xxx" {
		t.Errorf("Authorization=%q want 'Token r8_xxx'", got)
	}
}

func TestReplicate_Transport_ApplyAuth_MissingKey(t *testing.T) {
	s := NewSpec(slog.Default())
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	if err := s.Transport.ApplyAuth(r, provcore.CallTarget{}); err == nil {
		t.Errorf("expected error for missing key")
	}
}

func TestReplicate_Codec_EncodeRequest(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Why is the sky blue?"}
		],
		"max_tokens":256,
		"temperature":0.5
	}`)
	encRes, err := s.SchemaCodec.EncodeRequest(
		typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "meta/llama-3-70b-instruct:abc123"},
	)
	encoded := encRes.Body
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	str := string(encoded)
	if !strings.Contains(str, `"version":"meta/llama-3-70b-instruct:abc123"`) {
		t.Errorf("encoded missing version: %s", str)
	}
	if !strings.Contains(str, "Why is the sky blue?") {
		t.Errorf("encoded missing prompt: %s", str)
	}
	if !strings.Contains(str, "max_tokens") {
		t.Errorf("encoded missing max_tokens: %s", str)
	}
}

func TestReplicate_Codec_DecodeResponse_StringOutput(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{"id":"pred_1","status":"succeeded","output":"Paris.","version":"meta/x","created_at":"2024-01-01T00:00:00Z"}`)
	decRes, err := s.SchemaCodec.DecodeResponse(typology.WireShapeOpenAIChat, body, "")
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	str := string(out)
	if !strings.Contains(str, `"content":"Paris."`) {
		t.Errorf("decoded missing content: %s", str)
	}
}

func TestReplicate_Codec_DecodeResponse_ArrayOutput(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{"id":"pred_1","status":"succeeded","output":["The ","capital ","is Paris."],"version":"meta/x","created_at":"2024-01-01T00:00:00Z"}`)
	decRes, err := s.SchemaCodec.DecodeResponse(typology.WireShapeOpenAIChat, body, "")
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(string(out), "The capital is Paris.") {
		t.Errorf("decoded missing concatenated output: %s", out)
	}
}

func TestReplicate_Codec_DecodeResponse_Usage(t *testing.T) {
	s := NewSpec(slog.Default())
	body := []byte(`{"id":"x","status":"succeeded","output":"hi","metrics":{"input_token_count":7,"output_token_count":12}}`)
	decRes, err := s.SchemaCodec.DecodeResponse(typology.WireShapeOpenAIChat, body, "")
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 7 {
		t.Errorf("PromptTokens=%v", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 12 {
		t.Errorf("CompletionTokens=%v", usage.CompletionTokens)
	}
}

func TestReplicate_Stream_Output(t *testing.T) {
	s := NewSpec(slog.Default())
	raw := "event: output\ndata: Hello world\n\nevent: done\ndata: {}\n\n"
	sess, err := s.StreamDecoder.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if c.Delta != "Hello world" {
		t.Errorf("Delta=%q", c.Delta)
	}

	c, err = sess.Next(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !c.Done {
		t.Errorf("expected Done=true on second chunk")
	}
}

func TestReplicate_ErrorNormalizer_Detail(t *testing.T) {
	s := NewSpec(slog.Default())
	pe := s.ErrorNormalizer.Normalize(401, http.Header{}, []byte(`{"detail":"Authentication credentials were not provided."}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("Code=%q want auth_failed", pe.Code)
	}
	if pe.Message != "Authentication credentials were not provided." {
		t.Errorf("Message=%q", pe.Message)
	}
}

func TestReplicate_ErrorNormalizer_RateLimit(t *testing.T) {
	s := NewSpec(slog.Default())
	headers := http.Header{}
	headers.Set("retry-after", "30")
	pe := s.ErrorNormalizer.Normalize(429, headers, []byte(`{"detail":"too many requests"}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("Code=%q", pe.Code)
	}
	if pe.RetryAfter == nil {
		t.Errorf("expected RetryAfter set")
	}
}
