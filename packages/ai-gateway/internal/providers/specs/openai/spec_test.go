package openai_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// testAdapter wires a [provcore.Adapter] over the spec returned by
// openai.NewSpec for use in table tests.
func testAdapter(t *testing.T) provcore.Adapter {
	t.Helper()
	return provdispatch.NewSpecAdapter(openai.NewSpec(slog.Default()), slog.Default())
}

// TestSpec_RequestShapes pins the binding: spec_openai is the only
// adapter today that natively serves /v1/responses; this is the source
// of truth the canonical bridge consults via Adapter.SupportsShape.
// Adding a sibling adapter to this list requires empirical evidence
// (captured 200 from the real upstream endpoint) per provider-adapter-
// architecture.md §3a Rule 7.
func TestSpec_RequestShapes(t *testing.T) {
	spec := openai.NewSpec(slog.Default())
	shapes := spec.RequestShapes
	wantChat, wantResponses := false, false
	for _, s := range shapes {
		switch s {
		case typology.WireShapeOpenAIChat:
			wantChat = true
		case typology.WireShapeOpenAIResponses:
			wantResponses = true
		}
	}
	if !wantChat {
		t.Error("spec_openai must declare chat-completions shape support")
	}
	if !wantResponses {
		t.Error("spec_openai must declare responses-api shape support")
	}
	if !spec.SupportsShape(typology.WireShapeOpenAIChat) {
		t.Error("SupportsShape(chat-completions) must be true")
	}
	if !spec.SupportsShape(typology.WireShapeOpenAIResponses) {
		t.Error("SupportsShape(responses-api) must be true")
	}
	if spec.SupportsShape(typology.WireShape("nonexistent")) {
		t.Error("SupportsShape rejects undeclared shapes")
	}
}

func TestOpenAI_Transport_BuildURL(t *testing.T) {
	transport := openai.NewTransport(slog.Default())
	tgt := provcore.CallTarget{BaseURL: "https://api.openai.com/"}

	cases := []struct {
		endpoint typology.WireShape
		want     string
	}{
		{typology.WireShapeOpenAIChat, "https://api.openai.com/v1/chat/completions"},
		{typology.WireShapeOpenAIEmbeddings, "https://api.openai.com/v1/embeddings"},
		{typology.WireShapeNone, "https://api.openai.com/v1/models"},
	}
	for _, tc := range cases {
		got, err := transport.BuildURL(tgt, tc.endpoint, false)
		if err != nil {
			t.Fatalf("%s: %v", tc.endpoint, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.endpoint, got, tc.want)
		}
	}
}

func TestOpenAI_Transport_ApplyAuth(t *testing.T) {
	transport := openai.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	if err := transport.ApplyAuth(req, provcore.CallTarget{APIKey: "sk-secret"}); err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-secret" {
		t.Errorf("Authorization got %q", got)
	}
}

func TestOpenAI_Transport_ApplyAuthMissingKey(t *testing.T) {
	transport := openai.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	if err := transport.ApplyAuth(req, provcore.CallTarget{}); err == nil {
		t.Fatalf("expected error on empty API key")
	}
}

