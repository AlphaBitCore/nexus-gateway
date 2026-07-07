// Tests for the providers dispatch package: registry helpers, the metrics
// shim atomic callbacks, spec_adapter error / debug / GET / stream-error
// paths, PrepareBody non-rewrite paths, FilterResponseHeaders empty-source
// short-circuit, and ExtractUsage edge cases (empty / unknown format /
// Usage-extraction nil).
//
// Tests assert observable behavior: registry contents, captured upstream
// method/headers, surfaced ProviderError codes, slog DEBUG output, coerced
// rewrites, and Usage projection.

package dispatch

import (
	"bytes"
	"context"
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// adapter_registry.go: MustRegister + List + nil-format panic surface.

// TestRegistry_MustRegister_PanicsOnDuplicate pins that MustRegister
// promotes a Register error (invalid format / duplicate) into a panic so
// startup wiring fails loudly instead of silently dropping an adapter.
func TestRegistry_MustRegister_PanicsOnDuplicate(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubOpenAIAdapter{})

	defer func() {
		if recover() == nil {
			t.Fatalf("MustRegister: expected panic on duplicate registration")
		}
	}()
	r.MustRegister(stubOpenAIAdapter{})
}

// TestRegistry_MustRegister_SucceedsForNewFormat pins the happy path —
// registration completes without panic and Get returns the adapter.
func TestRegistry_MustRegister_SucceedsForNewFormat(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubOpenAIAdapter{})
	got, ok := r.Get(FormatOpenAI)
	if !ok || got == nil {
		t.Fatalf("MustRegister: adapter not retrievable after registration")
	}
}

// TestRegistry_List_ReturnsRegisteredInStableOrder verifies List walks
// AllFormats() and emits registered formats in that order. Two
// registrations in reverse order must still surface in AllFormats order.
func TestRegistry_List_ReturnsRegisteredInStableOrder(t *testing.T) {
	r := NewRegistry()
	// Register out of canonical order to prove List() doesn't return insertion order.
	r.MustRegister(formatStubAdapter{f: FormatAnthropic})
	r.MustRegister(formatStubAdapter{f: FormatOpenAI})

	got := r.List()
	if len(got) != 2 {
		t.Fatalf("List: want 2 formats, got %d (%v)", len(got), got)
	}
	// AllFormats has openai before anthropic.
	if got[0] != FormatOpenAI || got[1] != FormatAnthropic {
		t.Errorf("List: order not aligned with AllFormats(): %v", got)
	}
}

// TestRegistry_List_EmptyWhenNothingRegistered.
func TestRegistry_List_EmptyWhenNothingRegistered(t *testing.T) {
	r := NewRegistry()
	if got := r.List(); len(got) != 0 {
		t.Fatalf("empty registry List: want [], got %v", got)
	}
}

// stubOpenAIAdapter is a fixed-format stub used by registry tests.
type stubOpenAIAdapter struct{}

func (stubOpenAIAdapter) Format() Format { return FormatOpenAI }
func (stubOpenAIAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}
func (stubOpenAIAdapter) Execute(context.Context, Request) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}
func (stubOpenAIAdapter) Probe(context.Context, CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}
func (stubOpenAIAdapter) PrepareBody(r Request) ([]byte, []string, string, error) {
	return r.Body, nil, "", nil
}
func (stubOpenAIAdapter) ExecuteWithBody(context.Context, Request, []byte, []string, string) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}

// formatStubAdapter is a parameterized stub Adapter for registry tests.
type formatStubAdapter struct{ f Format }

func (s formatStubAdapter) Format() Format { return s.f }
func (s formatStubAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}
func (s formatStubAdapter) Execute(context.Context, Request) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}
func (s formatStubAdapter) Probe(context.Context, CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}
func (s formatStubAdapter) PrepareBody(r Request) ([]byte, []string, string, error) {
	return r.Body, nil, "", nil
}
func (s formatStubAdapter) ExecuteWithBody(context.Context, Request, []byte, []string, string) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}

// metrics_shim.go: SetForwardHeaderDropFn / SetReasoningPassthroughFn +
// EmitReasoningPassthrough emit paths.

// TestSetForwardHeaderDropFn_EmitInvokesCallback asserts the atomic
// pointer is wired so emitForwardHeaderDrop reaches the registered fn.
func TestSetForwardHeaderDropFn_EmitInvokesCallback(t *testing.T) {
	// Reset to a known state and restore at end.
	prev := forwardHeaderDropFn.Load()
	t.Cleanup(func() { forwardHeaderDropFn.Store(prev) })

	var got struct {
		direction, adapterType, header string
	}
	var calls atomic.Int32
	SetForwardHeaderDropFn(func(direction, adapterType, header string) {
		got.direction = direction
		got.adapterType = adapterType
		got.header = header
		calls.Add(1)
	})
	emitForwardHeaderDrop("request", "openai", "cookie")

	if calls.Load() != 1 {
		t.Fatalf("callback not invoked exactly once: %d", calls.Load())
	}
	if got.direction != "request" || got.adapterType != "openai" || got.header != "cookie" {
		t.Errorf("callback args wrong: %+v", got)
	}
}

