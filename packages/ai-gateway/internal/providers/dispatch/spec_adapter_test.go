package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Fake components for composing test AdapterSpecs.

type fakeTransport struct {
	buildURL    func(CallTarget, typology.WireShape, bool) (string, error)
	applyAuth   func(*http.Request, CallTarget) error
	do          func(context.Context, *http.Request) (*http.Response, error)
	probe       func(context.Context, CallTarget) (*ProbeResult, error)
	lastReq     *http.Request
	lastReqBody []byte
}

func (f *fakeTransport) BuildURL(t CallTarget, e typology.WireShape, stream bool) (string, error) {
	if f.buildURL != nil {
		return f.buildURL(t, e, stream)
	}
	return "https://upstream.test/v1/chat/completions", nil
}
func (f *fakeTransport) ApplyAuth(r *http.Request, t CallTarget) error {
	if f.applyAuth != nil {
		return f.applyAuth(r, t)
	}
	r.Header.Set("Authorization", "Bearer "+t.APIKey)
	return nil
}
func (f *fakeTransport) Do(ctx context.Context, r *http.Request, _ CallTarget) (*http.Response, error) {
	f.lastReq = r
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		f.lastReqBody = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	if f.do != nil {
		return f.do(ctx, r)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}
func (f *fakeTransport) Probe(ctx context.Context, t CallTarget) (*ProbeResult, error) {
	if f.probe != nil {
		return f.probe(ctx, t)
	}
	return &ProbeResult{OK: true}, nil
}

type fakeCodec struct {
	encodeErr   error
	decodeErr   error
	encoded     []byte
	decoded     []byte
	usage       Usage
	urlOverride string

	encodeCalled bool
	decodeCalled bool
}

func (c *fakeCodec) EncodeRequest(_ typology.WireShape, body []byte, _ CallTarget) (EncodeResult, error) {
	c.encodeCalled = true
	if c.encodeErr != nil {
		return EncodeResult{}, c.encodeErr
	}
	if c.encoded != nil {
		return EncodeResult{Body: c.encoded, ContentType: "application/json", URLOverride: c.urlOverride}, nil
	}
	return EncodeResult{Body: body, ContentType: "application/json", URLOverride: c.urlOverride}, nil
}
func (c *fakeCodec) DecodeResponse(_ typology.WireShape, body []byte, _ string, _ DecodeContext) (DecodeResult, error) {
	c.decodeCalled = true
	if c.decodeErr != nil {
		return DecodeResult{}, c.decodeErr
	}
	if c.decoded != nil {
		return DecodeResult{CanonicalBody: c.decoded, Usage: c.usage}, nil
	}
	return DecodeResult{CanonicalBody: body, Usage: c.usage}, nil
}

type fakeStreamDecoder struct {
	session StreamSession
	err     error
}

func (d *fakeStreamDecoder) Open(body io.ReadCloser, _ typology.WireShape) (StreamSession, error) {
	if body != nil {
		_ = body.Close()
	}
	if d.err != nil {
		return nil, d.err
	}
	if d.session != nil {
		return d.session, nil
	}
	return &emptySession{}, nil
}

type emptySession struct{ closed bool }

func (s *emptySession) Next(_ context.Context) (Chunk, error) {
	if s.closed {
		return Chunk{}, io.EOF
	}
	s.closed = true
	return Chunk{Done: true}, nil
}
func (s *emptySession) Close() error { return nil }

type fakeErrorNormalizer struct {
	override *ProviderError
}

func (n *fakeErrorNormalizer) Normalize(status int, _ http.Header, body []byte) *ProviderError {
	if n.override != nil {
		return n.override
	}
	return &ProviderError{
		Status:  status,
		Code:    CodeUpstreamError,
		Message: "upstream error",
		Raw:     body,
	}
}

func specFrom(t *fakeTransport, c *fakeCodec, s *fakeStreamDecoder, n *fakeErrorNormalizer, f Format) AdapterSpec {
	return AdapterSpec{
		Format:          f,
		Transport:       t,
		SchemaCodec:     c,
		StreamDecoder:   s,
		ErrorNormalizer: n,
	}
}

func TestSpecAdapter_Passthrough(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	codec := &fakeCodec{}
	adapter := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatAnthropic), slog.Default())

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatAnthropic,
		Body:       []byte(`{"model":"claude","messages":[]}`),
		Target:     CallTarget{APIKey: "sk-x"},
	}
	resp, err := adapter.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if codec.encodeCalled {
		t.Errorf("passthrough must not invoke codec.EncodeRequest")
	}
	if !bytes.Equal(tr.lastReqBody, req.Body) {
		t.Errorf("upstream body mismatch: got %q want %q", tr.lastReqBody, req.Body)
	}
	if resp.BodyFormat != FormatAnthropic {
		t.Errorf("expected response BodyFormat anthropic, got %s", resp.BodyFormat)
	}
}

