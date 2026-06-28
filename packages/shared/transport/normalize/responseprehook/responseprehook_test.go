package responseprehook

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// fakeNormalizer is a normcore.Normalizer that records the Meta it was
// called with and returns a canned payload. Lets us assert that Build's
// Meta-normalization (lowercase adapter, strip content-type params,
// derive Stream from text/event-stream) reaches the Registry intact.
type fakeNormalizer struct {
	gotMeta normcore.Meta
	gotBody []byte
	payload normcore.NormalizedPayload
	err     error
}

func (f *fakeNormalizer) ID() string { return "fake" }
func (f *fakeNormalizer) Normalize(_ context.Context, body []byte, meta normcore.Meta) (normcore.NormalizedPayload, error) {
	f.gotMeta = meta
	f.gotBody = body
	if f.err != nil {
		return normcore.NormalizedPayload{}, f.err
	}
	return f.payload, nil
}

func newRegistryWithFake(key string, fake *fakeNormalizer) *normcore.Registry {
	r := normcore.NewRegistry()
	r.Register(key, fake)
	return r
}

func TestBuild_NilRegistry_ReturnsNil(t *testing.T) {
	if cb := Build(Options{Registry: nil}); cb != nil {
		t.Errorf("Build with nil Registry must return nil callback, got non-nil")
	}
}

func TestBuild_StampsCiNormalizedOnSuccess(t *testing.T) {
	fake := &fakeNormalizer{
		payload: normcore.NormalizedPayload{
			Kind:     "ai-chat",
			Protocol: "anthropic-messages",
		},
	}
	reg := newRegistryWithFake("anthropic-messages", fake)

	cb := Build(Options{
		Ctx:          context.Background(),
		Registry:     reg,
		AdapterID:    "Anthropic-Messages", // mixed case — should lowercase
		EndpointPath: "/v1/messages",
		ContentType:  "text/event-stream; charset=utf-8", // should strip params
	})
	if cb == nil {
		t.Fatal("Build returned nil for valid Options")
	}

	ci := &hookcore.HookInput{}
	cb([]byte("data: hi\n\n"), ci)

	if ci.Normalized == nil {
		t.Fatal("expected ci.Normalized stamped, got nil")
	}
	if ci.Normalized.Protocol != "anthropic-messages" {
		t.Errorf("Normalized.Protocol = %q, want anthropic-messages", ci.Normalized.Protocol)
	}
	if fake.gotMeta.AdapterType != "anthropic-messages" {
		t.Errorf("Meta.AdapterType = %q, want lower-cased anthropic-messages", fake.gotMeta.AdapterType)
	}
	if fake.gotMeta.ContentType != "text/event-stream" {
		t.Errorf("Meta.ContentType = %q, want bare text/event-stream (params stripped)", fake.gotMeta.ContentType)
	}
	if !fake.gotMeta.Stream {
		t.Errorf("Meta.Stream = false; want true for text/event-stream response")
	}
	if fake.gotMeta.Direction != normcore.DirectionResponse {
		t.Errorf("Meta.Direction = %q, want %q", fake.gotMeta.Direction, normcore.DirectionResponse)
	}
	if fake.gotMeta.EndpointPath != "/v1/messages" {
		t.Errorf("Meta.EndpointPath = %q, want /v1/messages", fake.gotMeta.EndpointPath)
	}
}

func TestBuild_OnPayloadFiresAfterStamp(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "openai-chat"}}
	reg := newRegistryWithFake("openai-chat", fake)

	var (
		onPayloadCalls int
		seenRawLen     int
		seenProtocol   string
		seenAfterStamp bool
	)

	cb := Build(Options{
		Ctx:         context.Background(),
		Registry:    reg,
		AdapterID:   "openai-chat",
		ContentType: "text/event-stream",
		OnPayload: func(payload *normcore.NormalizedPayload, rawBody []byte) {
			onPayloadCalls++
			if payload != nil {
				seenProtocol = payload.Protocol
			}
			seenRawLen = len(rawBody)
		},
	})
	ci := &hookcore.HookInput{}
	// Pre-flight assertion: ci.Normalized starts nil; if OnPayload
	// already fired the stamp must be visible by the time OnPayload
	// runs — capture it.
	cb([]byte("data: x\n\n"), ci)
	if ci.Normalized != nil {
		seenAfterStamp = true
	}

	if onPayloadCalls != 1 {
		t.Errorf("OnPayload called %d times, want 1", onPayloadCalls)
	}
	if seenProtocol != "openai-chat" {
		t.Errorf("OnPayload saw protocol %q, want openai-chat", seenProtocol)
	}
	if seenRawLen != len("data: x\n\n") {
		t.Errorf("OnPayload saw rawBody len=%d, want %d", seenRawLen, len("data: x\n\n"))
	}
	if !seenAfterStamp {
		t.Errorf("ci.Normalized should be stamped before OnPayload returns")
	}
}

