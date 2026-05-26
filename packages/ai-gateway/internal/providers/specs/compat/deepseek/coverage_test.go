package deepseek_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/deepseek"
	"github.com/tidwall/gjson"
)

// TestNewSpec_NilLogReplaced covers spec.go:17-19 — the nil-log branch
// must substitute slog.Default rather than panicking on the first log
// call inside the embedded OpenAI Transport / StreamDecoder. Without
// this branch, NewSpec(nil) would push a nil *slog.Logger into both
// downstream constructors, which expect a non-nil value.
func TestNewSpec_NilLogReplaced(t *testing.T) {
	s := deepseek.NewSpec(nil)
	if s.Format != provcore.FormatDeepSeek {
		t.Errorf("Format=%q want %q", s.Format, provcore.FormatDeepSeek)
	}
	if s.Transport == nil {
		t.Error("Transport must be non-nil")
	}
	if s.SchemaCodec == nil {
		t.Error("SchemaCodec must be non-nil")
	}
	if s.StreamDecoder == nil {
		t.Error("StreamDecoder must be non-nil")
	}
	if s.ErrorNormalizer == nil {
		t.Error("ErrorNormalizer must be non-nil")
	}
	if !s.Valid() {
		t.Error("spec must be Valid()")
	}
}

// TestNewSpec_CustomLogKept exercises the non-nil branch of the
// guard at spec.go:17-19 — when the caller supplies a logger,
// NewSpec must keep it (no replacement).
func TestNewSpec_CustomLogKept(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := deepseek.NewSpec(log)
	if !s.Valid() {
		t.Fatalf("spec invalid")
	}
}

// TestSchemaCodec_IsIdentity pins the binding contract from
// spec.go's docstring: "the SchemaCodec is identity because the
// on-the-wire shape already matches OpenAI". A canonical body
// carrying DeepSeek-specific fields (model="deepseek-reasoner",
// reasoning_content delta, prompt_cache_hit_tokens-style fields)
// must pass through EncodeRequest and DecodeResponse byte-identical
// — any future change that injects format translation would break
// this assertion before it breaks DeepSeek smoke runs.
func TestSchemaCodec_IsIdentity(t *testing.T) {
	codec := deepseek.NewSpec(nil).SchemaCodec

	t.Run("encode_request_passes_through_unchanged", func(t *testing.T) {
		canon := []byte(`{"model":"deepseek-reasoner","messages":[{"role":"user","content":"hi"}],"max_tokens":4096,"stream":false}`)
		encRes, err := codec.EncodeRequest(typology.WireShapeOpenAIChat, canon, provcore.CallTarget{ProviderModelID: "deepseek-reasoner"})
		out := encRes.Body
		rewrites := encRes.Rewrites
		if err != nil {
			t.Fatalf("EncodeRequest err=%v", err)
		}
		if string(out) != string(canon) {
			t.Errorf("body mutated by identity codec:\n got: %s\nwant: %s", out, canon)
		}
		// Identity codec contributes no extra rewrites — the Transport
		// layer owns Authorization. A non-empty rewrites slice here would
		// signal a regression that injected format-translation logic.
		if len(rewrites) != 0 {
			t.Errorf("identity codec leaked rewrites: %v", rewrites)
		}
	})

	t.Run("decode_response_preserves_reasoning_content", func(t *testing.T) {
		// DeepSeek's reasoning-model wire emits the assistant's
		// chain-of-thought under choices[0].message.reasoning_content.
		// Identity DecodeResponse must surface it byte-for-byte so the
		// downstream canonical projector can pick it up.
		native := []byte(`{
			"id":"chatcmpl-x","object":"chat.completion","model":"deepseek-reasoner",
			"choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_content":"step 1. step 2."},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_cache_hit_tokens":7}
		}`)
		decRes, err := codec.DecodeResponse(typology.WireShapeOpenAIChat, native, "")
		out := decRes.CanonicalBody
		usage := decRes.Usage
		if err != nil {
			t.Fatalf("DecodeResponse err=%v", err)
		}
		if string(out) != string(native) {
			t.Errorf("identity DecodeResponse mutated body")
		}
		// Observable: the shared OpenAI-style Usage extractor must
		// surface DeepSeek's prompt_cache_hit_tokens via the canonical
		// CacheReadTokens alias (binding for DeepSeek cost analytics).
		if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 7 {
			t.Errorf("CacheReadTokens=%v want 7 (DeepSeek prompt_cache_hit_tokens alias)", usage.CacheReadTokens)
		}
		if usage.PromptTokens == nil || *usage.PromptTokens != 10 {
			t.Errorf("PromptTokens=%v want 10", usage.PromptTokens)
		}
		if usage.CompletionTokens == nil || *usage.CompletionTokens != 20 {
			t.Errorf("CompletionTokens=%v want 20", usage.CompletionTokens)
		}
		// reasoning_content must still be present in the projected body
		// — the canonical projector downstream reads it from here.
		if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "step 1. step 2." {
			t.Errorf("reasoning_content lost: %q", got)
		}
	})
}

