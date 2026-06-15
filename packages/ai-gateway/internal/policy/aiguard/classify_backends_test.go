// Tests for the aiguard classify entrypoint and its backends (in-process,
// external HTTP, adapter), plus the cache and Prometheus registration. Each
// test asserts observable behavior — decision, error shape, side-effects,
// registered collectors.
package aiguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/prometheus/client_golang/prometheus"
)

// ── classify.go ─────────────────────────────────────────────────────

func TestBackendUnavailable_ErrorString(t *testing.T) {
	e := &BackendUnavailable{Detail: "boom"}
	if got := e.Error(); got != "backend_unavailable: boom" {
		t.Fatalf("Error() = %q; want prefix + detail", got)
	}
}

func TestClassify_PublicWrapper_DelegatesToImpl(t *testing.T) {
	// Classify() is a thin shim. Exercise it to bring the wrapper line
	// to 100% and assert it returns whatever classifyImpl returns.
	_, rdb := newMiniRedis(t)
	cache := NewCache(rdb)
	sink := &stubTrafficSink{}
	be := &stubBackend{resp: &Response{Decision: "approve"}}
	cfg := &RuntimeConfig{
		BackendMode:        "configured_provider",
		BackendFingerprint: "fp-public",
		PromptTemplate:     DefaultPrompt,
		TimeoutMs:          1000,
		CacheTTLSeconds:    60,
	}
	resp, err := Classify(context.Background(), Request{
		DetectorType: "prompt_injection",
		Content:      "hello",
	}, cfg, be, cache, sink)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if resp.Decision != "approve" {
		t.Fatalf("decision: %q", resp.Decision)
	}
}

func TestClassifyImpl_PromptRenderFailure_EmitsEventAnd503(t *testing.T) {
	_, rdb := newMiniRedis(t)
	cache := NewCache(rdb)
	sink := &stubTrafficSink{}
	be := &stubBackend{resp: &Response{Decision: "approve"}}
	cfg := &RuntimeConfig{
		BackendMode:        "configured_provider",
		BackendFingerprint: "fp-render",
		// Invalid template — unclosed action triggers parse error inside Render.
		PromptTemplate:  "{{.Unclosed",
		TimeoutMs:       1000,
		CacheTTLSeconds: 60,
	}
	_, err := classifyImpl(context.Background(), Request{
		DetectorType: "x", Content: "y",
	}, cfg, be, cache, sink)
	var bu *BackendUnavailable
	if !errors.As(err, &bu) {
		t.Fatalf("want BackendUnavailable, got %T %v", err, err)
	}
	if bu.Detail != "prompt_render_failed" {
		t.Fatalf("detail: %q", bu.Detail)
	}
	if be.callCount != 0 {
		t.Fatal("backend must not be called when prompt render fails")
	}
	if len(sink.events) != 1 {
		t.Fatalf("want 1 audit event on render failure, got %d", len(sink.events))
	}
	if !strings.Contains(sink.events[0].ErrorDetail, "prompt_render_failed") {
		t.Errorf("audit event missing render failure detail: %+v", sink.events[0])
	}
}

func TestClassifyImpl_ZeroTimeout_FallsBackTo30s(t *testing.T) {
	// TimeoutMs=0 should fall through to the 30s safety-net default — we
	// just need the call to succeed without context being immediately
	// cancelled. The stub returns instantly.
	_, rdb := newMiniRedis(t)
	cache := NewCache(rdb)
	sink := &stubTrafficSink{}
	be := &stubBackend{resp: &Response{Decision: "approve"}}
	cfg := &RuntimeConfig{
		BackendMode:        "configured_provider",
		BackendFingerprint: "fp-zero-timeout",
		PromptTemplate:     DefaultPrompt,
		TimeoutMs:          0, // exercise default-branch
		CacheTTLSeconds:    60,
	}
	if _, err := classifyImpl(context.Background(), Request{
		DetectorType: "x", Content: "y",
	}, cfg, be, cache, sink); err != nil {
		t.Fatalf("Classify with zero TimeoutMs: %v", err)
	}
}