func TestSpecAdapter_TranslateViaCodec(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	codec := &fakeCodec{encoded: []byte(`{"translated":true}`)}
	adapter := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatAnthropic), slog.Default())

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4o"}`),
		Target:     CallTarget{APIKey: "sk-x"},
	}
	if _, err := adapter.Execute(context.Background(), req); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !codec.encodeCalled {
		t.Fatalf("cross-format request must invoke codec.EncodeRequest")
	}
	if !bytes.Equal(tr.lastReqBody, []byte(`{"translated":true}`)) {
		t.Errorf("upstream body not the codec output: %q", tr.lastReqBody)
	}
}

// TestSpecAdapter_NonStream_OversizeBody_SetsTruncated pins the F-0349
// contract: when the upstream non-streaming body exceeds the runtime read
// cap (req.MaxResponseBytes), Execute clamps the bytes AND flags
// Response.Truncated so the handler refuses usage_extraction_status="ok".
func TestSpecAdapter_NonStream_OversizeBody_SetsTruncated(t *testing.T) {
	bigBody := strings.Repeat("a", 4096) // far larger than the 16-byte cap below
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(bigBody)),
			}, nil
		},
	}
	codec := &fakeCodec{} // identity decode, no error on the clamped bytes
	adapter := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	resp, err := adapter.Execute(context.Background(), Request{
		WireShape:        typology.WireShapeOpenAIChat,
		BodyFormat:       FormatOpenAI,
		Body:             []byte(`{"model":"gpt-4o","messages":[]}`),
		Target:           CallTarget{APIKey: "sk-x"},
		MaxResponseBytes: 16,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !resp.Truncated {
		t.Error("Response.Truncated=false want true (oversize body must surface truncation)")
	}
	if len(resp.Body) != 16 {
		t.Errorf("clamped body len=%d want 16 (cap)", len(resp.Body))
	}
}

// Control: a body within the cap must NOT be flagged truncated.
func TestSpecAdapter_NonStream_BodyWithinCap_NotTruncated(t *testing.T) {
	small := `{"ok":true}`
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(small)),
			}, nil
		},
	}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	resp, err := adapter.Execute(context.Background(), Request{
		WireShape:        typology.WireShapeOpenAIChat,
		BodyFormat:       FormatOpenAI,
		Body:             []byte(`{"model":"gpt-4o","messages":[]}`),
		Target:           CallTarget{APIKey: "sk-x"},
		MaxResponseBytes: 1024,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.Truncated {
		t.Error("Response.Truncated=true want false on a body within the cap")
	}
}

func TestSpecAdapter_Passthrough_RewritesModelToProviderModelID(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	codec := &fakeCodec{}
	adapter := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"test"}]}`),
		Target:     CallTarget{APIKey: "sk-x", ProviderModelID: "moonshot-v1-8k"},
	}
	if _, err := adapter.Execute(context.Background(), req); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if codec.encodeCalled {
		t.Fatalf("passthrough must not invoke codec.EncodeRequest")
	}
	var got map[string]any
	if err := json.Unmarshal(tr.lastReqBody, &got); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if got["model"] != "moonshot-v1-8k" {
		t.Fatalf("expected rewritten model moonshot-v1-8k, got %#v", got["model"])
	}
}

// TestSpecAdapter_Passthrough_RewritesModelForAllOpenAIWireShapeFormats
// is the regression guard for the bug where routing OpenAI-shape bodies
// to FormatMoonshot/Mistral/Groq/... left payload["model"] equal to the
// caller's original code (e.g. "claude-opus-4-7") instead of the target
// provider's ProviderModelID. specAdapter.rewritePassthroughModel used
// to whitelist only OpenAI/DeepSeek/GLM and silently dropped through
// for the ten OpenAI-compat re-users that share IdentityCodec; those
// upstreams then 4xx'd with "model not found" and the proxy surfaced
// "all upstream providers failed".
func TestSpecAdapter_Passthrough_RewritesModelForAllOpenAIWireShapeFormats(t *testing.T) {
	formats := []Format{
		FormatOpenAI, FormatDeepSeek, FormatGLM, FormatAzureOpenAI,
		FormatMoonshot, FormatMiniMax, FormatHuggingFace,
		FormatMistral, FormatXai, FormatGroq, FormatPerplexity,
		FormatTogether, FormatFireworks,
	}
	for _, fmtTag := range formats {
		t.Run(string(fmtTag), func(t *testing.T) {
			tr := &fakeTransport{
				do: func(_ context.Context, r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{},
						Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					}, nil
				},
			}
			adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, fmtTag), slog.Default())

			req := Request{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: fmtTag,
				Body:       []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`),
				Target:     CallTarget{APIKey: "sk-x", ProviderModelID: "target-model-id"},
			}
			if _, err := adapter.Execute(context.Background(), req); err != nil {
				t.Fatalf("execute: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(tr.lastReqBody, &got); err != nil {
				t.Fatalf("decode upstream body: %v", err)
			}
			if got["model"] != "target-model-id" {
				t.Fatalf("%s: model not rewritten — got %#v, want target-model-id", fmtTag, got["model"])
			}
		})
	}
}