// TestTransport_WiringToOpenAISurface confirms the AdapterSpec
// produced by NewSpec really delegates to the OpenAI-compatible
// URL builder for both the supported chat-completions and
// embeddings endpoints. DeepSeek's public API lives under
// `/v1/chat/completions` and `/v1/embeddings`; any change to
// the Transport wiring that broke either path would surface a
// 404 in prod — pin both contracts here.
func TestTransport_WiringToOpenAISurface(t *testing.T) {
	tr := deepseek.NewSpec(nil).Transport

	t.Run("chat_completions_url", func(t *testing.T) {
		got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.deepseek.com", ProviderModelID: "deepseek-reasoner"}, typology.WireShapeOpenAIChat, false)
		if err != nil {
			t.Fatalf("BuildURL err=%v", err)
		}
		if got != "https://api.deepseek.com/v1/chat/completions" {
			t.Errorf("url=%q", got)
		}
	})

	t.Run("trailing_slash_base_normalized", func(t *testing.T) {
		got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.deepseek.com/", ProviderModelID: "deepseek-chat"}, typology.WireShapeOpenAIChat, false)
		if err != nil {
			t.Fatalf("BuildURL err=%v", err)
		}
		if strings.Contains(got, "//v1") {
			t.Errorf("double slash in URL: %q", got)
		}
	})

	t.Run("apply_auth_sets_bearer", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)
		if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "sk-test"}); err != nil {
			t.Fatalf("ApplyAuth err=%v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization=%q want Bearer sk-test", got)
		}
	})

	t.Run("apply_auth_missing_key_errors", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)
		if err := tr.ApplyAuth(req, provcore.CallTarget{}); err == nil {
			t.Fatal("expected error for missing API key")
		}
	})
}

// TestErrorNormalizer_Wiring exercises the ErrorNormalizer field
// produced by NewSpec. DeepSeek emits OpenAI-style error envelopes,
// occasionally with only a `code` field (older deployments) — the
// shared OpenAI normalizer's code-promotion path is the contract
// we depend on. This test pins that contract from DeepSeek's seat
// so a refactor that swapped to a DeepSeek-only normalizer would
// surface here.
func TestErrorNormalizer_Wiring(t *testing.T) {
	norm := deepseek.NewSpec(nil).ErrorNormalizer

	t.Run("rate_limit_429_with_retry_after", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("Retry-After", "12")
		pe := norm.Normalize(429, headers, []byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
		if pe.Code != provcore.CodeRateLimited {
			t.Errorf("Code=%q want %q", pe.Code, provcore.CodeRateLimited)
		}
		if pe.RetryAfter == nil || pe.RetryAfter.Seconds() != 12 {
			t.Errorf("RetryAfter=%v want 12s", pe.RetryAfter)
		}
		if !strings.Contains(pe.Message, "slow down") {
			t.Errorf("Message=%q want contain 'slow down'", pe.Message)
		}
	})

	t.Run("auth_401", func(t *testing.T) {
		pe := norm.Normalize(401, nil, []byte(`{"error":{"type":"authentication_error","message":"bad key"}}`))
		if pe.Code != provcore.CodeAuthFailed {
			t.Errorf("Code=%q want %q", pe.Code, provcore.CodeAuthFailed)
		}
	})

	t.Run("server_500_upstream_error", func(t *testing.T) {
		pe := norm.Normalize(500, nil, []byte(`{"error":{"type":"server_error","message":"oops"}}`))
		if pe.Code != provcore.CodeUpstreamError {
			t.Errorf("Code=%q want %q", pe.Code, provcore.CodeUpstreamError)
		}
	})

	t.Run("code_only_envelope_promotes_to_type", func(t *testing.T) {
		// DeepSeek occasionally emits only `code` (no `type`); the
		// shared OpenAI normalizer must promote it. Without this
		// fallback, the canonical ProviderError.Type would be empty
		// and audit/analytics rows would lose the provider hint.
		pe := norm.Normalize(503, nil, []byte(`{"error":{"code":"deepseek_overload","message":"try later"}}`))
		if pe.Type != "deepseek_overload" {
			t.Errorf("Type=%q want deepseek_overload (code promoted)", pe.Type)
		}
	})
}

// TestStreamDecoder_ReasoningContentWiring confirms the StreamDecoder
// produced by NewSpec really is the OpenAI-compatible decoder that
// knows how to surface DeepSeek's `delta.reasoning_content` frames.
// Per the shared OpenAI chat-completions stream contract
// (spec_openai/stream.go:99-105), reasoning_content is APPENDED to
// the canonical Delta so audit logs capture the full model output;
// a regression that wired a vanilla decoder lacking the
// reasoning_content branch would silently drop chain-of-thought
// from DeepSeek-R1 streams (the Delta would be just "final answer").
func TestStreamDecoder_ReasoningContentWiring(t *testing.T) {
	dec := deepseek.NewSpec(nil).StreamDecoder

	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"thinking step one. "}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"thinking step two. "}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"final answer"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	var merged strings.Builder
	sawReasoningFrame := false
	for {
		ev, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next err=%v", err)
		}
		merged.WriteString(ev.Delta)
		// Pin the per-frame observable: reasoning-only frames produce a
		// non-empty Delta carrying the reasoning text (the decoder
		// promotes reasoning_content into Delta when content is absent).
		if strings.Contains(ev.Delta, "thinking step") {
			sawReasoningFrame = true
		}
	}
	if !sawReasoningFrame {
		t.Error("decoder dropped reasoning_content frames (DeepSeek-R1 regression)")
	}
	want := "thinking step one. thinking step two. final answer"
	if got := merged.String(); got != want {
		t.Errorf("merged Delta=%q want %q (reasoning_content must append, not replace)", got, want)
	}
}