// TestSetForwardHeaderDropFn_NilResetsToNoop asserts that passing nil
// clears the active callback (resets to the no-op default) so a test
// teardown can leave global state clean.
func TestSetForwardHeaderDropFn_NilResetsToNoop(t *testing.T) {
	prev := forwardHeaderDropFn.Load()
	t.Cleanup(func() { forwardHeaderDropFn.Store(prev) })

	var calls atomic.Int32
	SetForwardHeaderDropFn(func(string, string, string) { calls.Add(1) })
	SetForwardHeaderDropFn(nil)
	emitForwardHeaderDrop("response", "anthropic", "set-cookie")
	if calls.Load() != 0 {
		t.Fatalf("after SetForwardHeaderDropFn(nil), emit must NOT call the previous callback (calls=%d)", calls.Load())
	}
	if got := forwardHeaderDropFn.Load(); got != nil {
		t.Errorf("after nil-reset, stored pointer must be nil; got %p", got)
	}
}

// TestSetReasoningPassthroughFn_EmitInvokesCallback pins the same
// wiring for the reasoning-passthrough counter.
func TestSetReasoningPassthroughFn_EmitInvokesCallback(t *testing.T) {
	prev := reasoningPassthroughFn.Load()
	t.Cleanup(func() { reasoningPassthroughFn.Store(prev) })

	var captured struct {
		provider, action string
	}
	var calls atomic.Int32
	SetReasoningPassthroughFn(func(provider, action string) {
		captured.provider = provider
		captured.action = action
		calls.Add(1)
	})

	EmitReasoningPassthrough("anthropic", "injected")
	if calls.Load() != 1 {
		t.Fatalf("EmitReasoningPassthrough not invoked exactly once: %d", calls.Load())
	}
	if captured.provider != "anthropic" || captured.action != "injected" {
		t.Errorf("callback args wrong: %+v", captured)
	}
}

// TestSetReasoningPassthroughFn_NilResetsToNoop covers the nil-store branch.
func TestSetReasoningPassthroughFn_NilResetsToNoop(t *testing.T) {
	prev := reasoningPassthroughFn.Load()
	t.Cleanup(func() { reasoningPassthroughFn.Store(prev) })

	SetReasoningPassthroughFn(func(string, string) {})
	SetReasoningPassthroughFn(nil)
	// EmitReasoningPassthrough must not panic and must do nothing.
	EmitReasoningPassthrough("gemini", "skipped_malformed")
	if got := reasoningPassthroughFn.Load(); got != nil {
		t.Errorf("after nil-reset, stored pointer must be nil; got %p", got)
	}
}

// TestEmitReasoningPassthrough_NoOpWhenUnset documents that the no-op
// default path (callback never set) is silent and safe.
func TestEmitReasoningPassthrough_NoOpWhenUnset(t *testing.T) {
	prev := reasoningPassthroughFn.Load()
	t.Cleanup(func() { reasoningPassthroughFn.Store(prev) })
	reasoningPassthroughFn.Store(nil)
	EmitReasoningPassthrough("openai", "injected")
}

// spec_adapter.go: panic on invalid spec, nil-logger fallback, Format() /
// SupportsShape() trivial accessors.

// TestNewSpecAdapterWithAllowlist_PanicsOnInvalidSpec asserts that wiring
// a structurally invalid AdapterSpec aborts at startup. This is the
// "Registry.RegisterBuiltins safety net" — programmer errors do not get
// to silently land in the registry.
func TestNewSpecAdapterWithAllowlist_PanicsOnInvalidSpec(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic for invalid AdapterSpec (missing components)")
		}
	}()
	// All component fields nil → Valid() == false → panic.
	NewSpecAdapterWithAllowlist(AdapterSpec{Format: FormatOpenAI}, nil, slog.Default())
}

// TestNewSpecAdapter_NilLogger_FallsBackToDefault confirms the
// log==nil branch reuses slog.Default() so adapters built without a
// logger (some tests / probes) don't NPE on first debug call.
func TestNewSpecAdapter_NilLogger_FallsBackToDefault(t *testing.T) {
	ad := NewSpecAdapter(specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), nil)
	if ad == nil {
		t.Fatal("adapter is nil after nil-log construction")
	}
	if ad.Format() != FormatOpenAI {
		t.Errorf("Format(): want openai, got %s", ad.Format())
	}
	if !ad.SupportsShape(typology.WireShapeOpenAIChat) {
		t.Error("SupportsShape default chat-completions: want true")
	}
	if ad.SupportsShape(typology.WireShapeOpenAIResponses) {
		t.Error("SupportsShape responses-api on default-spec adapter: want false")
	}
}

// spec_adapter.go: effectiveAllowlist branches — Active() set vs
// construction-time allowlist vs Default() fallback.