// TestSpecAdapter_Passthrough_StripsNexusNamespace guards against the
// regression where the gateway-internal `nexus` namespace (canonicalext:
// nexus.dry_run, nexus.ext.<provider>.<key>, etc.) leaks through to the
// upstream provider on the passthrough path (same-shape forward), causing
// the provider to 400 with "Unrecognized request argument supplied: nexus"
// (or equivalent for Anthropic / Gemini) — observed against OpenAI on
// 2026-05-24 after E58-S5 removed the dry-run short-circuit that
// previously masked the leak.
//
// Coverage is per-ingress shape because rewritePassthroughModel has
// distinct exit paths: OpenAI-wire-shape parses+rewrites+marshals the
// payload, non-OpenAI-shape returns verbatim. Both must strip nexus.*.
// The cross-format codec path is intentionally NOT covered here because
// codecs rebuild the wire body from canonical fields and never see the
// passthrough function.
func TestSpecAdapter_Passthrough_StripsNexusNamespace(t *testing.T) {
	cases := []struct {
		name       string
		bodyFormat Format
		body       []byte
	}{
		{
			name:       "openai_wire_shape_parsed_and_rewritten",
			bodyFormat: FormatOpenAI,
			body:       []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"nexus":{"dry_run":true,"ext":{"openai":{"foo":"bar"}}}}`),
		},
		{
			name:       "anthropic_wire_shape_verbatim_passthrough",
			bodyFormat: FormatAnthropic,
			body:       []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}],"nexus":{"ext":{"anthropic":{"topK":42}}}}`),
		},
		{
			name:       "gemini_wire_shape_verbatim_passthrough",
			bodyFormat: FormatGemini,
			body:       []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"nexus":{"dry_run":true}}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{
				do: func(_ context.Context, r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{},
						Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					}, nil
				},
			}
			adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, tc.bodyFormat), slog.Default())
			req := Request{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: tc.bodyFormat,
				Body:       tc.body,
				Target:     CallTarget{APIKey: "sk-x", ProviderModelID: "target-model-id"},
			}
			if _, err := adapter.Execute(context.Background(), req); err != nil {
				t.Fatalf("execute: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(tr.lastReqBody, &got); err != nil {
				t.Fatalf("decode upstream body: %v", err)
			}
			if _, ok := got["nexus"]; ok {
				t.Fatalf("nexus namespace must be stripped before upstream send; got %#v", got)
			}
		})
	}
}