func TestClassifyImpl_BackendTimeout_TaggedTimeout(t *testing.T) {
	_, rdb := newMiniRedis(t)
	cache := NewCache(rdb)
	sink := &stubTrafficSink{}
	// delay > timeout → DeadlineExceeded.
	be := &stubBackend{resp: &Response{Decision: "approve"}, delay: 50 * time.Millisecond}
	cfg := &RuntimeConfig{
		BackendMode:        "configured_provider",
		BackendFingerprint: "fp-timeout",
		PromptTemplate:     DefaultPrompt,
		TimeoutMs:          5,
		CacheTTLSeconds:    60,
	}
	_, err := classifyImpl(context.Background(), Request{
		DetectorType: "x", Content: "y",
	}, cfg, be, cache, sink)
	var bu *BackendUnavailable
	if !errors.As(err, &bu) {
		t.Fatalf("want BackendUnavailable, got %T %v", err, err)
	}
	// The deadline-exceeded path is the one we wanted to exercise; the
	// audit event has the underlying error string.
	if len(sink.events) != 1 || sink.events[0].ErrorDetail == "" {
		t.Fatalf("audit event missing timeout detail: %+v", sink.events)
	}
}

func TestClassifyImpl_TTLZero_SkipsCacheWrite(t *testing.T) {
	s, rdb := newMiniRedis(t)
	cache := NewCache(rdb)
	sink := &stubTrafficSink{}
	be := &stubBackend{resp: &Response{Decision: "approve"}}
	cfg := &RuntimeConfig{
		BackendMode:        "configured_provider",
		BackendFingerprint: "fp-ttl-zero",
		PromptTemplate:     DefaultPrompt,
		TimeoutMs:          1000,
		CacheTTLSeconds:    0, // disabled → Set is a no-op
	}
	if _, err := classifyImpl(context.Background(), Request{
		DetectorType: "x", Content: "y",
	}, cfg, be, cache, sink); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got := len(s.Keys()); got != 0 {
		t.Errorf("CacheTTLSeconds=0 must not write; got %d keys", got)
	}
}

func TestEmit_NilSink_NoPanic(t *testing.T) {
	// emit() must tolerate nil sinks for ad-hoc callers.
	emit(context.Background(), nil, TrafficEvent{DetectorType: "x"})
}

// ── inproc.go ───────────────────────────────────────────────────────

type stubInProcLoader struct {
	cfg *configstore.AIGuardConfig
	err error
}

func (s *stubInProcLoader) Load(_ context.Context) (*configstore.AIGuardConfig, error) {
	return s.cfg, s.err
}

func TestInProcClient_Classify_HappyPath(t *testing.T) {
	loader := &stubInProcLoader{cfg: &configstore.AIGuardConfig{
		ID:                 "singleton",
		BackendMode:        "configured_provider",
		BackendFingerprint: "fp-inproc",
		PromptTemplate:     DefaultPrompt,
		TimeoutMs:          1000,
		CacheTTLSeconds:    60,
	}}
	cc := NewConfigCache(loader, time.Minute, silentSlog())
	_, rdb := newMiniRedis(t)
	cache := NewCache(rdb)
	sink := &stubTrafficSink{}
	be := &stubBackend{resp: &Response{Decision: "approve"}}

	var capturedRC *RuntimeConfig
	client := NewInProcClient(cc, func(rc *RuntimeConfig) (Backend, error) {
		capturedRC = rc
		return be, nil
	}, cache, sink)

	resp, err := client.Classify(context.Background(), Request{
		DetectorType: "prompt_injection", Content: "hi",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if resp.Decision != "approve" {
		t.Fatalf("decision: %q", resp.Decision)
	}
	// Verify the RuntimeConfig built from AIGuardConfig is faithful.
	if capturedRC == nil {
		t.Fatal("backendFor was not called")
	}
	if capturedRC.BackendFingerprint != "fp-inproc" || capturedRC.TimeoutMs != 1000 {
		t.Errorf("RuntimeConfig mismatch: %+v", capturedRC)
	}
}

func TestInProcClient_Classify_ConfigLoadError(t *testing.T) {
	loader := &stubInProcLoader{err: errors.New("db down")}
	cc := NewConfigCache(loader, time.Minute, silentSlog())
	_, rdb := newMiniRedis(t)
	client := NewInProcClient(cc, func(_ *RuntimeConfig) (Backend, error) {
		t.Fatal("backendFor must not be called on config error")
		return nil, nil
	}, NewCache(rdb), &stubTrafficSink{})
	_, err := client.Classify(context.Background(), Request{DetectorType: "x", Content: "y"})
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("expected db-down error, got %v", err)
	}
}

func TestInProcClient_Classify_BackendForError(t *testing.T) {
	loader := &stubInProcLoader{cfg: &configstore.AIGuardConfig{
		ID: "singleton", BackendMode: "configured_provider",
		BackendFingerprint: "fp", PromptTemplate: DefaultPrompt,
		TimeoutMs: 1000, CacheTTLSeconds: 60,
	}}
	cc := NewConfigCache(loader, time.Minute, silentSlog())
	_, rdb := newMiniRedis(t)
	client := NewInProcClient(cc, func(_ *RuntimeConfig) (Backend, error) {
		return nil, errors.New("no backend available")
	}, NewCache(rdb), &stubTrafficSink{})
	_, err := client.Classify(context.Background(), Request{DetectorType: "x", Content: "y"})
	if err == nil || !strings.Contains(err.Error(), "no backend available") {
		t.Fatalf("expected backend-for error, got %v", err)
	}
}

// ── metrics.go ──────────────────────────────────────────────────────

func TestRegister_IdempotentAndCollectorsAttached(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg) // first call
	Register(reg) // second call — AlreadyRegisteredError must be tolerated

	// All six metric Names must be reachable in the registry.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := map[string]bool{
		"nexus_aiguard_cache_hits_total":      false,
		"nexus_aiguard_cache_misses_total":    false,
		"nexus_aiguard_cache_writes_total":    false,
		"nexus_aiguard_judge_latency_seconds": false,
		"nexus_aiguard_judge_errors_total":    false,
		"nexus_aiguard_decisions_total":       false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %q not registered", name)
		}
	}
}