func TestOpenAI_Codec_RoundTrip(t *testing.T) {
	codec := openai.IdentityCodec()
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	encRes, err := codec.EncodeRequest(typology.WireShapeOpenAIChat, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("encode must be identity; got %q", out)
	}
	decRes, err := codec.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	canon := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(canon) != string(body) {
		t.Errorf("decode body identity mismatch")
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 1 {
		t.Errorf("usage.PromptTokens=%v", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 2 {
		t.Errorf("usage.CompletionTokens=%v", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 3 {
		t.Errorf("usage.TotalTokens=%v", usage.TotalTokens)
	}
}

// TestIdentityCodec_DecodeCachedAndReasoningTokens covers the audit fix:
// IdentityCodec must surface OpenAI-canonical cache + reasoning splits on
// the typed Usage envelope so DeepSeek / GLM / Azure-OpenAI cost analytics
// don't all need to re-parse the canonical body. Also covers DeepSeek's
// non-OpenAI prompt_cache_hit_tokens field as a CacheReadTokens synonym.
func TestIdentityCodec_DecodeCachedAndReasoningTokens(t *testing.T) {
	codec := openai.IdentityCodec()
	t.Run("openai_canonical", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":15}}}`)
		decRes, err := codec.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
		usage := decRes.Usage
		if err != nil {
			t.Fatal(err)
		}
		if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 40 {
			t.Errorf("CacheReadTokens=%v want 40", usage.CacheReadTokens)
		}
		if usage.ReasoningTokens == nil || *usage.ReasoningTokens != 15 {
			t.Errorf("ReasoningTokens=%v want 15", usage.ReasoningTokens)
		}
	})
	t.Run("deepseek_prompt_cache_hit_tokens_fallback", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`)
		decRes, err := codec.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
		usage := decRes.Usage
		if err != nil {
			t.Fatal(err)
		}
		if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 80 {
			t.Errorf("CacheReadTokens (DeepSeek prompt_cache_hit_tokens fallback)=%v want 80", usage.CacheReadTokens)
		}
	})
}

func TestOpenAI_ErrorNormalize(t *testing.T) {
	norm := openai.ErrorNormalizerInstance()

	cases := []struct {
		name       string
		status     int
		body       string
		headers    http.Header
		wantCode   string
		wantRetry  bool
		retryAfter string
	}{
		{
			name:     "invalid",
			status:   400,
			body:     `{"error":{"type":"invalid_request_error","message":"bad model"}}`,
			wantCode: provcore.CodeInvalidRequest,
		},
		{
			name:     "auth",
			status:   401,
			body:     `{"error":{"type":"authentication_error","message":"bad key"}}`,
			wantCode: provcore.CodeAuthFailed,
		},
		{
			name:       "rate",
			status:     429,
			body:       `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`,
			wantCode:   provcore.CodeRateLimited,
			wantRetry:  true,
			retryAfter: "2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			if tc.retryAfter != "" {
				headers.Set("Retry-After", tc.retryAfter)
			}
			pe := norm.Normalize(tc.status, headers, []byte(tc.body))
			if pe == nil {
				t.Fatalf("nil ProviderError")
			}
			if pe.Code != tc.wantCode {
				t.Errorf("code got %q want %q", pe.Code, tc.wantCode)
			}
			if tc.wantRetry && pe.RetryAfter == nil {
				t.Errorf("expected RetryAfter populated")
			}
		})
	}
}

func TestOpenAI_StreamDecoder(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		``,
		`data: {"id":"1","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	dec := openai.NewStreamDecoder(slog.Default())
	session, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("open: %v", err)
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
		t.Errorf("delta got %q want Hello", text)
	}
	if !done {
		t.Errorf("expected done chunk")
	}
	if usage == nil || usage.PromptTokens == nil || *usage.PromptTokens != 3 {
		t.Errorf("usage prompt tokens mismatch: %+v", usage)
	}
}

func TestOpenAI_Execute_Passthrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer key123" {
			t.Errorf("Authorization header got %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer server.Close()

	a := testAdapter(t)
	resp, err := a.Execute(context.Background(), provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"gpt"}`),
		Target: provcore.CallTarget{
			BaseURL: server.URL,
			APIKey:  "key123",
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	if resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 15 {
		t.Errorf("usage total mismatch: %+v", resp.Usage)
	}
}

func TestOpenAI_Execute_ErrorNormalized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"bad"}}`)
	}))
	defer server.Close()

	a := testAdapter(t)
	_, err := a.Execute(context.Background(), provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"gpt"}`),
		Target:     provcore.CallTarget{BaseURL: server.URL, APIKey: "bad"},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code got %q", pe.Code)
	}
}