// TestEffectiveAllowlist_LiveActiveWins asserts that forwardheader.SetActive
// supersedes a construction-time allowlist (hot-swap path).
func TestEffectiveAllowlist_LiveActiveWins(t *testing.T) {
	// Snapshot and restore the global so test isolation holds.
	prev := forwardheader.Active()
	t.Cleanup(func() { forwardheader.SetActive(prev) })

	// Construction-time allowlist (will be overridden).
	constructionList := forwardheader.Default()
	live := forwardheader.Default()
	forwardheader.SetActive(live)

	a := &specAdapter{
		spec:      AdapterSpec{Format: FormatOpenAI},
		allowlist: constructionList,
	}
	got := a.effectiveAllowlist()
	if got != live {
		t.Errorf("effectiveAllowlist: live Active() must win; got %p, want %p", got, live)
	}
}

// TestEffectiveAllowlist_ConstructionAllowlistWhenActiveNil asserts the
// middle branch — when forwardheader.Active() is nil, the construction-time
// pointer is returned.
func TestEffectiveAllowlist_ConstructionAllowlistWhenActiveNil(t *testing.T) {
	prev := forwardheader.Active()
	t.Cleanup(func() { forwardheader.SetActive(prev) })
	forwardheader.SetActive(nil)

	mine := forwardheader.Default()
	a := &specAdapter{spec: AdapterSpec{Format: FormatOpenAI}, allowlist: mine}
	got := a.effectiveAllowlist()
	if got != mine {
		t.Errorf("effectiveAllowlist: construction allowlist must win when Active()=nil; got %p, want %p", got, mine)
	}
}

// TestEffectiveAllowlist_DefaultFallback asserts the bottom branch —
// no Active(), no construction-time allowlist → embedded Default().
func TestEffectiveAllowlist_DefaultFallback(t *testing.T) {
	prev := forwardheader.Active()
	t.Cleanup(func() { forwardheader.SetActive(prev) })
	forwardheader.SetActive(nil)

	a := &specAdapter{spec: AdapterSpec{Format: FormatOpenAI}}
	if got := a.effectiveAllowlist(); got == nil {
		t.Fatal("effectiveAllowlist: bottom fallback returned nil")
	}
}

// spec_adapter.go: ExecuteWithBody error / GET / debug-log / non-2xx
// nil-normalizer / timeout / read-body-error / decode-error / stream-open
// error paths.

// TestExecuteWithBody_BuildURLError surfaces a ProviderError with
// StatusInternalServerError + CodeInvalidRequest when the Transport
// can't compose a URL.
func TestExecuteWithBody_BuildURLError(t *testing.T) {
	tr := &fakeTransport{
		buildURL: func(CallTarget, typology.WireShape, bool) (string, error) {
			return "", errors.New("no base url")
		},
	}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Status != http.StatusInternalServerError || pe.Code != CodeInvalidRequest {
		t.Errorf("BuildURL error: status=%d code=%q (want 500 / invalid_request)", pe.Status, pe.Code)
	}
}

// TestExecuteWithBody_ModelsUsesGET asserts the typology.WireShapeNone short
// circuit produces a GET, never sends a body, and surfaces the upstream
// response intact.
func TestExecuteWithBody_ModelsUsesGET(t *testing.T) {
	var capturedMethod string
	var capturedBody int
	tr := &fakeTransport{
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			capturedMethod = r.Method
			if r.Body != nil {
				b, _ := io.ReadAll(r.Body)
				capturedBody = len(b)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			}, nil
		},
	}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeNone,
		BodyFormat: FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("Execute(typology.WireShapeNone): %v", err)
	}
	if capturedMethod != http.MethodGet {
		t.Errorf("typology.WireShapeNone must dispatch GET, got %q", capturedMethod)
	}
	if capturedBody != 0 {
		t.Errorf("typology.WireShapeNone must not send a body, got %d bytes", capturedBody)
	}
}

// TestExecuteWithBody_ApplyAuthError surfaces 401 / auth_failed.
func TestExecuteWithBody_ApplyAuthError(t *testing.T) {
	tr := &fakeTransport{
		applyAuth: func(*http.Request, CallTarget) error { return errors.New("missing key") },
	}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Status != http.StatusUnauthorized || pe.Code != CodeAuthFailed {
		t.Errorf("ApplyAuth error: status=%d code=%q (want 401 / auth_failed)", pe.Status, pe.Code)
	}
}

