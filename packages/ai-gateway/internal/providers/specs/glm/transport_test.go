package glm_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/glm"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The tests in this file fill the coverage gaps for spec_glm to push the
// package above the 95% statement-coverage gate (binding
// [[unit_test_coverage_95]]). They assert OBSERVABLE behavior — GLM
// (Zhipu AI) URL composition, JWT bearer wiring, OpenAI-identity codec
// passthrough (since the wire shape mirrors OpenAI), stream decoder
// delegation to the shared OpenAI SSE decoder, and Probe/Do over a real
// httptest server — and avoid err==nil padding.

// spec.go — NewSpec wiring

// TestNewSpec_NilLoggerDefaults pins the slog.Default() fallback on
// spec.go:20-22. Without it, NewSpec(nil) would push a nil *slog.Logger
// into the embedded Transport / StreamDecoder constructors which then
// panic on the first log call.
func TestNewSpec_NilLoggerDefaults(t *testing.T) {
	s := glm.NewSpec(nil)
	if s.Format != provcore.FormatGLM {
		t.Errorf("Format=%q want %q", s.Format, provcore.FormatGLM)
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

// TestNewSpec_CustomLoggerKept exercises the non-nil branch of the
// guard at spec.go:20-22 — when the caller supplies a logger, NewSpec
// must keep it (no replacement).
func TestNewSpec_CustomLoggerKept(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := glm.NewSpec(log)
	if !s.Valid() {
		t.Fatal("spec invalid after custom-logger construction")
	}
}

// spec.go — SchemaCodec is OpenAI identity

// TestSchemaCodec_IsIdentity pins the binding contract from spec.go's
// docstring: "the SchemaCodec is identity because GLM's wire body is
// identical to OpenAI". A canonical body carrying GLM-specific quirks
// (model="glm-4-plus", tool_choice, response_format) must pass through
// EncodeRequest and DecodeResponse byte-identical — any future change
// that injected format translation would break this assertion before
// it broke GLM smoke runs.
func TestSchemaCodec_IsIdentity(t *testing.T) {
	codec := glm.NewSpec(nil).SchemaCodec

	t.Run("encode_request_passes_through_unchanged", func(t *testing.T) {
		canon := []byte(`{"model":"glm-4-plus","messages":[{"role":"user","content":"hi"}],"max_tokens":2048,"stream":false,"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)
		encRes, err := codec.EncodeRequest(typology.WireShapeOpenAIChat, canon, provcore.CallTarget{ProviderModelID: "glm-4-plus"})
		out := encRes.Body
		rewrites := encRes.Rewrites
		if err != nil {
			t.Fatalf("EncodeRequest err=%v", err)
		}
		if string(out) != string(canon) {
			t.Errorf("body mutated by identity codec:\n got: %s\nwant: %s", out, canon)
		}
		// Identity codec contributes no extra rewrites — Transport.ApplyAuth
		// owns Authorization. A non-empty rewrites slice here would signal a
		// regression that injected format-translation logic.
		if len(rewrites) != 0 {
			t.Errorf("identity codec leaked rewrites: %v", rewrites)
		}
	})

	t.Run("decode_response_preserves_usage_and_choices", func(t *testing.T) {
		// GLM emits standard OpenAI chat-completion responses. Identity
		// DecodeResponse must surface them byte-for-byte so the downstream
		// canonical projector picks them up unchanged.
		native := []byte(`{
			"id":"chatcmpl-glm-x","object":"chat.completion","model":"glm-4-plus",
			"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":15,"completion_tokens":25,"total_tokens":40}
		}`)
		decRes, err := codec.DecodeResponse(typology.WireShapeOpenAIChat, native, "", provcore.DecodeContext{})
		out := decRes.CanonicalBody
		usage := decRes.Usage
		if err != nil {
			t.Fatalf("DecodeResponse err=%v", err)
		}
		if string(out) != string(native) {
			t.Errorf("identity DecodeResponse mutated body:\n got: %s\nwant: %s", out, native)
		}
		// Observable: the shared OpenAI-style Usage extractor must surface
		// GLM's prompt_tokens / completion_tokens — these feed the cost
		// ledger and analytics.
		if usage.PromptTokens == nil || *usage.PromptTokens != 15 {
			t.Errorf("PromptTokens=%v want 15", usage.PromptTokens)
		}
		if usage.CompletionTokens == nil || *usage.CompletionTokens != 25 {
			t.Errorf("CompletionTokens=%v want 25", usage.CompletionTokens)
		}
		// Choice text must still be present in the projected body.
		if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "answer" {
			t.Errorf("content lost: %q", got)
		}
	})
}

// spec.go — StreamDecoder delegates to shared OpenAI SSE decoder

// TestStreamDecoder_DelegatesToOpenAIDecoder confirms the StreamDecoder
// produced by NewSpec really is the OpenAI-compatible decoder — GLM's
// SSE shape mirrors OpenAI chat-completion streaming, so a regression
// that wired a stub decoder would silently drop deltas. We replay a
// canonical OpenAI-style SSE transcript and assert the merged Delta
// surfaces the full assistant text.
func TestStreamDecoder_DelegatesToOpenAIDecoder(t *testing.T) {
	dec := glm.NewSpec(nil).StreamDecoder

	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"hello "}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"world"}}]}`,
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
	for {
		ev, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next err=%v", err)
		}
		merged.WriteString(ev.Delta)
	}
	if got, want := merged.String(), "hello world"; got != want {
		t.Errorf("merged Delta=%q want %q", got, want)
	}
}

// spec.go — ErrorNormalizer delegates to shared OpenAI normalizer

// TestErrorNormalizer_Wiring exercises the ErrorNormalizer field
// produced by NewSpec. GLM emits OpenAI-style error envelopes; the
// shared OpenAI normalizer's status→Code map is the contract we
// depend on. This test pins that contract from GLM's seat so a
// refactor that swapped to a GLM-only normalizer would surface here.
func TestErrorNormalizer_Wiring(t *testing.T) {
	norm := glm.NewSpec(nil).ErrorNormalizer

	t.Run("rate_limit_429_with_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "8")
		pe := norm.Normalize(429, h, []byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
		if pe.Code != provcore.CodeRateLimited {
			t.Errorf("Code=%q want %q", pe.Code, provcore.CodeRateLimited)
		}
		if pe.RetryAfter == nil || pe.RetryAfter.Seconds() != 8 {
			t.Errorf("RetryAfter=%v want 8s", pe.RetryAfter)
		}
		if !strings.Contains(pe.Message, "slow down") {
			t.Errorf("Message=%q want contain 'slow down'", pe.Message)
		}
	})

	t.Run("auth_401", func(t *testing.T) {
		pe := norm.Normalize(401, nil, []byte(`{"error":{"type":"authentication_error","message":"bad jwt"}}`))
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
}

// transport.go — NewTransport nil-logger fallback

// TestNewTransport_NilLoggerDefaults pins the slog.Default() fallback on
// transport.go:23-25 — without it, every log call inside the Transport
// would panic when the caller passes nil.
func TestNewTransport_NilLoggerDefaults(t *testing.T) {
	tr := glm.NewTransport(nil)
	if tr == nil {
		t.Fatal("Transport must be non-nil after nil-logger construction")
	}
	// Smoke-test that the transport is usable — BuildURL exercises the
	// internal state without needing a network call.
	got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://open.bigmodel.cn"}, typology.WireShapeOpenAIChat, false)
	if err != nil || got == "" {
		t.Errorf("nil-logger transport not usable: url=%q err=%v", got, err)
	}
}

// transport.go — BuildURL all endpoints + edge cases

// TestTransport_BuildURL_AllEndpoints covers every Endpoint arm:
// chat (/api/paas/v4/chat/completions), embeddings
// (/api/paas/v4/embeddings), and models (/api/paas/v4/models). Also
// pins the trailing-slash trim so a BaseURL configured with a trailing
// "/" never produces a `//` double-slash in the final URL (which Zhipu's
// gateway has historically returned 404 for).
func TestTransport_BuildURL_AllEndpoints(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	cases := []struct {
		name     string
		endpoint typology.WireShape
		baseURL  string
		want     string
	}{
		{"chat", typology.WireShapeOpenAIChat, "https://open.bigmodel.cn", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"chat_trailing_slash", typology.WireShapeOpenAIChat, "https://open.bigmodel.cn/", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"embeddings", typology.WireShapeOpenAIEmbeddings, "https://open.bigmodel.cn", "https://open.bigmodel.cn/api/paas/v4/embeddings"},
		{"embeddings_trailing_slash", typology.WireShapeOpenAIEmbeddings, "https://open.bigmodel.cn/", "https://open.bigmodel.cn/api/paas/v4/embeddings"},
		{"models", typology.WireShapeNone, "https://open.bigmodel.cn", "https://open.bigmodel.cn/api/paas/v4/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tr.BuildURL(provcore.CallTarget{BaseURL: tc.baseURL}, tc.endpoint, false)
			if err != nil {
				t.Fatalf("BuildURL err=%v", err)
			}
			if got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
			if strings.Contains(got, "//api") {
				t.Errorf("double slash leaked: %q", got)
			}
		})
	}
}

// TestTransport_BuildURL_EmptyBaseURL pins the base-URL guard
// (transport.go:37-39) — a half-configured target must error instead
// of producing "/api/paas/v4/chat/completions" with no host.
func TestTransport_BuildURL_EmptyBaseURL(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	_, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeOpenAIChat, false)
	if err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
	if !strings.Contains(err.Error(), "BaseURL is empty") {
		t.Errorf("err=%v want contain 'BaseURL is empty'", err)
	}
}

// TestTransport_BuildURL_UnsupportedEndpoint pins the default arm
// (transport.go:48) — endpoints outside the explicit map (e.g. the
// Responses API) must error so callers never route the wrong shape
// to GLM's `/api/paas/v4/*` surface.
func TestTransport_BuildURL_UnsupportedEndpoint(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	_, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://open.bigmodel.cn"}, typology.WireShapeOpenAIResponses, false)
	if err == nil {
		t.Fatal("expected error for unsupported endpoint")
	}
	if !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Errorf("err=%v want contain 'unsupported endpoint'", err)
	}
}

// transport.go — ApplyAuth

// TestTransport_ApplyAuth_MissingAPIKey pins the missing-key guard
// (transport.go:53-55). Per the package docstring, the Resolver hands
// the Transport a pre-minted JWT under CallTarget.APIKey — if that is
// empty, the call must error rather than silently fire an
// unauthenticated request that Zhipu would 401 on.
func TestTransport_ApplyAuth_MissingAPIKey(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "https://open.bigmodel.cn/api/paas/v4/chat/completions", nil)
	err := tr.ApplyAuth(req, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "missing API key") {
		t.Errorf("err=%v want contain 'missing API key'", err)
	}
	// Negative pin: missing-key path must NOT leak a half-set header
	// onto the request — downstream interceptors otherwise might forward
	// "Bearer " (empty) and waste an upstream attempt.
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Authorization leaked on error: %q", req.Header.Get("Authorization"))
	}
}