func TestBuild_RegistryErrorDropsSilently(t *testing.T) {
	fake := &fakeNormalizer{err: errors.New("boom")}
	reg := newRegistryWithFake("err-adapter", fake)

	var onPayloadCalls int
	cb := Build(Options{
		Ctx:         context.Background(),
		Registry:    reg,
		AdapterID:   "err-adapter",
		ContentType: "text/event-stream",
		OnPayload: func(*normcore.NormalizedPayload, []byte) {
			onPayloadCalls++
		},
	})
	ci := &hookcore.HookInput{}
	cb([]byte("data: anything\n\n"), ci)

	if ci.Normalized != nil {
		t.Errorf("ci.Normalized stamped despite Registry error; got %+v", ci.Normalized)
	}
	if onPayloadCalls != 0 {
		t.Errorf("OnPayload fired %d times despite Registry error; want 0", onPayloadCalls)
	}
}

func TestBuild_EmptyBody_NoOp(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "p"}}
	reg := newRegistryWithFake("p", fake)
	cb := Build(Options{Registry: reg, AdapterID: "p", ContentType: "text/event-stream"})

	ci := &hookcore.HookInput{}
	cb(nil, ci)
	cb([]byte{}, ci)

	if ci.Normalized != nil {
		t.Errorf("empty body should not stamp Normalized")
	}
	if fake.gotBody != nil {
		t.Errorf("Registry called with empty body; should have short-circuited")
	}
}

func TestBuild_NilHookInput_NoOp(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "p"}}
	reg := newRegistryWithFake("p", fake)
	cb := Build(Options{Registry: reg, AdapterID: "p", ContentType: "text/event-stream"})

	// Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil ci HookInput caused panic: %v", r)
		}
	}()
	cb([]byte("data: x\n\n"), nil)

	if fake.gotBody != nil {
		t.Errorf("Registry called with nil ci; should have short-circuited")
	}
}

func TestBuild_DefaultsContentTypeAndDirection(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "p"}}
	reg := newRegistryWithFake("p", fake)
	cb := Build(Options{
		Registry:  reg,
		AdapterID: "p",
		// ContentType + Direction omitted — Build should default.
	})
	ci := &hookcore.HookInput{}
	cb([]byte("data: x\n\n"), ci)

	if fake.gotMeta.ContentType != "text/event-stream" {
		t.Errorf("default ContentType = %q, want text/event-stream", fake.gotMeta.ContentType)
	}
	if fake.gotMeta.Direction != normcore.DirectionResponse {
		t.Errorf("default Direction = %q, want %q", fake.gotMeta.Direction, normcore.DirectionResponse)
	}
	if !fake.gotMeta.Stream {
		t.Errorf("Stream should default true when ContentType defaulted to text/event-stream")
	}
}

func TestBuild_NonSSEContentType_StreamFalse(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "p"}}
	reg := newRegistryWithFake("p", fake)
	cb := Build(Options{
		Registry:    reg,
		AdapterID:   "p",
		ContentType: "application/json",
		Direction:   normcore.DirectionResponse,
	})
	ci := &hookcore.HookInput{}
	cb([]byte(`{"x":1}`), ci)

	if fake.gotMeta.Stream {
		t.Errorf("Stream = true for application/json; want false (only text/event-stream is streaming)")
	}
	if fake.gotMeta.ContentType != "application/json" {
		t.Errorf("ContentType passed through = %q, want application/json", fake.gotMeta.ContentType)
	}
}