// TestExecuteWithBody_CanceledCtx_SurfacesTimeout proves the ctx.Err()
// branch wins over the generic upstream-error path when the caller has
// already canceled.
func TestExecuteWithBody_CanceledCtx_SurfacesTimeout(t *testing.T) {
	tr := &fakeTransport{
		do: func(ctx context.Context, _ *http.Request) (*http.Response, error) {
			return nil, ctx.Err()
		},
	}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ad.Execute(ctx, Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Code != CodeTimeout || pe.Status != http.StatusGatewayTimeout {
		t.Errorf("canceled-ctx must surface timeout: status=%d code=%q", pe.Status, pe.Code)
	}
}

// TestExecuteWithBody_NormalizerNilFallback covers the path where the
// adapter's ErrorNormalizer returns nil on a 4xx — the dispatcher must
// synthesize a default ProviderError so callers always see a non-nil err.
func TestExecuteWithBody_NormalizerNilFallback(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTeapot,
				Header:     http.Header{"X-Request-Id": []string{"req-x"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"teapot"}`)),
			}, nil
		},
	}
	// Normalizer returns nil — exercises the synthesized fallback in
	// the dispatcher. Use forceNilNormalizer (declared below) so the
	// test does not need to mutate fakeErrorNormalizer's shape.
	spec := AdapterSpec{
		Format:          FormatOpenAI,
		Transport:       tr,
		SchemaCodec:     &fakeCodec{},
		StreamDecoder:   &fakeStreamDecoder{},
		ErrorNormalizer: forceNilNormalizer{},
	}
	ad := NewSpecAdapter(spec, slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Status != http.StatusTeapot || pe.Code != CodeUpstreamError {
		t.Errorf("nil-normalizer fallback: status=%d code=%q (want 418 / upstream_error)", pe.Status, pe.Code)
	}
	// Headers must be cloned onto the error envelope even on the synthesized path.
	if pe.Headers.Get("X-Request-Id") != "req-x" {
		t.Errorf("synthesized error must clone upstream headers; got %v", pe.Headers)
	}
	if pe.TargetMethod != http.MethodPost {
		t.Errorf("TargetMethod: want POST, got %q", pe.TargetMethod)
	}
}

// TestExecuteWithBody_DecodeResponseError surfaces upstream_error with
// the upstream native body attached as Raw.
func TestExecuteWithBody_DecodeResponseError(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"native":1}`)),
			}, nil
		},
	}
	codec := &fakeCodec{decodeErr: errors.New("bad shape")}
	ad := NewSpecAdapter(specFrom(tr, codec, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Status != http.StatusBadGateway || pe.Code != CodeUpstreamError {
		t.Errorf("DecodeResponse error: status=%d code=%q (want 502 / upstream_error)", pe.Status, pe.Code)
	}
	if !bytes.Contains(pe.Raw, []byte(`"native"`)) {
		t.Errorf("Raw must carry the upstream body; got %q", pe.Raw)
	}
}

// TestExecuteWithBody_StreamOpenError translates a StreamDecoder.Open
// failure into upstream_error and closes the upstream body.
func TestExecuteWithBody_StreamOpenError(t *testing.T) {
	var closedBody bool
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &closeNotifyBody{onClose: func() { closedBody = true }},
			}, nil
		},
	}
	sd := &fakeStreamDecoder{err: errors.New("bad sse")}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, sd, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())

	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
		Stream:     true,
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Code != CodeUpstreamError {
		t.Errorf("stream-open error must surface upstream_error, got %q", pe.Code)
	}
	if !closedBody {
		t.Error("stream-open error must close the upstream body to avoid a leak")
	}
}

// closeNotifyBody is an io.ReadCloser whose Close fires a side-effect.
type closeNotifyBody struct {
	onClose func()
}

func (b *closeNotifyBody) Read(p []byte) (int, error) { return 0, io.EOF }
func (b *closeNotifyBody) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}

// TestExecuteWithBody_PhaseSinkBreakdownStamped verifies the
// resp_adapter_ms breakdown entry lands when a PhaseSink is attached
// to the context. PhaseSink.AddBreakdown drops zero/negative ms, so we
// wire a SchemaCodec that sleeps a few ms to guarantee the entry
// surfaces — the goal is to exercise the code path that calls
// traffic.PhaseSinkFromContext + AddBreakdown, not the timing itself.
func TestExecuteWithBody_PhaseSinkBreakdownStamped(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		},
	}
	slowSpec := AdapterSpec{
		Format:          FormatOpenAI,
		Transport:       tr,
		SchemaCodec:     &sleepyCodec{sleep: 2 * time.Millisecond},
		StreamDecoder:   &fakeStreamDecoder{},
		ErrorNormalizer: &fakeErrorNormalizer{},
	}
	ad := NewSpecAdapter(slowSpec, slog.Default())

	ps := traffic.NewPhaseSink()
	ctx := traffic.WithPhaseSink(context.Background(), ps)
	_, err := ad.Execute(ctx, Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	bd := ps.Breakdown()
	if v, ok := bd[string(traffic.PhaseRespAdapter)]; !ok || v <= 0 {
		t.Errorf("PhaseSink missing resp_adapter_ms entry: %v", bd)
	}
}

// sleepyCodec delays DecodeResponse so the PhaseSink resp_adapter_ms
// branch produces a positive ms entry (PhaseSink.AddBreakdown drops <= 0).
type sleepyCodec struct {
	sleep time.Duration
}