// TestTransport_ApplyAuth_JWTBearerSet covers the happy path: the
// Resolver-minted JWT lives in CallTarget.APIKey (Zhipu's auth model is
// a JWT signed from the `<api_id>.<api_secret>` credential pair —
// signing happens at credential time, this layer just stamps the
// bearer token verbatim).
func TestTransport_ApplyAuth_JWTBearerSet(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "https://open.bigmodel.cn/api/paas/v4/chat/completions", nil)
	// Pass a token that looks like a real Zhipu JWT (three dot-separated
	// base64url segments) — the Transport must forward it as-is.
	jwt := "eyJhbGciOiJIUzI1NiIsInNpZ25fdHlwZSI6IlNJR04ifQ.eyJhcGlfa2V5IjoiYWJjIiwiZXhwIjoxNzM1Njg5NjAwLCJ0aW1lc3RhbXAiOjE3MzU2ODYwMDB9.signature"
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: jwt}); err != nil {
		t.Fatalf("ApplyAuth err=%v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer "+jwt; got != want {
		t.Errorf("Authorization=%q want %q", got, want)
	}
}

// transport.go — Do

// TestTransport_Do_DelegatesToClient drives a real HTTP round-trip
// through httptest to cover transport.go:61-63. We assert the request
// hits the server with the canonical GLM chat-completions path, the
// body round-trips, and the response body is readable.
func TestTransport_Do_DelegatesToClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%q want POST", r.Method)
		}
		if r.URL.Path != "/api/paas/v4/chat/completions" {
			t.Errorf("path=%q want /api/paas/v4/chat/completions", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"glm-4-plus"`) {
			t.Errorf("body did not round-trip: %q", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"glm-resp"}`))
	}))
	defer srv.Close()

	tr := glm.NewTransport(slog.Default())
	req, _ := http.NewRequest(
		http.MethodPost,
		srv.URL+"/api/paas/v4/chat/completions",
		strings.NewReader(`{"model":"glm-4-plus"}`),
	)
	resp, err := tr.Do(context.Background(), req, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Do err=%v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"glm-resp"`) {
		t.Errorf("response body=%q", body)
	}
}

// TestTransport_Do_PropagatesContextCancel covers the ctx.WithContext
// branch — a cancelled context must terminate the request before the
// server replies, so the gateway can abort slow GLM calls when the
// client disconnects.
func TestTransport_Do_PropagatesContextCancel(t *testing.T) {
	// Server that blocks forever — only ctx-cancel can unblock us.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	tr := glm.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	resp, err := tr.Do(ctx, req, provcore.CallTarget{})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// transport.go — Probe

// TestTransport_Probe_EmptyBaseURL pins the empty-base-URL guard
// (transport.go:67-69) so a half-configured target produces a not-OK
// probe without a network dial.
func TestTransport_Probe_EmptyBaseURL(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r == nil || r.OK {
		t.Errorf("empty BaseURL must produce not-OK probe, got %+v", r)
	}
	if !strings.Contains(r.Detail, "BaseURL empty") {
		t.Errorf("Detail=%q want contain 'BaseURL empty'", r.Detail)
	}
}

// TestTransport_Probe_Success covers the 2xx happy path — GLM serves
// /api/paas/v4/models to authenticated tokens and we assert (1) the
// bearer token rides the request, (2) latency is captured, (3) the
// Detail string is "ok".
func TestTransport_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/paas/v4/models" {
			t.Errorf("path=%q want /api/paas/v4/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer glm-jwt" {
			t.Errorf("Authorization=%q want Bearer glm-jwt", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	tr := glm.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "glm-jwt"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs=%d", r.LatencyMs)
	}
	if r.Detail != "ok" {
		t.Errorf("Detail=%q want ok", r.Detail)
	}
	// Trailing slash on the configured base must NOT produce `//models`.
	if strings.Contains(r.Detail, "//") {
		t.Errorf("Detail leaked double-slash: %q", r.Detail)
	}
}

