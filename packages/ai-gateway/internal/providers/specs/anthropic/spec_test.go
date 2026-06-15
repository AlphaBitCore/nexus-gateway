package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func adapter() provcore.Adapter {
	return provdispatch.NewSpecAdapter(anthropic.NewSpec(slog.Default()), slog.Default())
}

func TestAnthropic_BuildURL(t *testing.T) {
	tr := anthropic.NewTransport(slog.Default())
	got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.anthropic.com"}, typology.WireShapeAnthropicMessages, false)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("got %q", got)
	}
}

func TestAnthropic_ApplyAuth(t *testing.T) {
	tr := anthropic.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "key"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if req.Header.Get("x-api-key") != "key" {
		t.Errorf("x-api-key missing")
	}
	if req.Header.Get("anthropic-version") == "" {
		t.Errorf("anthropic-version missing")
	}
	// Authorization must never be set.
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Authorization must not leak: %q", req.Header.Get("Authorization"))
	}
}

// TestAnthropic_ApplyAuth_RespectsClientVersion pins that a client-sent
// anthropic-version is preserved verbatim. Pre-fix the gateway always
// stamped the target / hardcoded default, so a client opting into a
// newer version (e.g. for context-management or new tools) was
// silently downgraded.
func TestAnthropic_ApplyAuth_RespectsClientVersion(t *testing.T) {
	tr := anthropic.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	req.Header.Set("anthropic-version", "2024-10-22")
	err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "key"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got := req.Header.Get("anthropic-version"); got != "2024-10-22" {
		t.Errorf("anthropic-version: want client-sent 2024-10-22, got %q", got)
	}
}

// TestAnthropic_ApplyAuth_RespectsClientBeta pins that a client-sent
// anthropic-beta is preserved verbatim, and the per-target default
// only stamps when the client omitted it. Without this the gateway
// would clobber Claude Code's beta opt-ins (context-management,
// prompt-caching, ...) with whatever the credential row set.
func TestAnthropic_ApplyAuth_RespectsClientBeta(t *testing.T) {
	tr := anthropic.NewTransport(slog.Default())

	// Client sends a beta header — gateway must leave it alone.
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	req.Header.Set("anthropic-beta", "context-management-2025-04-15")
	target := provcore.CallTarget{APIKey: "key", Extras: map[string]string{
		"anthropic.beta": "credential-default-beta",
	}}
	if err := tr.ApplyAuth(req, target); err != nil {
		t.Fatalf("%v", err)
	}
	if got := req.Header.Get("anthropic-beta"); got != "context-management-2025-04-15" {
		t.Errorf("anthropic-beta: client value must win, got %q", got)
	}

	// Client omits — target default applies.
	req2, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req2, target); err != nil {
		t.Fatalf("%v", err)
	}
	if got := req2.Header.Get("anthropic-beta"); got != "credential-default-beta" {
		t.Errorf("anthropic-beta: target default must stamp when client omits, got %q", got)
	}

	// No client beta, no credential beta → gateway default (prompt-caching-2024-07-31).
	req3, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req3, provcore.CallTarget{APIKey: "key"}); err != nil {
		t.Fatalf("%v", err)
	}
	if got := req3.Header.Get("anthropic-beta"); got != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic-beta: gateway default expected when both client and credential omit, got %q", got)
	}
}

func TestAnthropic_Codec_EncodeRequest_FromOpenAI(t *testing.T) {
	canonical := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":512,
		"temperature":0.3,
		"messages":[
			{"role":"system","content":"You are brief."},
			{"role":"user","content":"Hi"}
		]
	}`)
	encRes, err := anthropic.NewSpec(slog.Default()).SchemaCodec.EncodeRequest(
		typology.WireShapeAnthropicMessages,
		canonical,
		provcore.CallTarget{ProviderModelID: "claude-3-5-sonnet-20241022"},
	)
	native := encRes.Body
	if err != nil {
		t.Fatalf("%v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(native, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed["system"] != "You are brief." {
		t.Errorf("system: %v", parsed["system"])
	}
	msgs, _ := parsed["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	if parsed["max_tokens"].(float64) != 512 {
		t.Errorf("max_tokens: %v", parsed["max_tokens"])
	}
}

func TestAnthropic_Codec_DecodeResponse(t *testing.T) {
	native := []byte(`{
		"id":"msg_1",
		"model":"claude-3-5-sonnet-20241022",
		"content":[{"type":"text","text":"Hello!"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	decRes, err := anthropic.NewSpec(slog.Default()).SchemaCodec.DecodeResponse(typology.WireShapeAnthropicMessages, native, "", provcore.DecodeContext{})
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
	if err := json.Unmarshal(canon, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Choices[0].Message.Content != "Hello!" {
		t.Errorf("content got %q", parsed.Choices[0].Message.Content)
	}
	if parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason got %q", parsed.Choices[0].FinishReason)
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 10 {
		t.Errorf("input_tokens mismatch")
	}
}

func TestAnthropic_StreamDecoder(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start"}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: message_delta`,
		`data: {"usage":{"input_tokens":5,"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {}`,
		``,
	}, "\n")

	dec := anthropic.NewStreamDecoder(slog.Default())
	session, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
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
	if text != "Hi" {
		t.Errorf("delta %q", text)
	}
	if !done {
		t.Errorf("missing done")
	}
	if usage == nil || usage.PromptTokens == nil || *usage.PromptTokens != 5 {
		t.Errorf("usage %v", usage)
	}
}

func TestAnthropic_ErrorNormalizer(t *testing.T) {
	norm := anthropic.NewSpec(slog.Default()).ErrorNormalizer
	pe := norm.Normalize(429, http.Header{"Retry-After": []string{"10"}}, []byte(`{"error":{"type":"rate_limit_error","message":"slow"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code %q", pe.Code)
	}
	if pe.RetryAfter == nil {
		t.Errorf("RetryAfter missing")
	}
}

func TestAnthropic_Execute_Translate(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			t.Errorf("x-api-key missing: %q", r.Header.Get("x-api-key"))
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer server.Close()

	a := adapter()
	resp, err := a.Execute(context.Background(), provcore.Request{
		WireShape:  typology.WireShapeAnthropicMessages,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`),
		Target: provcore.CallTarget{
			BaseURL:         server.URL,
			APIKey:          "k",
			ProviderModelID: "claude",
		},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	if received["model"] != "claude" {
		t.Errorf("model in received: %v", received["model"])
	}
}