func (s *sleepyCodec) EncodeRequest(_ typology.WireShape, body []byte, _ CallTarget) (EncodeResult, error) {
	return EncodeResult{Body: body, ContentType: "application/json"}, nil
}
func (s *sleepyCodec) DecodeResponse(_ typology.WireShape, body []byte, _ string, _ DecodeContext) (DecodeResult, error) {
	time.Sleep(s.sleep)
	return DecodeResult{CanonicalBody: body}, nil
}

// TestExecuteWithBody_DebugLog_FiresUpstreamRequestRecord captures the
// debug LogAttrs branch by setting a slog handler at LevelDebug and
// asserting "upstream request body" + "upstream response headers" lines
// are emitted.
func TestExecuteWithBody_DebugLog_FiresUpstreamRequestRecord(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	var sink syncBuffer
	log := slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), log)

	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4o"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := sink.String()
	if !strings.Contains(got, "upstream request body") {
		t.Errorf("debug log missing 'upstream request body' line: %s", got)
	}
	if !strings.Contains(got, "upstream response headers") {
		t.Errorf("debug log missing 'upstream response headers' line: %s", got)
	}
}

// TestExecuteWithBody_DebugLog_TruncatesLargeBody asserts the
// debugBodyLimit truncation on the request log path (body > 8 KiB).
func TestExecuteWithBody_DebugLog_TruncatesLargeBody(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}
	var sink syncBuffer
	log := slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), log)

	body := bytes.Repeat([]byte("x"), debugBodyLimit+1024)
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       body,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// We can't easily compare the entire log; but the truncated body
	// shouldn't include the trailing chunk past debugBodyLimit. As a
	// proxy, the captured log must NOT contain a sequence longer than
	// debugBodyLimit. Rough check: count x's emitted.
	count := strings.Count(sink.String(), "x")
	if count > debugBodyLimit+200 { // 200 = slack for other log content
		t.Errorf("debug log body not truncated to %d bytes (got ~%d x's)", debugBodyLimit, count)
	}
}

// TestExecuteWithBody_StreamDebugBodyWraps verifies that DEBUG-level
// stream paths wrap the upstream body so the debug snapshot prints on
// Close (covers debugBody.Read + Close + capped branches).
func TestExecuteWithBody_StreamDebugBodyWraps(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: hello\n\ndata: world\n\n")),
			}, nil
		},
	}
	var sink syncBuffer
	log := slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), log)

	resp, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
		Stream:     true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Stream == nil {
		t.Fatal("expected stream session")
	}
	// drain the stream to force Read on the wrapped body.
	for {
		c, err := resp.Stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream Next: %v", err)
		}
		_ = c
	}
	_ = resp.Stream.Close()
	if !strings.Contains(sink.String(), "upstream stream body") {
		t.Errorf("debug stream body log not emitted: %s", sink.String())
	}
}

// TestDebugBody_CappedTruncation drives debugBody directly to cover the
// remaining > 0 → take > remaining → capped=true branch. We pass a
// single read buffer larger than debugBodyLimit so the n > remaining
// guard fires within one Read call.
func TestDebugBody_CappedTruncation(t *testing.T) {
	var sink syncBuffer
	log := slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	inner := io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), debugBodyLimit+512)))
	d := newDebugBody(inner, log, context.Background(), "openai")
	// Single read sized > debugBodyLimit forces n > remaining, hitting the
	// cap branch on the first Read. Subsequent reads drain the tail.
	buf := make([]byte, debugBodyLimit+1024)
	if _, err := d.Read(buf); err != nil {
		t.Fatalf("debugBody.Read: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("debugBody.Close: %v", err)
	}
	if !strings.Contains(sink.String(), "(truncated)") {
		t.Errorf("expected truncation marker in log: %s", sink.String())
	}
}

// TestDebugBody_EmptyStreamLogged drives the buf-empty branch of Close.
func TestDebugBody_EmptyStreamLogged(t *testing.T) {
	var sink syncBuffer
	log := slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := newDebugBody(io.NopCloser(strings.NewReader("")), log, context.Background(), "anthropic")
	buf := make([]byte, 16)
	_, _ = d.Read(buf) // EOF
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(sink.String(), "(empty") {
		t.Errorf("empty-body log line missing: %s", sink.String())
	}
}

// spec_adapter.go: PrepareBody coverage for the "not a rewrite candidate"
// branches — non-chat endpoint, non-OpenAI-wire body, empty body.

// TestPrepareBody_NonChatEndpoint_PassesBodyThrough asserts the switch
// arm that returns the body unchanged when typology.WireShape is neither
// chat-completions, embeddings, nor legacy-completions.
func TestPrepareBody_NonChatEndpoint_PassesBodyThrough(t *testing.T) {
	ad := NewSpecAdapter(specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	body := []byte(`{"model":"foo"}`)
	got, rw, _, err := ad.PrepareBody(Request{
		WireShape:  typology.WireShapeOpenAIResponses, // not in the rewrite-eligible set
		BodyFormat: FormatOpenAI,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "bar"},
	})
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if !bytes.Equal(got, body) || rw != nil {
		t.Errorf("non-chat endpoint: body must pass through unchanged; got %s rewrites=%v", got, rw)
	}
}