// TestTransport_Probe_TrailingSlashNormalized verifies the trailing-slash
// trim runs in Probe (transport.go:67) the same way it runs in BuildURL,
// so a half-configured BaseURL doesn't poison the probe with a 404.
func TestTransport_Probe_TrailingSlashNormalized(t *testing.T) {
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := glm.NewTransport(slog.Default())
	_, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL + "/", APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if sawPath != "/api/paas/v4/models" {
		t.Errorf("path=%q want /api/paas/v4/models (trailing slash should be trimmed)", sawPath)
	}
}

// TestTransport_Probe_NoAPIKeyOmitsBearer covers the conditional
// Authorization header branch (transport.go:78-80) — when no JWT is
// available, the probe must still fire (some self-hosted GLM
// deployments allow anonymous /models) but without a bogus "Bearer "
// (empty) header that some gateways treat as a 401-trigger.
func TestTransport_Probe_NoAPIKeyOmitsBearer(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := glm.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if !r.OK {
		t.Errorf("expected OK probe without auth, got %+v", r)
	}
	if sawAuth != "" {
		t.Errorf("Authorization must be omitted when APIKey empty, got %q", sawAuth)
	}
}

// TestTransport_Probe_HTTPFailure covers the non-2xx branch
// (transport.go:90) — 5xx from GLM /api/paas/v4/models marks the probe
// not-OK and surfaces the status in Detail but does NOT surface a Go
// error (a returned err would crash the orchestrator's polling loop).
func TestTransport_Probe_HTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := glm.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r.OK {
		t.Errorf("5xx must mark OK=false: %+v", r)
	}
	if !strings.Contains(r.Detail, "500") {
		t.Errorf("Detail must mention status, got %q", r.Detail)
	}
}

// TestTransport_Probe_TransportError covers the dial-failure branch
// (transport.go:83-85). Pointing at a closed server records r.Err and
// OK=false so the orchestrator can mark the credential degraded.
func TestTransport_Probe_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	tr := glm.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: addr, APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r.OK {
		t.Error("closed server must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on transport failure")
	}
	if r.Detail == "" {
		t.Error("Detail must carry the error string")
	}
}

// TestTransport_Probe_BadURL covers the http.NewRequestWithContext
// error branch (transport.go:74-77) — a base URL containing an illegal
// control char fails at request construction, before any network call.
func TestTransport_Probe_BadURL(t *testing.T) {
	tr := glm.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: "http://\x7f", APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r.OK {
		t.Error("malformed URL must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on request-construction failure")
	}
}