func TestBuild_NilCtxDefaultsToBackground(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "p"}}
	reg := newRegistryWithFake("p", fake)
	cb := Build(Options{
		// Ctx left nil — Build should default to context.Background()
		Registry:    reg,
		AdapterID:   "p",
		ContentType: "text/event-stream",
	})
	ci := &hookcore.HookInput{}
	// Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Ctx caused panic: %v", r)
		}
	}()
	cb([]byte("data: x\n\n"), ci)
	if ci.Normalized == nil {
		t.Fatalf("expected Normalized stamped even with nil Ctx (defaulted to Background)")
	}
}

// panickyNormalizer panics inside Normalize. Used to assert
// panic-safety — Build's wrapper recovers so the SSE pipeline never
// sees the panic.
type panickyNormalizer struct{}

func (panickyNormalizer) ID() string { return "panicky" }
func (panickyNormalizer) Normalize(context.Context, []byte, normcore.Meta) (normcore.NormalizedPayload, error) {
	panic("simulated tier-1 spec crash")
}

func TestBuild_RegistryPanic_RecoveredAndDropped(t *testing.T) {
	r := normcore.NewRegistry()
	r.Register("panicky", panickyNormalizer{})

	cb := Build(Options{
		Registry:    r,
		AdapterID:   "panicky",
		ContentType: "text/event-stream",
	})

	// Reset both timeseries so parallel sibling tests bumping
	// the same labels don't race the +1 / +0 deltas below.
	normalizePanicTotal.DeleteLabelValues("registry")
	prehookNormalizeDropTotal.DeleteLabelValues("panicky")

	// Panic counter MUST tick by 1 for location=registry.
	// Disjointness — drop counter MUST stay flat (panic is a
	// distinct category from drop; otherwise admins double-count).
	beforePanic := testutil.ToFloat64(normalizePanicTotal.WithLabelValues("registry"))
	beforeDrop := testutil.ToFloat64(prehookNormalizeDropTotal.WithLabelValues("panicky"))

	ci := &hookcore.HookInput{}
	// Must not panic.
	defer func() {
		if rr := recover(); rr != nil {
			t.Fatalf("Build callback let panic escape: %v", rr)
		}
	}()
	cb([]byte("data: anything\n\n"), ci)

	if ci.Normalized != nil {
		t.Errorf("ci.Normalized stamped despite normalizer panic; got %+v", ci.Normalized)
	}
	afterPanic := testutil.ToFloat64(normalizePanicTotal.WithLabelValues("registry"))
	afterDrop := testutil.ToFloat64(prehookNormalizeDropTotal.WithLabelValues("panicky"))
	if delta := afterPanic - beforePanic; delta != 1 {
		t.Errorf("expected nexus_normalize_panic_total{location=registry} +1, got delta=%v", delta)
	}
	if delta := afterDrop - beforeDrop; delta != 0 {
		t.Errorf("expected nexus_prehook_normalize_drop_total{adapter=panicky} +0 (panic is disjoint from drop), got delta=%v", delta)
	}
}

// erroringNormalizer returns a non-panic, non-ErrUnsupported error.
// Drives Build's "Registry.Normalize returned err" silent-drop path
// so we can assert the S5 counter bumps and the WARN log fires.
type erroringNormalizer struct{ err error }

func (erroringNormalizer) ID() string { return "erroring" }
func (e erroringNormalizer) Normalize(context.Context, []byte, normcore.Meta) (normcore.NormalizedPayload, error) {
	return normcore.NormalizedPayload{}, e.err
}