// TestPrepareBody_NonOpenAIWire_NoRewrite asserts that a non-OpenAI-family
// body is NOT rewritten by the generic passthrough when the adapter does not
// declare PassthroughModelInBody — the Gemini/Bedrock contract (model applied
// by the transport/codec, not the body). A spec that DOES declare the
// capability is covered by TestPrepareBody_Anthropic_ModelInBody_Wired.
func TestPrepareBody_NonOpenAIWire_NoRewrite(t *testing.T) {
	// specFrom leaves PassthroughModelInBody false, so the model-in-body gate
	// (OpenAI-family wire OR capability) is not satisfied for FormatAnthropic.
	ad := NewSpecAdapter(specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatAnthropic), slog.Default())
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[]}`)
	got, rw, _, err := ad.PrepareBody(Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatAnthropic,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "claude-3-7-sonnet"},
	})
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if !bytes.Equal(got, body) || rw != nil {
		t.Errorf("non-OpenAI-wire format: body must not be JSON-rewritten; got %s rewrites=%v", got, rw)
	}
}

// TestPrepareBody_EmptyBodyShortCircuits covers the len(req.Body) == 0
// guard on the passthrough path.
func TestPrepareBody_EmptyBodyShortCircuits(t *testing.T) {
	ad := NewSpecAdapter(specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	got, rw, _, err := ad.PrepareBody(Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       nil,
		Target:     CallTarget{ProviderModelID: "gpt-4o-mini"},
	})
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if got != nil || rw != nil {
		t.Errorf("empty body: want (nil,nil,nil); got body=%v rewrites=%v", got, rw)
	}
}

// TestPrepareBody_NoProviderModelID_BodyUnchanged covers the
// ProviderModelID == "" guard.
func TestPrepareBody_NoProviderModelID_BodyUnchanged(t *testing.T) {
	ad := NewSpecAdapter(specFrom(&fakeTransport{}, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	body := []byte(`{"model":"original-id"}`)
	got, rw, _, err := ad.PrepareBody(Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       body,
		Target:     CallTarget{}, // empty ProviderModelID
	})
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if !bytes.Equal(got, body) || rw != nil {
		t.Errorf("empty ProviderModelID: body must pass through unchanged; got %s rewrites=%v", got, rw)
	}
}

// spec_adapter.go: Probe delegation.

// TestProbe_DelegatesToTransport asserts the adapter forwards Probe
// straight to the Transport (the only thing it can usefully do).
func TestProbe_DelegatesToTransport(t *testing.T) {
	want := &ProbeResult{OK: true, LatencyMs: 42, Detail: "delegated"}
	tr := &fakeTransport{probe: func(context.Context, CallTarget) (*ProbeResult, error) { return want, nil }}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	got, err := ad.Probe(context.Background(), CallTarget{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got != want {
		t.Errorf("Probe: want %+v, got %+v", want, got)
	}
}

// spec_adapter.go: FilterResponseHeaders empty-source short-circuit.

// TestFilterResponseHeaders_EmptySource_ReturnsEmpty covers the
// len(src) == 0 branch.
func TestFilterResponseHeaders_EmptySource_ReturnsEmpty(t *testing.T) {
	got := FilterResponseHeaders(nil, FormatOpenAI, http.Header{}, false)
	if got == nil {
		t.Fatal("FilterResponseHeaders: must return a non-nil empty header map")
	}
	if len(got) != 0 {
		t.Errorf("FilterResponseHeaders: expected empty map, got %v", got)
	}
}

// usage_extractor.go: empty-raw, unknown-format, and nil-Usage branches.

// TestExtractUsage_EmptyBody_ReturnsZero covers the len(raw) == 0 short
// circuit.
func TestExtractUsage_EmptyBody_ReturnsZero(t *testing.T) {
	if got := ExtractUsage(nil, FormatOpenAI); !isZeroUsage(got) {
		t.Errorf("empty body: want zero Usage; got %+v", got)
	}
	if got := ExtractUsage([]byte{}, FormatOpenAI); !isZeroUsage(got) {
		t.Errorf("zero-length body: want zero Usage; got %+v", got)
	}
}

// TestExtractUsage_UnknownFormat_ReturnsZero covers the default branch
// of the format switch (an extant Format whose ExtractUsage doesn't yet
// have a wired Tier-1 normalizer).
func TestExtractUsage_UnknownFormat_ReturnsZero(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	got := ExtractUsage(body, Format("does-not-exist"))
	if !isZeroUsage(got) {
		t.Errorf("unknown format: want zero Usage; got %+v", got)
	}
}

// TestExtractUsage_BedrockReachesAnthropicNormalizer pins the
// FormatBedrock case (extra-careful: Bedrock wraps Anthropic; once the
// envelope is stripped, the codec routes here as Anthropic).
func TestExtractUsage_BedrockReachesAnthropicNormalizer(t *testing.T) {
	body := []byte(`{
		"model":"claude-3-5-sonnet",
		"content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":3,"output_tokens":5}
	}`)
	got := ExtractUsage(body, FormatBedrock)
	if got.PromptTokens == nil || *got.PromptTokens != 3 {
		t.Errorf("Bedrock PromptTokens: want 3, got %v", got.PromptTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != 5 {
		t.Errorf("Bedrock CompletionTokens: want 5, got %v", got.CompletionTokens)
	}
}

// TestExtractUsage_VertexReachesGeminiNormalizer pins FormatVertex →
// Gemini normalizer routing.
func TestExtractUsage_VertexReachesGeminiNormalizer(t *testing.T) {
	body := []byte(`{
		"modelVersion":"gemini-2.5-pro",
		"candidates":[{"content":{"parts":[{"text":"hi"}]}}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"totalTokenCount":30}
	}`)
	got := ExtractUsage(body, FormatVertex)
	if got.PromptTokens == nil || *got.PromptTokens != 10 {
		t.Errorf("Vertex PromptTokens: want 10, got %v", got.PromptTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != 20 {
		t.Errorf("Vertex CompletionTokens: want 20, got %v", got.CompletionTokens)
	}
}

// TestExtractUsage_GarbageBody_ReturnsZero verifies the np.Usage == nil
// fallback when a body for a known format is unrecognisable to the
// normalizer (covers usage_extractor.go:105-107).
func TestExtractUsage_GarbageBody_ReturnsZero(t *testing.T) {
	got := ExtractUsage([]byte(`not json`), FormatOpenAI)
	if !isZeroUsage(got) {
		t.Errorf("garbage body: want zero Usage; got %+v", got)
	}
}

func isZeroUsage(u Usage) bool {
	return u.PromptTokens == nil && u.CompletionTokens == nil &&
		u.TotalTokens == nil && u.CacheReadTokens == nil &&
		u.CacheCreationTokens == nil && u.ReasoningTokens == nil
}

// syncBuffer is a thread-safe bytes.Buffer for slog handlers that may
// be invoked from multiple goroutines (stream decoder Close in particular).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// forceNilNormalizer always returns nil from Normalize so we can drive
// the dispatcher's synthesized-fallback branch (spec_adapter.go around
// line 189). A separate type rather than extending fakeErrorNormalizer
// keeps this file additive per the tests-only-own-data binding.
type forceNilNormalizer struct{}

func (forceNilNormalizer) Normalize(int, http.Header, []byte) *ProviderError { return nil }

// _ retains the normalize import in case a future refactor wants to
// cross-check the canonical Usage type — kept here so the test file's
// import set documents the public dependency.
var _ = normalize.Usage{}

// applyURLOverride + ExecuteWithBody wrapper coverage

// TestApplyURLOverride covers the four behaviour arms — colon-prefixed
// override replaces the action suffix, non-colon override replaces the
// full URL, empty override is a no-op, and an override against a URL
// without any colon segment appends.
func TestApplyURLOverride(t *testing.T) {
	cases := []struct {
		name, base, override, want string
	}{
		{"empty override no-op", "https://x/v1/models/m:embedContent", "", "https://x/v1/models/m:embedContent"},
		{"colon replaces last colon segment", "https://x/v1/models/m:embedContent", ":batchEmbedContents", "https://x/v1/models/m:batchEmbedContents"},
		// LastIndex(":") picks the protocol-separator when no action suffix
		// exists — Gemini URLs always carry the action suffix in real use,
		// so this edge case is documented but not a production path.
		{"colon override on URL with only protocol colon strips at protocol", "https://x/v1/models/m", ":batchEmbedContents", "https:batchEmbedContents"},
		{"non-colon override is full URL replacement", "https://x/old", "https://y/new", "https://y/new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyURLOverride(tc.base, tc.override); got != tc.want {
				t.Fatalf("applyURLOverride(%q,%q): got %q want %q", tc.base, tc.override, got, tc.want)
			}
		})
	}
}

// TestExecuteWithBody_DelegatesToInternal pins the contract that the
// public ExecuteWithBody method delegates to executeWithBodyAndURL with
// an empty urlOverride — i.e. the codec-provided body is sent verbatim
// to the Transport's default URL.
func TestExecuteWithBody_DelegatesToInternal(t *testing.T) {
	var capturedURL string
	tr := &fakeTransport{
		buildURL: func(_ CallTarget, _ typology.WireShape, _ bool) (string, error) {
			return "https://upstream/v1/chat/completions", nil
		},
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			capturedURL = r.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		},
	}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := ad.ExecuteWithBody(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"already":"encoded"}`),
	}, []byte(`{"already":"encoded"}`), nil, "")
	if err != nil {
		t.Fatalf("ExecuteWithBody: %v", err)
	}
	if capturedURL != "https://upstream/v1/chat/completions" {
		t.Fatalf("URL override should be empty by default; got captured=%q", capturedURL)
	}
}