// TestStripNexusNamespace_FastPaths exercises stripNexusNamespace's
// short-circuits to lock the no-op behavior for the common case where the
// client did not include a nexus extension (which is most production
// traffic).
func TestStripNexusNamespace_FastPaths(t *testing.T) {
	t.Run("nil_body", func(t *testing.T) {
		if got := stripNexusNamespace(nil); got != nil {
			t.Fatalf("nil body must short-circuit; got %v", got)
		}
	})
	t.Run("empty_body", func(t *testing.T) {
		if got := stripNexusNamespace([]byte{}); len(got) != 0 {
			t.Fatalf("empty body must return empty; got %v", got)
		}
	})
	t.Run("body_without_nexus", func(t *testing.T) {
		in := []byte(`{"model":"x","messages":[]}`)
		got := stripNexusNamespace(in)
		if !bytes.Equal(in, got) {
			t.Fatalf("body without nexus must be returned identical; got %s", got)
		}
	})
	t.Run("body_with_nexus", func(t *testing.T) {
		in := []byte(`{"model":"x","nexus":{"dry_run":true}}`)
		got := stripNexusNamespace(in)
		if bytes.Contains(got, []byte(`"nexus"`)) {
			t.Fatalf("nexus must be removed; got %s", got)
		}
	})
}

func TestSpecAdapter_Passthrough_InvalidJSONReturnsInvalidRequest(t *testing.T) {
	adapter := NewSpecAdapter(specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4o-mini"`),
		Target:     CallTarget{APIKey: "sk-x", ProviderModelID: "moonshot-v1-8k"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Code != CodeInvalidRequest {
		t.Fatalf("expected invalid_request, got %q", pe.Code)
	}
}

func TestSpecAdapter_Non2xxNormalized(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"slow down"}`)),
			}, nil
		},
	}
	norm := &fakeErrorNormalizer{override: &ProviderError{
		Status:  429,
		Code:    CodeRateLimited,
		Message: "rate limited",
	}}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, norm, FormatOpenAI), slog.Default())

	_, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected provider error")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Code != CodeRateLimited {
		t.Errorf("unexpected code %q", pe.Code)
	}
}

func TestSpecAdapter_TransportErrorSurfacedAsUpstream(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connection refused")
		},
	}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Code != CodeUpstreamError {
		t.Errorf("expected upstream_error, got %q", pe.Code)
	}
}

func TestSpecAdapter_HeaderAllowList(t *testing.T) {
	var captured *http.Request
	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			captured = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		},
	}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer leaky")
	hdr.Set("Cookie", "sid=abc")
	hdr.Set("User-Agent", "nexus-test")
	hdr.Set("X-Nexus-Request-Id", "req-123")

	_, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
		Headers:    hdr,
		Target:     CallTarget{APIKey: "sk-x"},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if captured == nil {
		t.Fatal("transport was not called")
	}
	if got := captured.Header.Get("User-Agent"); got != "nexus-test" {
		t.Errorf("User-Agent not forwarded, got %q", got)
	}
	if got := captured.Header.Get("Cookie"); got != "" {
		t.Errorf("Cookie must be stripped, got %q", got)
	}
	if got := captured.Header.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id must be stripped, got %q", got)
	}
	// Authorization must be whatever ApplyAuth wrote, not the ingress value.
	if got := captured.Header.Get("Authorization"); got != "Bearer sk-x" {
		t.Errorf("Authorization not re-issued by ApplyAuth: %q", got)
	}
}

// The OpenAI reasoning-rewrite and Moonshot fixed-temp-rewrite tests
// moved alongside their helpers per Rule 3 — see
// spec_openai/rewrites_test.go and spec_moonshot/rewrites_test.go.

// TestSpecAdapter_ReasoningModel_ResponseCoerced verifies that when a
// passthrough request targets a gpt-5 reasoning model and includes
// max_tokens, the returned Response.Coerced contains the rewrite descriptor
// and the upstream body has max_completion_tokens instead of max_tokens.
// The test wires a minimal inline PassthroughRewrite that mirrors what
// openai.ApplyReasoningRewrites does, so this test exercises the
// SpecAdapter passthrough machinery without an import cycle into
// spec_openai (which itself imports providers).
func TestSpecAdapter_ReasoningModel_ResponseCoerced(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			if r.Body != nil {
				capturedBody, _ = io.ReadAll(r.Body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	codec := &fakeCodec{}
	spec := specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI)
	spec.PassthroughRewrite = func(payload map[string]any, modelID string) []string {
		if modelID != "gpt-5" {
			return nil
		}
		var rewrites []string
		if v, ok := payload["max_tokens"]; ok {
			payload["max_completion_tokens"] = v
			delete(payload, "max_tokens")
			rewrites = append(rewrites, "max_tokens→max_completion_tokens")
		}
		return rewrites
	}
	adapter := NewSpecAdapter(spec, slog.Default())

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_tokens":50}`),
		Target:     CallTarget{APIKey: "sk-x", ProviderModelID: "gpt-5"},
	}
	resp, err := adapter.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(resp.Coerced) != 1 || resp.Coerced[0] != "max_tokens→max_completion_tokens" {
		t.Errorf("Coerced: got %v, want [\"max_tokens→max_completion_tokens\"]", resp.Coerced)
	}
	// Verify the upstream body was actually rewritten.
	var sent map[string]any
	if err := json.Unmarshal(capturedBody, &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if _, ok := sent["max_tokens"]; ok {
		t.Error("max_tokens must be absent in the upstream body for reasoning model")
	}
	if v, ok := sent["max_completion_tokens"]; !ok || v != float64(50) {
		t.Errorf("max_completion_tokens missing or wrong in upstream body: %v", sent)
	}
}