// TestBuild_NormalizeError_BumpsDropCounterAndWarns pins the
// drop-on-normalize-error contract:
// when Registry.Normalize returns a non-nil non-panic error, the
// pre-hook callback used to drop silently and the hook executor saw
// the flat-text fallback. Now it MUST:
//   - bump nexus_prehook_normalize_drop_total{adapter="<id>"} by 1
//   - emit a WARN log line naming the adapter + error
//   - still not stamp ci.Normalized (preserves the old don't-panic
//     contract; we surface, we don't change semantics)
func TestBuild_NormalizeError_BumpsDropCounterAndWarns(t *testing.T) {
	r := normcore.NewRegistry()
	r.Register("erroring", erroringNormalizer{err: errors.New("simulated tier-1 hard failure")})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cb := Build(Options{
		Registry:    r,
		AdapterID:   "erroring",
		ContentType: "text/event-stream",
		Logger:      logger,
	})

	// Reset before snapshot for parallel-safe delta.
	prehookNormalizeDropTotal.DeleteLabelValues("erroring")

	before := testutil.ToFloat64(prehookNormalizeDropTotal.WithLabelValues("erroring"))

	ci := &hookcore.HookInput{}
	cb([]byte("data: anything\n\n"), ci)

	if ci.Normalized != nil {
		t.Errorf("ci.Normalized must remain nil on normalize err (preserves prior contract); got %+v", ci.Normalized)
	}
	after := testutil.ToFloat64(prehookNormalizeDropTotal.WithLabelValues("erroring"))
	if delta := after - before; delta != 1 {
		t.Errorf("expected nexus_prehook_normalize_drop_total{adapter=erroring} +1, got delta=%v", delta)
	}
	if !strings.Contains(logBuf.String(), "Registry.Normalize returned error") {
		t.Errorf("expected WARN log naming the err path, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "erroring") {
		t.Errorf("expected adapter id in WARN log, got: %s", logBuf.String())
	}
}

func TestBuild_OnPayloadPanic_RecoveredAndDropped(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "ok"}}
	reg := newRegistryWithFake("ok", fake)

	cb := Build(Options{
		Registry:    reg,
		AdapterID:   "ok",
		ContentType: "text/event-stream",
		OnPayload: func(*normcore.NormalizedPayload, []byte) {
			panic("simulated audit-stamp crash")
		},
	})

	// Reset before snapshot for parallel-safe delta.
	normalizePanicTotal.DeleteLabelValues("on_payload")

	// Counter MUST tick by 1 for location=on_payload.
	before := testutil.ToFloat64(normalizePanicTotal.WithLabelValues("on_payload"))

	ci := &hookcore.HookInput{}
	defer func() {
		if rr := recover(); rr != nil {
			t.Fatalf("Build callback let OnPayload panic escape: %v", rr)
		}
	}()
	cb([]byte("data: x\n\n"), ci)

	if ci.Normalized == nil {
		t.Errorf("ci.Normalized should still be stamped before OnPayload runs")
	}
	after := testutil.ToFloat64(normalizePanicTotal.WithLabelValues("on_payload"))
	if delta := after - before; delta != 1 {
		t.Errorf("expected nexus_normalize_panic_total{location=on_payload} +1, got delta=%v", delta)
	}
}

func TestBuild_NilLoggerDefaultsToSlogDefault(t *testing.T) {
	r := normcore.NewRegistry()
	r.Register("panicky", panickyNormalizer{})
	cb := Build(Options{
		Registry:    r,
		AdapterID:   "panicky",
		ContentType: "text/event-stream",
		Logger:      nil,
	})
	defer func() {
		if rr := recover(); rr != nil {
			t.Fatalf("nil Logger + panic let panic escape: %v", rr)
		}
	}()
	cb([]byte("data: x\n\n"), &hookcore.HookInput{})
}

func TestPanicError_String(t *testing.T) {
	if errPanicked.Error() == "" || !strings.Contains(errPanicked.Error(), "panicked") {
		t.Errorf("errPanicked.Error() should describe the panic; got %q", errPanicked.Error())
	}
}

func TestBuild_AdapterIDLowercase(t *testing.T) {
	fake := &fakeNormalizer{payload: normcore.NormalizedPayload{Protocol: "p"}}
	// Registry routes by lower-cased key; register lower-case so the
	// callback's lower-case AdapterType reaches the fake.
	reg := newRegistryWithFake(strings.ToLower("ANTHROPIC-Messages-V1"), fake)
	cb := Build(Options{
		Registry:    reg,
		AdapterID:   "ANTHROPIC-Messages-V1",
		ContentType: "text/event-stream",
	})
	ci := &hookcore.HookInput{}
	cb([]byte("data: x\n\n"), ci)
	if fake.gotMeta.AdapterType != strings.ToLower("ANTHROPIC-Messages-V1") {
		t.Errorf("AdapterType = %q, want lowercased", fake.gotMeta.AdapterType)
	}
}