// Cover applyURLOverride all branches (pure function).
func TestApplyURLOverride_AllBranches(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		override string
		want     string
	}{
		{"empty override returns base", "https://x/v1/models/m:embedContent", "", "https://x/v1/models/m:embedContent"},
		{"colon override replaces last segment", "https://x/v1/models/m:embedContent", ":batchEmbedContents", "https://x/v1/models/m:batchEmbedContents"},
		{"colon override on bare path with no colon appends", "/v1/models/m", ":batchEmbedContents", "/v1/models/m:batchEmbedContents"},
		{"non-colon override is full replacement", "https://x/v1/foo", "https://y/v2/bar", "https://y/v2/bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyURLOverride(tc.base, tc.override); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Cover the read-body error branch (line 290-296).
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("simulated upstream stream broke") }
func (errReader) Close() error             { return nil }

func TestExecute_ReadBodyError_SurfacesUpstreamError(t *testing.T) {
	tr := &fakeTransport{
		do: func(_ context.Context, _ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       errReader{},
			}, nil
		},
	}
	ad := NewSpecAdapter(specFrom(tr, &fakeCodec{}, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatOpenAI), slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{}`),
	})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProviderError, got %T (%v)", err, err)
	}
	if pe.Status != http.StatusBadGateway || pe.Code != CodeUpstreamError {
		t.Errorf("got status=%d code=%q (want 502/upstream_error)", pe.Status, pe.Code)
	}
}

// TestExecute_CodecURLOverride_AppliedToBaseURL pins the contract: a codec that
// emits EncodeResult.URLOverride must see that override land on the
// dispatched URL through Execute (full path: prepareBodyFull → codec
// translation surfaces the override → executeWithBodyAndURL applies it).
func TestExecute_CodecURLOverride_AppliedToBaseURL(t *testing.T) {
	var capturedURL string
	tr := &fakeTransport{
		buildURL: func(CallTarget, typology.WireShape, bool) (string, error) {
			return "https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:embedContent", nil
		},
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			capturedURL = r.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"embeddings":[{"values":[0.1]}]}`)),
			}, nil
		},
	}
	cd := &fakeCodec{
		encoded:     []byte(`{"requests":[]}`),
		decoded:     []byte(`{"object":"list","data":[]}`),
		urlOverride: ":batchEmbedContents",
	}
	ad := NewSpecAdapter(specFrom(tr, cd, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatGemini), slog.Default())
	_, err := ad.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: FormatOpenAI, // forces codec translation in prepareBodyFull
		Body:       []byte(`{"model":"text-embedding-004","input":["a","b"]}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasSuffix(capturedURL, ":batchEmbedContents") {
		t.Errorf("URLOverride not applied via Execute: dispatched URL = %q, want suffix :batchEmbedContents", capturedURL)
	}
}

// TestExecuteWithBody_CodecURLOverride_ThreadsThrough pins the fix
// on the cache-MISS fast path: PrepareBody surfaces the codec's URLOverride
// and ExecuteWithBody must honor the value passed to it — the dispatcher no
// longer re-derives it by peeking at provider-specific body fields.
func TestExecuteWithBody_CodecURLOverride_ThreadsThrough(t *testing.T) {
	var capturedURL string
	tr := &fakeTransport{
		buildURL: func(CallTarget, typology.WireShape, bool) (string, error) {
			return "https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:embedContent", nil
		},
		do: func(_ context.Context, r *http.Request) (*http.Response, error) {
			capturedURL = r.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"embeddings":[{"values":[0.1]}]}`)),
			}, nil
		},
	}
	cd := &fakeCodec{
		encoded:     []byte(`{"requests":[]}`),
		decoded:     []byte(`{"object":"list","data":[]}`),
		urlOverride: ":batchEmbedContents",
	}
	ad := NewSpecAdapter(specFrom(tr, cd, &fakeStreamDecoder{}, &fakeErrorNormalizer{}, FormatGemini), slog.Default())

	// PrepareBody surfaces the URLOverride the codec emitted.
	body, _, urlOverride, err := ad.PrepareBody(Request{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: FormatOpenAI,
		Body:       []byte(`{"model":"text-embedding-004","input":["a","b"]}`),
	})
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	if urlOverride != ":batchEmbedContents" {
		t.Fatalf("PrepareBody URLOverride = %q, want :batchEmbedContents", urlOverride)
	}

	// ExecuteWithBody must apply the passed override to the dispatched URL.
	_, err = ad.ExecuteWithBody(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: FormatGemini,
	}, body, nil, urlOverride)
	if err != nil {
		t.Fatalf("ExecuteWithBody: %v", err)
	}
	if !strings.HasSuffix(capturedURL, ":batchEmbedContents") {
		t.Errorf("URLOverride not threaded through ExecuteWithBody: dispatched URL = %q", capturedURL)
	}
}