func TestRegister_PanicsOnNonAlreadyRegisteredError(t *testing.T) {
	// A registerer that surfaces a non-AlreadyRegisteredError must
	// panic — the production code chose hard-fail because metric wiring
	// errors that are NOT duplicate-registration indicate a programming
	// bug operators must see at boot.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on non-AlreadyRegisteredError")
		}
	}()
	Register(brokenRegisterer{})
}

type brokenRegisterer struct{}

func (brokenRegisterer) Register(prometheus.Collector) error  { return errors.New("nope") }
func (brokenRegisterer) MustRegister(...prometheus.Collector) {}
func (brokenRegisterer) Unregister(prometheus.Collector) bool { return false }

// ── backend_external.go ─────────────────────────────────────────────

func TestExternalBackend_NilHTTPClient(t *testing.T) {
	b := &ExternalBackend{URL: "http://example.com", Model: "m"} // HTTPClient nil
	_, err := b.Call(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "HTTPClient is nil") {
		t.Fatalf("expected HTTPClient nil error, got %v", err)
	}
}

func TestExternalBackend_BadURLBuildRequestFails(t *testing.T) {
	b := &ExternalBackend{
		// %ZZ is an invalid percent-encoding — url.Parse inside
		// http.NewRequestWithContext returns an error.
		URL:        "http://example.com/%ZZ",
		Model:      "m",
		HTTPClient: &http.Client{Timeout: time.Second},
	}
	_, err := b.Call(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("expected build-request error, got %v", err)
	}
}

func TestExternalBackend_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	b := &ExternalBackend{URL: srv.URL, Model: "m", HTTPClient: &http.Client{Timeout: time.Second}}
	_, err := b.Call(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("expected empty-choices error, got %v", err)
	}
}

func TestExternalBackend_UnparseableChatResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices": "not an array"}`))
	}))
	defer srv.Close()
	b := &ExternalBackend{URL: srv.URL, Model: "m", HTTPClient: &http.Client{Timeout: time.Second}}
	_, err := b.Call(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "parse chat response") {
		t.Fatalf("expected parse-chat-response error, got %v", err)
	}
}

// ── backend_provider.go ─────────────────────────────────────────────

func TestAdapterBackend_NilReceiver(t *testing.T) {
	var b *AdapterBackend
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not fully wired") {
		t.Fatalf("expected not-fully-wired error, got %v", err)
	}
}

func TestAdapterBackend_NilResolver(t *testing.T) {
	b := &AdapterBackend{Resolver: nil, Registry: provcore.NewRegistry(), ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not fully wired") {
		t.Fatalf("expected not-fully-wired error, got %v", err)
	}
}

