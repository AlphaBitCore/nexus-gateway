package gemini_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
)

func TestGemini_BuildURL_NonStream(t *testing.T) {
	tr := gemini.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{BaseURL: "https://generativelanguage.googleapis.com", ProviderModelID: "gemini-1.5-pro"},
		typology.WireShapeGeminiGenerateContent,
		false,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got != "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent" {
		t.Errorf("got %q", got)
	}
}

func TestGemini_BuildURL_Stream(t *testing.T) {
	tr := gemini.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{BaseURL: "https://generativelanguage.googleapis.com", ProviderModelID: "gemini-2.0-flash"},
		typology.WireShapeGeminiGenerateContent,
		true,
	)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(got, ":streamGenerateContent?alt=sse") {
		t.Errorf("streaming url malformed: %s", got)
	}
}

func TestGemini_ApplyAuth(t *testing.T) {
	tr := gemini.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "AIza"}); err != nil {
		t.Fatalf("%v", err)
	}
	if req.Header.Get("x-goog-api-key") != "AIza" {
		t.Errorf("x-goog-api-key: %q", req.Header.Get("x-goog-api-key"))
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Authorization must not leak")
	}
}

func TestGemini_Codec_EncodeRequest(t *testing.T) {
	canonical := []byte(`{
		"messages":[
			{"role":"system","content":"Be helpful."},
			{"role":"user","content":"Hi"},
			{"role":"assistant","content":"Hello"}
		],
		"temperature":0.7,
		"max_tokens":128
	}`)
	encRes, err := gemini.NewCodec().EncodeRequest(typology.WireShapeGeminiGenerateContent, canonical, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("%v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	sys, _ := parsed["systemInstruction"].(map[string]any)
	if sys == nil {
		t.Fatalf("missing systemInstruction")
	}
	contents, _ := parsed["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents len=%d", len(contents))
	}
	first, _ := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("first role %v", first["role"])
	}
	second, _ := contents[1].(map[string]any)
	if second["role"] != "model" {
		t.Errorf("second role %v", second["role"])
	}
	gen, _ := parsed["generationConfig"].(map[string]any)
	if gen == nil || gen["temperature"].(float64) != 0.7 {
		t.Errorf("generationConfig: %+v", gen)
	}
}

func TestGemini_Codec_DecodeResponse(t *testing.T) {
	native := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"Hello"}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3},
		"modelVersion":"gemini-1.5-pro"
	}`)
	decRes, err := gemini.NewCodec().DecodeResponse(typology.WireShapeGeminiGenerateContent, native, "")
	canon := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("%v", err)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(canon, &parsed)
	if parsed.Choices[0].Message.Content != "Hello" {
		t.Errorf("content: %q", parsed.Choices[0].Message.Content)
	}
	if parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason: %q", parsed.Choices[0].FinishReason)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 1 {
		t.Errorf("usage completion mismatch")
	}
}

func TestGemini_StreamDecoder(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"He"}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"llo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`,
		``,
	}, "\n")

	dec := gemini.NewStreamDecoder(slog.Default())
	session, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer session.Close() //nolint:errcheck

	var text string
	var usage *provcore.Usage
	done := false
	for {
		ch, err := session.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		text += ch.Delta
		if ch.Usage != nil {
			usage = ch.Usage
		}
		if ch.Done {
			done = true
		}
	}
	if text != "Hello" {
		t.Errorf("text=%q", text)
	}
	if !done {
		t.Errorf("missing done")
	}
	if usage == nil {
		t.Errorf("no usage")
	}
}

func TestGemini_ErrorNormalizer(t *testing.T) {
	norm := gemini.NewErrorNormalizer()
	pe := norm.Normalize(429, nil, []byte(`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: %q", pe.Code)
	}
}