// TestSpecAdapter_NonReasoningModel_CoercedEmpty verifies that a non-reasoning
// model passthrough produces an empty Response.Coerced slice.
func TestSpecAdapter_NonReasoningModel_CoercedEmpty(t *testing.T) {
	t.Parallel()

	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	codec := &fakeCodec{}
	adapter := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`),
		Target:     CallTarget{APIKey: "sk-x", ProviderModelID: "gpt-4o"},
	}
	resp, err := adapter.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(resp.Coerced) != 0 {
		t.Errorf("Coerced: got %v, want empty for non-reasoning model", resp.Coerced)
	}
}

// TestSpecAdapter_PrepareBody_ExposedAndIdempotent verifies that PrepareBody is
// a public method on the Adapter interface, is idempotent (calling it twice on
// the same Request produces byte-equal output), and that it rewrites the model
// field to req.Target.ProviderModelID.
func TestSpecAdapter_PrepareBody_ExposedAndIdempotent(t *testing.T) {
	t.Parallel()

	codec := &fakeCodec{}
	adapter := NewSpecAdapter(
		specFrom(&fakeTransport{}, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI),
		slog.Default(),
	)

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
		Stream:     true,
		Target:     CallTarget{ProviderModelID: "gpt-4o-2024-08-06"},
	}

	body1, rw1, _, err := adapter.PrepareBody(req)
	if err != nil {
		t.Fatalf("PrepareBody error: %v", err)
	}
	body2, rw2, _, err := adapter.PrepareBody(req)
	if err != nil {
		t.Fatalf("PrepareBody error (2nd): %v", err)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatalf("PrepareBody not idempotent: %s != %s", body1, body2)
	}
	if !reflect.DeepEqual(rw1, rw2) {
		t.Fatalf("PrepareBody rewrites not stable: %v vs %v", rw1, rw2)
	}
	if !bytes.Contains(body1, []byte(`"gpt-4o-2024-08-06"`)) {
		t.Fatalf("expected ProviderModelID in prepared body, got: %s", body1)
	}
}

// TestSpecAdapter_PrepareBody_CodecPathIdempotent verifies that PrepareBody
// is idempotent on the codec path: when the client body is OpenAI-format
// but the adapter spec format is Anthropic, PrepareBody takes the
// SchemaCodec.EncodeRequest branch, and calling it twice on the same
// Request produces byte-equal output and equal rewrites. It also asserts
// that the codec is invoked exactly twice — once per PrepareBody call —
// proving PrepareBody does not cache results.
func TestSpecAdapter_PrepareBody_CodecPathIdempotent(t *testing.T) {
	t.Parallel()

	// countingCodec wraps fakeCodec and counts EncodeRequest invocations.
	type countingCodec struct {
		*fakeCodec
		calls int
	}
	cc := &countingCodec{
		fakeCodec: &fakeCodec{encoded: []byte(`{"anthropic":"body","model":"claude-3-5-sonnet-20240620"}`)},
	}
	// Build a spec whose Format is Anthropic but the request carries
	// FormatOpenAI, forcing the codec branch in PrepareBody.
	spec := AdapterSpec{
		Format:    FormatAnthropic,
		Transport: &fakeTransport{},
		SchemaCodec: schemaCodecFunc{
			encFn: func(ep typology.WireShape, body []byte, ct CallTarget) ([]byte, error) {
				cc.calls++
				res, err := cc.EncodeRequest(ep, body, ct)
				return res.Body, err
			},
			decFn: func(ep typology.WireShape, body []byte, contentType string) (DecodeResult, error) {
				return cc.DecodeResponse(ep, body, contentType, DecodeContext{})
			},
		},
		StreamDecoder:   &fakeStreamDecoder{},
		ErrorNormalizer: &fakeErrorNormalizer{},
	}
	adapter := NewSpecAdapter(spec, slog.Default())

	req := Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
		Stream:     true,
		Target:     CallTarget{ProviderModelID: "claude-3-5-sonnet-20240620"},
	}

	body1, rw1, _, err := adapter.PrepareBody(req)
	if err != nil {
		t.Fatalf("PrepareBody (1st) error: %v", err)
	}
	body2, rw2, _, err := adapter.PrepareBody(req)
	if err != nil {
		t.Fatalf("PrepareBody (2nd) error: %v", err)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatalf("PrepareBody codec path not idempotent: %s != %s", body1, body2)
	}
	if !reflect.DeepEqual(rw1, rw2) {
		t.Fatalf("PrepareBody codec rewrites not stable: %v vs %v", rw1, rw2)
	}
	if cc.calls != 2 {
		t.Fatalf("expected codec called exactly twice (no caching), got %d", cc.calls)
	}
}

// schemaCodecFunc adapts two plain functions into the SchemaCodec interface
// for use in test AdapterSpecs without needing a real provider codec.
type schemaCodecFunc struct {
	encFn func(typology.WireShape, []byte, CallTarget) ([]byte, error)
	decFn func(typology.WireShape, []byte, string) (DecodeResult, error)
}

func (s schemaCodecFunc) EncodeRequest(ep typology.WireShape, body []byte, ct CallTarget) (EncodeResult, error) {
	out, err := s.encFn(ep, body, ct)
	return EncodeResult{Body: out, ContentType: "application/json"}, err
}
func (s schemaCodecFunc) DecodeResponse(ep typology.WireShape, body []byte, ct string, _ DecodeContext) (DecodeResult, error) {
	return s.decFn(ep, body, ct)
}

// TestSpecAdapter_PrepareBody_EndpointModelsReturnsNil verifies that
// PrepareBody returns (nil, nil, nil) for the typology.WireShapeNone path, which
// uses GET with no request body.
func TestSpecAdapter_PrepareBody_EndpointModelsReturnsNil(t *testing.T) {
	t.Parallel()

	codec := &fakeCodec{}
	adapter := NewSpecAdapter(
		specFrom(&fakeTransport{}, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI),
		slog.Default(),
	)
	req := Request{
		WireShape:  typology.WireShapeNone,
		BodyFormat: FormatOpenAI,
		Body:       nil,
		Target:     CallTarget{},
	}
	body, rewrites, _, err := adapter.PrepareBody(req)
	if err != nil {
		t.Fatalf("PrepareBody typology.WireShapeNone error: %v", err)
	}
	if body != nil {
		t.Fatalf("expected nil body for typology.WireShapeNone, got %s", body)
	}
	if rewrites != nil {
		t.Fatalf("expected nil rewrites for typology.WireShapeNone, got %v", rewrites)
	}
}

func TestSpecAdapter_StreamOpened(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			}, nil
		},
	}
	adapter := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	resp, err := adapter.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"stream":true}`),
		Stream:     true,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.Stream == nil {
		t.Fatal("expected non-nil stream session")
	}
	chunk, err := resp.Stream.Next(context.Background())
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !chunk.Done {
		t.Errorf("expected terminal chunk")
	}
	_ = resp.Stream.Close()
}