func TestAdapterBackend_NilRegistry(t *testing.T) {
	res := &fakeResolver{target: provcore.CallTarget{Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: nil, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not fully wired") {
		t.Fatalf("expected not-fully-wired error, got %v", err)
	}
}

func TestAdapterBackend_InvalidFormat(t *testing.T) {
	reg := provcore.NewRegistry()
	reg.Freeze()
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "x", Format: provcore.Format("not-a-real-format")}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "invalid adapter_type") {
		t.Fatalf("expected invalid-adapter_type error, got %v", err)
	}
}

func TestAdapterBackend_NoAdapterForFormat(t *testing.T) {
	// Empty registry — Format valid but Get returns ok=false.
	reg := provcore.NewRegistry()
	reg.Freeze()
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "openai", Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "no adapter for format") {
		t.Fatalf("expected no-adapter error, got %v", err)
	}
}

func TestAdapterBackend_NilAdapterResponse(t *testing.T) {
	a := &nilRespAdapter{}
	reg := provcore.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "openai", Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "adapter returned nil") {
		t.Fatalf("expected adapter-nil error, got %v", err)
	}
}

func TestAdapterBackend_UnparseableChatResponse(t *testing.T) {
	a := &fakeAdapter{stubStatus: 200, stubBody: []byte(`{"choices": "not an array"}`)}
	reg := provcore.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "openai", Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestAdapterBackend_EmptyChoices(t *testing.T) {
	a := &fakeAdapter{stubStatus: 200, stubBody: []byte(`{"choices":[]}`)}
	reg := provcore.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	res := &fakeResolver{target: provcore.CallTarget{ProviderName: "openai", Format: provcore.FormatOpenAI}}
	b := &AdapterBackend{Resolver: res, Registry: reg, ProviderID: "p", ModelID: "m"}
	_, err := b.Call(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("expected empty-choices error, got %v", err)
	}
}

type nilRespAdapter struct{}

func (nilRespAdapter) Format() provcore.Format                 { return provcore.FormatOpenAI }
func (nilRespAdapter) SupportsShape(_ typology.WireShape) bool { return true }
func (nilRespAdapter) Execute(_ context.Context, _ provcore.Request) (*provcore.Response, error) {
	return nil, nil
}
func (nilRespAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return &provcore.ProbeResult{OK: true}, nil
}
func (nilRespAdapter) PrepareBody(req provcore.Request) ([]byte, []string, string, error) {
	return req.Body, nil, "", nil
}
func (nilRespAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, _ []string, _ string) (*provcore.Response, error) {
	req.Body = body
	return nilRespAdapter{}.Execute(ctx, req)
}

// Ensure the fakeResolver in backend_provider_test.go satisfies the
// provtarget.Resolver interface at compile time. Catches signature drift.
var _ provtarget.Resolver = (*fakeResolver)(nil)

// ── cache.go ────────────────────────────────────────────────────────

func TestCache_Get_RedisError(t *testing.T) {
	s, rdb := newMiniRedis(t)
	c := NewCache(rdb)
	// Pre-populate then force any subsequent command to fail.
	if err := c.Set(context.Background(), "k", &Response{Decision: "approve"}, time.Minute); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	s.SetError("forced GET failure")
	defer s.SetError("")
	_, hit, err := c.Get(context.Background(), "k")
	if err == nil {
		t.Fatal("expected error when redis fails")
	}
	if hit {
		t.Error("hit must be false on error")
	}
}

func TestCache_Get_UnmarshalError(t *testing.T) {
	s, rdb := newMiniRedis(t)
	c := NewCache(rdb)
	// Bypass our Set() so we can plant garbage that is not JSON.
	if err := s.Set("garbage-key", "}}}not-json"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}
	_, hit, err := c.Get(context.Background(), "garbage-key")
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if hit {
		t.Error("hit must be false on unmarshal error")
	}
}

func TestCache_Set_RedisError(t *testing.T) {
	s, rdb := newMiniRedis(t)
	c := NewCache(rdb)
	s.SetError("SET refused")
	defer s.SetError("")
	if err := c.Set(context.Background(), "k", &Response{Decision: "approve"}, time.Minute); err == nil {
		t.Fatal("expected error when redis SET fails")
	}
}

// ── config_cache.go ─────────────────────────────────────────────────

func TestConfigCache_DoubleCheckHit_AfterContention(t *testing.T) {
	// Two goroutines race on the same expired snapshot. The double-check
	// branch inside the mutex should ensure only one Load happens.
	loader := &stubLoader{out: &configstore.AIGuardConfig{ID: "singleton"}}
	c := NewConfigCache(loader, 1*time.Hour, silentSlog())
	// Warm the cache.
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Invalidate to force expiry; then fire concurrent Get()s.
	c.Invalidate()
	done := make(chan struct{}, 8)
	for range 8 {
		go func() {
			_, _ = c.Get(context.Background())
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	// After Invalidate, exactly ONE additional Load — others see the
	// snapshot via the in-mutex double-check.
	if loader.calls != 2 {
		t.Errorf("loader calls after concurrent miss: got %d, want 2 (initial + 1 collapsed reload)", loader.calls)
	}
}

// ── decoder.go ──────────────────────────────────────────────────────

func TestDecode_StripActionPreserved(t *testing.T) {
	r, err := DecodeJudgeOutput(`{"decision":"modify","redactions":[{"start":1,"end":5,"action":"strip"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Redactions) != 1 || r.Redactions[0].Action != "strip" {
		t.Fatalf("strip action lost: %+v", r.Redactions)
	}
}

func TestDecode_ReplaceActionPreserved(t *testing.T) {
	r, err := DecodeJudgeOutput(`{"decision":"modify","redactions":[{"start":1,"end":5,"action":"replace","replacement":"X"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Redactions) != 1 || r.Redactions[0].Action != "replace" {
		t.Fatalf("replace action lost: %+v", r.Redactions)
	}
}

func TestDecode_SortStableByEndWhenStartEqual(t *testing.T) {
	// Two redactions with identical Start — sort tiebreaker is End ascending.
	raw := `{"decision":"modify","redactions":[
		{"start":5,"end":20,"replacement":"[B]"},
		{"start":5,"end":10,"replacement":"[A]"}
	]}`
	r, err := DecodeJudgeOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Redactions) != 2 {
		t.Fatalf("want 2, got %d", len(r.Redactions))
	}
	if r.Redactions[0].End != 10 || r.Redactions[1].End != 20 {
		t.Errorf("End tiebreaker wrong: %+v", r.Redactions)
	}
}

func TestDecode_NoJSONObjectInRaw(t *testing.T) {
	// "no JSON object" path: no `{` in raw at all.
	_, err := DecodeJudgeOutput("plain prose no braces")
	if err == nil || !strings.Contains(err.Error(), "no JSON object") {
		t.Fatalf("want no-JSON-object error, got %v", err)
	}
}

func TestDecode_UnbalancedBracesYieldEmpty(t *testing.T) {
	// `{` present but never closes — extractJSON returns "", DecodeJudgeOutput
	// reports "no JSON object".
	_, err := DecodeJudgeOutput("prefix { but no close")
	if err == nil || !strings.Contains(err.Error(), "no JSON object") {
		t.Fatalf("want no-JSON-object error for unbalanced braces, got %v", err)
	}
}

func TestDecode_EmptyLabelsArrayPreserved(t *testing.T) {
	// Labels is an array of only whitespace strings → normalizeLabels
	// returns nil (all entries trimmed away).
	r, err := DecodeJudgeOutput(`{"decision":"approve","labels":["   ","\t"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Labels != nil {
		t.Errorf("labels should be nil when all entries trim to empty, got %v", r.Labels)
	}
}

func TestDecode_PrefixProseBeforeJSON(t *testing.T) {
	// Brace-matching fallback when raw has leading prose + balanced JSON.
	r, err := DecodeJudgeOutput(`Here is the result: {"decision":"approve"} thanks.`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Decision != "approve" {
		t.Errorf("brace-match fallback failed: %+v", r)
	}
}

// ── prompt.go ───────────────────────────────────────────────────────

func TestRender_ExecuteErrorOnMissingField(t *testing.T) {
	// {{.DoesNotExist}} compiles but fails at execute time because the
	// data context (RenderInput) has no such field. Exercises the
	// execute-error branch separately from parse-error.
	_, err := Render(`{{.DoesNotExist}}`, RenderInput{DetectorType: "x", Content: "y"})
	if err == nil || !strings.Contains(err.Error(), "execute") {
		t.Fatalf("expected execute error, got %v", err)
	}
}

// (no helpers — tests inline httptest where needed)
