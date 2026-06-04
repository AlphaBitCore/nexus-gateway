// coverage_boost_test.go — small, additive unit tests that lift
// per-helper coverage on the 88.6% → ≥95% push without requiring the
// full proxy harness. Each test pins a specific uncovered branch
// surfaced by `go tool cover -func=h.cov` (early-return arms, nil-input
// guards, fallback defaults, error-arm logging).
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	vkauthpkg "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// routing_audit_trace.go — buildRoutingAuditTrace nil + empty paths

func TestBuildRoutingAuditTrace_NilResult(t *testing.T) {
	if got := buildRoutingAuditTrace(nil); got != nil {
		t.Fatalf("nil RouteResult must yield nil; got %+v", got)
	}
}

func TestBuildRoutingAuditTrace_AllEmpty(t *testing.T) {
	// All three triggers empty → nil so the audit row stays NULL.
	if got := buildRoutingAuditTrace(&routingcore.RouteResult{}); got != nil {
		t.Fatalf("empty RouteResult must yield nil; got %+v", got)
	}
}

func TestBuildRoutingAuditTrace_SplitsRecovery(t *testing.T) {
	r := &routingcore.RouteResult{
		Targets: []routingcore.RoutingTarget{
			{ProviderID: "p1", Source: "primary", ProviderName: "openai"},
			{ProviderID: "p2", Source: "fallback", ProviderName: "openai"},
			{ProviderID: "p3", Source: "recovery", ProviderName: "anthropic"},
		},
		Substituted:     true,
		OriginalModelID: "auto",
	}
	got := buildRoutingAuditTrace(r)
	if got == nil {
		t.Fatal("non-empty Targets must produce a trace")
	}
	if len(got.Targets) != 2 || len(got.RecoveryTargets) != 1 {
		t.Errorf("expected 2 targets + 1 recovery; got targets=%d recovery=%d",
			len(got.Targets), len(got.RecoveryTargets))
	}
	if got.RecoveryTargets[0].ProviderID != "p3" {
		t.Errorf("recovery slot mis-routed: %+v", got.RecoveryTargets)
	}
	if !got.Substituted || got.OriginalModelID != "auto" {
		t.Errorf("Substituted / OriginalModelID not propagated: %+v", got)
	}
}

// Note: normalizeIngressBodyFormat, projectTargets tests have moved to
// packages/ai-gateway/internal/ingress/debug (they test debug-package internals).

// cross_format.go — schemaMode bridge-non-nil paths

func TestSchemaMode_BridgeRejectedPath(t *testing.T) {
	fb := &fakeBridge{
		endpointRoutable: func(_ typology.WireShape, _, _ provcore.Format) bool { return false },
	}
	if got := schemaMode(provcore.FormatOpenAI, provcore.FormatAnthropic, typology.WireShapeOpenAIChat, fb); got != "rejected" {
		t.Errorf("schemaMode bridge rejected arm=%q want rejected", got)
	}
}

func TestSchemaMode_BridgePassthroughPath(t *testing.T) {
	fb := &fakeBridge{
		endpointRoutable: func(_ typology.WireShape, _, _ provcore.Format) bool { return true },
	}
	if got := schemaMode(provcore.FormatOpenAI, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, fb); got != "passthrough" {
		t.Errorf("schemaMode bridge same-format=%q want passthrough", got)
	}
}

func TestSchemaMode_BridgeTranslatedPath(t *testing.T) {
	fb := &fakeBridge{
		endpointRoutable: func(_ typology.WireShape, _, _ provcore.Format) bool { return true },
	}
	if got := schemaMode(provcore.FormatAnthropic, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, fb); got != "translated" {
		t.Errorf("schemaMode bridge cross-format=%q want translated", got)
	}
}

// Sanity: bridge==nil falls through to the legacy matrix; covered by
// existing tests, exercised here as a regression guard.
func TestSchemaMode_BridgeNil_LegacyDispatch(t *testing.T) {
	if got := schemaMode(provcore.FormatOpenAI, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, nil); got != "passthrough" {
		t.Errorf("legacy same-format=%q want passthrough", got)
	}
	if got := schemaMode(provcore.FormatOpenAI, provcore.FormatAnthropic, typology.WireShapeOpenAIChat, nil); got != "translated" {
		t.Errorf("legacy openai→x=%q want translated", got)
	}
	if got := schemaMode(provcore.FormatAnthropic, provcore.FormatGemini, typology.WireShapeOpenAIChat, nil); got != "rejected" {
		t.Errorf("legacy x→y=%q want rejected", got)
	}
}

// Note: buildHookTestInput, runHook tests have moved to
// packages/ai-gateway/internal/ingress/debug (they test debug-package internals).

// proxy_cache.go — copyUpstreamHeaders nil / empty + non-empty deep copy

func TestCopyUpstreamHeaders_AllArms(t *testing.T) {
	if got := copyUpstreamHeaders(nil); got != nil {
		t.Errorf("nil input must yield nil; got %v", got)
	}
	if got := copyUpstreamHeaders(make(map[string][]string)); got != nil {
		t.Errorf("empty input must yield nil; got %v", got)
	}
	src := map[string][]string{
		"Content-Type": {"application/json"},
		"X-Multi":      {"a", "b"},
	}
	got := copyUpstreamHeaders(src)
	if len(got) != 2 || got["Content-Type"][0] != "application/json" {
		t.Fatalf("copy lost data: %+v", got)
	}
	// Mutating the copy must NOT alter the source — verifies deep copy.
	got["X-Multi"][0] = "z"
	if src["X-Multi"][0] != "a" {
		t.Errorf("mutation leaked into src: %+v", src)
	}
}

// proxy_cache.go — joinCSV all arms

func TestJoinCSV_AllArms(t *testing.T) {
	if got := joinCSV(nil); got != "" {
		t.Errorf("nil=%q want empty", got)
	}
	if got := joinCSV([]string{}); got != "" {
		t.Errorf("empty=%q want empty", got)
	}
	if got := joinCSV([]string{"a"}); got != "a" {
		t.Errorf("single=%q want a", got)
	}
	if got := joinCSV([]string{"a", "b", "c"}); got != "a,b,c" {
		t.Errorf("triple=%q want a,b,c", got)
	}
}

// proxy_cache.go — chunkUsageHolder.record nil arms

func TestChunkUsageHolder_NilReceiverAndNilUsage(t *testing.T) {
	// Both nil-receiver and nil-usage early returns are uncovered.
	var nilHolder *chunkUsageHolder
	nilHolder.record(&provcore.Usage{}) // must not panic
	h := &chunkUsageHolder{}
	h.record(nil) // must not panic, no-op
	if h.usage.Load() != nil {
		t.Errorf("usage holder must remain empty after nil record; got %+v", h.usage.Load())
	}
}

// proxy_cache.go — chunkSSEReader nil sub + buffered-leftover arms

func TestChunkSSEReader_NilSubReturnsEOF(t *testing.T) {
	// nil sub triggers the "closed = true; return 0, io.EOF" arm.
	rd := newChunkSSEReaderFromSubscription(context.Background(), nil, nil, provcore.FormatOpenAI)
	buf := make([]byte, 16)
	n, err := rd.Read(buf)
	if n != 0 || err == nil {
		t.Fatalf("nil sub want (0, EOF); got n=%d err=%v", n, err)
	}
}

func TestChunkSSEReader_ClosedAfterErrorReturnsErr(t *testing.T) {
	rd := &chunkSSEReader{
		ctx:    context.Background(),
		closed: true,
		err:    errors.New("prior failure"),
	}
	n, err := rd.Read(make([]byte, 8))
	if n != 0 || err == nil || !strings.Contains(err.Error(), "prior failure") {
		t.Fatalf("closed+err arm want (0, prior failure); got n=%d err=%v", n, err)
	}
}

func TestChunkSSEReader_DrainsBufferedBytesFirst(t *testing.T) {
	rd := &chunkSSEReader{
		ctx: context.Background(),
		buf: []byte("hello"),
	}
	out := make([]byte, 3)
	n, err := rd.Read(out)
	if err != nil || n != 3 || !bytes.Equal(out[:n], []byte("hel")) {
		t.Fatalf("partial drain want (3,hel,nil); got n=%d err=%v out=%q", n, err, out[:n])
	}
	// remainder still in buf
	out2 := make([]byte, 8)
	n2, err2 := rd.Read(out2)
	if err2 != nil || n2 != 2 || !bytes.Equal(out2[:n2], []byte("lo")) {
		t.Fatalf("remainder drain want (2,lo,nil); got n=%d err=%v out=%q", n2, err2, out2[:n2])
	}
}

// Note: runHook large-timeout regression test moved to ingress/debug package.

// chunkSSEReader — provider error → synthesised SSE error frame

// boostFakeChunkSub is a tiny ChunkSubscription that returns a queued
// sequence of (chunk, err) pairs from Next.
type boostFakeChunkSub struct {
	entries []boostFakeChunkEntry
	idx     int
}

type boostFakeChunkEntry struct {
	chunk provcore.Chunk
	err   error
}

func (f *boostFakeChunkSub) Next(_ context.Context) (provcore.Chunk, error) {
	if f.idx >= len(f.entries) {
		return provcore.Chunk{}, io.EOF
	}
	e := f.entries[f.idx]
	f.idx++
	return e.chunk, e.err
}

func (f *boostFakeChunkSub) Close() error { return nil }

func TestChunkSSEReader_ProviderErrorEmitsSSEFrame(t *testing.T) {
	pErr := errors.New("upstream disconnected")
	sub := &boostFakeChunkSub{entries: []boostFakeChunkEntry{{err: pErr}}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	rd.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 4096)
	n, err := rd.Read(buf)
	if err != nil {
		t.Fatalf("first Read err=%v want nil (synthesised frame)", err)
	}
	frame := string(buf[:n])
	if !strings.Contains(frame, "data:") {
		t.Errorf("synthesised frame missing SSE prefix; got %q", frame)
	}
	if !strings.Contains(frame, "upstream disconnected") {
		t.Errorf("synthesised frame missing error message; got %q", frame)
	}
}

func TestChunkSSEReader_DoneChunkWithRawBytes(t *testing.T) {
	terminal := []byte("data: [DONE]\n\n")
	sub := &boostFakeChunkSub{entries: []boostFakeChunkEntry{
		{chunk: provcore.Chunk{Done: true, RawBytes: terminal}},
	}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	rd.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 64)
	n, err := rd.Read(buf)
	if err != nil {
		t.Fatalf("Read err=%v", err)
	}
	if !strings.Contains(string(buf[:n]), "[DONE]") {
		t.Errorf("Done chunk's RawBytes not forwarded; got %q", string(buf[:n]))
	}
	// next read should yield EOF
	_, err2 := rd.Read(buf)
	if err2 == nil || !strings.Contains(err2.Error(), "EOF") {
		t.Errorf("second Read after Done want EOF; got %v", err2)
	}
}

func TestChunkSSEReader_DeltaSynthesisedOpenAIEnvelope(t *testing.T) {
	sub := &boostFakeChunkSub{entries: []boostFakeChunkEntry{
		{chunk: provcore.Chunk{Delta: "hello "}},
	}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	rd.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 256)
	n, err := rd.Read(buf)
	if err != nil {
		t.Fatalf("Read err=%v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, `"content":"hello "`) || !strings.Contains(got, `"delta"`) {
		t.Errorf("synthesised envelope missing expected fields; got %q", got)
	}
}

func TestChunkSSEReader_DefaultArmYieldsZero(t *testing.T) {
	// Empty chunk (no Delta, no RawBytes, no Done) lands in the
	// `default: return 0, nil` arm.
	sub := &boostFakeChunkSub{entries: []boostFakeChunkEntry{
		{chunk: provcore.Chunk{}},
	}}
	rd := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	rd.usageSink = &chunkUsageHolder{}

	buf := make([]byte, 16)
	n, err := rd.Read(buf)
	if err != nil || n != 0 {
		t.Errorf("empty chunk want (0, nil); got (n=%d err=%v)", n, err)
	}
}

// chunkUsageHolder — multi-event accumulation + total recompute

func TestChunkUsageHolder_RecomputesTotalFromParts(t *testing.T) {
	h := &chunkUsageHolder{}
	prompt, completion := 5, 7
	// First event: prompt only.
	h.record(&provcore.Usage{PromptTokens: &prompt})
	// Second event: add completion. Total should be recomputed = 12.
	h.record(&provcore.Usage{CompletionTokens: &completion})
	snap := h.snapshot()
	if snap.TotalTokens == nil || *snap.TotalTokens != 12 {
		t.Errorf("recomputed total want 12; got %v", snap.TotalTokens)
	}
}

func TestChunkUsageHolder_ExplicitTotalWins(t *testing.T) {
	h := &chunkUsageHolder{}
	total := 99
	h.record(&provcore.Usage{TotalTokens: &total})
	snap := h.snapshot()
	if snap.TotalTokens == nil || *snap.TotalTokens != 99 {
		t.Errorf("explicit total want 99; got %v", snap.TotalTokens)
	}
}

func TestChunkUsageHolder_SnapshotOnNilReceiverReturnsZero(t *testing.T) {
	var h *chunkUsageHolder
	snap := h.snapshot()
	if snap.PromptTokens != nil || snap.CompletionTokens != nil || snap.TotalTokens != nil {
		t.Errorf("nil-receiver snapshot must yield zero usage; got %+v", snap)
	}
}

// directStreamSubscription — nil session + Close-then-Next + Close idempotent

func TestDirectStreamSubscription_NilSession(t *testing.T) {
	sub := newDirectStreamSubscription(nil)
	_, err := sub.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Errorf("nil session want EOF; got %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Errorf("Close on nil session want nil; got %v", err)
	}
	// Close again is a no-op
	if err := sub.Close(); err != nil {
		t.Errorf("repeated Close want nil; got %v", err)
	}
}

func TestDirectStreamSubscription_NextAfterClose(t *testing.T) {
	sub := newDirectStreamSubscription(nil)
	_ = sub.Close()
	_, err := sub.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Errorf("Next after Close want EOF; got %v", err)
	}
}

// estimate.go — ServeEstimate non-POST + invalid reasoning effort

func TestServeEstimate_GETMethodRejected(t *testing.T) {
	h := NewHandler(&Deps{})
	r, _ := http.NewRequest(http.MethodGet, "/v1/estimate", nil)
	w := httptest.NewRecorder()
	h.ServeEstimate(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", w.Code)
	}
	if !strings.Contains(w.Body.String(), "estimate_method_not_allowed") {
		t.Errorf("body=%s want code", w.Body.String())
	}
}

// estimateStubAuth returns a fake VK with a per-VK compare-endpoint
// rate-limit ceiling so checkCompareRateLimit can be driven into the
// reject arm.
type estimateStubAuth struct {
	rpm int
}

func (s *estimateStubAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauthpkg.VKMeta, error) {
	rpm := s.rpm
	return &vkauthpkg.VKMeta{
		ID:                          "vk-1",
		Name:                        "limited",
		CompareEndpointRateLimitRpm: &rpm,
	}, nil
}

// denyAllRateLimiter rejects every Allow() call so the
// checkCompareRateLimit reject arm fires.
type denyAllRateLimiter struct{}

func (denyAllRateLimiter) Allow(_ string, _ int, _ int64) (bool, int) {
	return false, 30
}

func TestServeEstimate_CompareRateLimited(t *testing.T) {
	h := NewHandler(&Deps{
		VKAuth:      &estimateStubAuth{rpm: 10},
		RateLimiter: denyAllRateLimiter{},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/estimate",
		strings.NewReader(`{"request":{"model":"x"},"compareTargets":[{"providerId":"p","modelId":"m"}]}`))
	w := httptest.NewRecorder()
	h.ServeEstimate(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "estimate_compare_rate_limited") {
		t.Errorf("body=%s want code", w.Body.String())
	}
}

func TestServeEstimate_InvalidReasoningEffort(t *testing.T) {
	bad := "garbage"
	h := NewHandler(&Deps{VKAuth: &estimateStubAuth{}})
	r := httptest.NewRequest(http.MethodPost, "/v1/estimate",
		strings.NewReader(`{
			"request":{"model":"x"},
			"compareTargets":[
				{"providerId":"p","modelId":"m","reasoningEffort":"`+bad+`"}
			]
		}`))
	w := httptest.NewRecorder()
	h.ServeEstimate(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "estimate_invalid_reasoning_effort") {
		t.Errorf("body=%s want code", w.Body.String())
	}
}

// TestServeEstimate_IngressFormatOverride drives the
// `req.Options.IngressFormat != nil` arm at line 189.
func TestServeEstimate_IngressFormatOverride(t *testing.T) {
	h := NewHandler(&Deps{
		VKAuth:  &estimateStubAuth{},
		Models:  &fakeModelsAndPricing{},
		Metrics: estimateNoopMetrics{},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/estimate",
		strings.NewReader(`{
			"request":{"model":"x"},
			"compareTargets":[{"providerId":"p","modelId":"unknown"}],
			"options":{"ingressFormat":"anthropic"}
		}`))
	w := httptest.NewRecorder()
	h.ServeEstimate(w, r)
	// Per-target error since model unknown, but the response is still 200 with
	// per-target error. The point of this test is to drive the ingress
	// override arm.
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// estimateNoopMetrics fills the MetricsRecorder interface as a no-op
// so the `h.deps.Metrics != nil` arms in estimate.go fire.
type estimateNoopMetrics struct{}

func (estimateNoopMetrics) RecordRequest(_, _, _ string, _ int, _ time.Duration, _ metricspkg.Usage) {
}
func (estimateNoopMetrics) RecordHookRequest(_, _, _ string)                       {}
func (estimateNoopMetrics) RecordTrafficExtract(_, _, _ string)                    {}
func (estimateNoopMetrics) RecordEstimate(_, _, _ string, _ time.Duration)         {}
func (estimateNoopMetrics) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}

// TestRunEstimateOnce_AllowedModelsReject drives the K1 per-target
// VK allowedModels enforcement arm. The VK's AllowedModels does NOT
// include the requested model → per-target error.
// Note: HooksTestHandler tests moved to packages/ai-gateway/internal/ingress/debug.

func TestRunEstimateOnce_AllowedModelsReject(t *testing.T) {
	h := NewHandler(&Deps{
		Models: &fakeModelsAndPricing{
			byCode: map[string]*store.Model{
				"gpt-4o": {ID: "m1", Code: "gpt-4o", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o"},
			},
		},
	})
	vk := &vkauthpkg.VKMeta{
		ID: "vk-1", Name: "restricted",
		AllowedModels: []store.AllowedModelRef{{ProviderID: "different-provider", ModelID: "only-this-one"}},
	}
	out := h.runEstimateOnce(context.Background(), []byte(`{}`),
		EstimateCompareTarget{ModelID: "gpt-4o"}, vk)
	if out.Error == nil || out.Error.Code != "vk_model_not_allowed" {
		t.Errorf("want vk_model_not_allowed; got %+v", out.Error)
	}
}

// isValidReasoningEffort — direct table-driven tests

// estimate.go — resolveTargetModel + runEstimateOnce arms

// fakeModelsAndPricing wraps the existing stubModels with optional
// pricing reads so resolveTargetModel + runEstimateOnce's allowedModels
// + metrics-recording arms can be exercised.
type fakeModelsAndPricing struct {
	byCode map[string]*store.Model
	byID   map[string]*store.Model
}

func (f *fakeModelsAndPricing) GetModel(_ context.Context, id string) (*store.Model, error) {
	if m, ok := f.byID[id]; ok {
		return m, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeModelsAndPricing) GetModelByCode(_ context.Context, code string) (*store.Model, error) {
	if m, ok := f.byCode[code]; ok {
		return m, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeModelsAndPricing) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return nil, nil
}
func (f *fakeModelsAndPricing) FetchModelPricing(_ context.Context, _ []string) ([]store.ModelPricing, error) {
	return nil, nil
}

func TestResolveTargetModel_NilDeps(t *testing.T) {
	h := &Handler{}
	_, ok := h.resolveTargetModel(context.Background(), EstimateCompareTarget{ModelID: "x"})
	if ok {
		t.Error("nil Deps must return ok=false")
	}
}

func TestResolveTargetModel_NilModels(t *testing.T) {
	h := NewHandler(&Deps{})
	_, ok := h.resolveTargetModel(context.Background(), EstimateCompareTarget{ModelID: "x"})
	if ok {
		t.Error("nil Models must return ok=false")
	}
}

func TestResolveTargetModel_ByCode(t *testing.T) {
	h := NewHandler(&Deps{Models: &fakeModelsAndPricing{
		byCode: map[string]*store.Model{
			"gpt-4o": {ID: "m1", Code: "gpt-4o", ProviderName: "openai"},
		},
	}})
	m, ok := h.resolveTargetModel(context.Background(), EstimateCompareTarget{ModelID: "gpt-4o"})
	if !ok || m.Code != "gpt-4o" {
		t.Errorf("by-code lookup failed; m=%+v ok=%v", m, ok)
	}
}

func TestResolveTargetModel_ByIDFallback(t *testing.T) {
	// byCode lookup misses; byID lookup wins. Exercises the fallthrough arm.
	h := NewHandler(&Deps{Models: &fakeModelsAndPricing{
		byCode: map[string]*store.Model{}, // empty so byCode fails
		byID: map[string]*store.Model{
			"id-1": {ID: "id-1", Code: "gpt-4o", ProviderName: "openai"},
		},
	}})
	m, ok := h.resolveTargetModel(context.Background(), EstimateCompareTarget{ModelID: "id-1"})
	if !ok || m.ID != "id-1" {
		t.Errorf("by-id fallback failed; m=%+v ok=%v", m, ok)
	}
}

func TestResolveTargetModel_NotFound(t *testing.T) {
	h := NewHandler(&Deps{Models: &fakeModelsAndPricing{}})
	_, ok := h.resolveTargetModel(context.Background(), EstimateCompareTarget{ModelID: "nope"})
	if ok {
		t.Error("missing model must return ok=false")
	}
}

// ingress_model.go — ExtractIngressModel error arms

func TestExtractIngressModel_GeminiBadSegment(t *testing.T) {
	// Path segment that doesn't end with the expected Gemini suffix.
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	r.SetPathValue("model", "gemini-pro:unknownOp")
	_, _, err := ExtractIngressModel(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatGemini,
	}, r, nil)
	if err == nil {
		t.Fatal("unknown gemini suffix must error")
	}
}

func TestExtractIngressModel_GeminiEmptyModelID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	r.SetPathValue("model", ":generateContent") // model prefix is empty
	_, _, err := ExtractIngressModel(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatGemini,
	}, r, nil)
	if err == nil {
		t.Fatal("empty model id must error")
	}
}

func TestExtractIngressModel_AzureMissingDeployment(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	_, _, err := ExtractIngressModel(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAzureOpenAI,
	}, r, nil)
	if err == nil {
		t.Fatal("missing deployment must error")
	}
}

func TestExtractIngressModel_UnsupportedBedrock(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	_, _, err := ExtractIngressModel(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatBedrock,
	}, r, nil)
	if err == nil {
		t.Fatal("bedrock ingress must be rejected")
	}
}

// proxy_hook_rewrite.go — collectRuleIDs empty-SourceID + dedup arms

func TestCollectRuleIDs_EmptyInput(t *testing.T) {
	if got := collectRuleIDs(nil); got != nil {
		t.Errorf("nil input must yield nil; got %v", got)
	}
}

func TestCollectRuleIDs_SkipsEmptyAndDedupes(t *testing.T) {
	// Mix of empty SourceID, duplicates, and unique entries.
	spans := []hookTestSpan{
		{ID: "a"},
		{ID: ""}, // skipped
		{ID: "b"},
		{ID: "a"}, // deduped
		{ID: "c"},
	}
	got := collectRuleIDs(toTransformSpans(spans))
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

type hookTestSpan struct{ ID string }

func toTransformSpans(in []hookTestSpan) []normalize.TransformSpan {
	out := make([]normalize.TransformSpan, len(in))
	for i, s := range in {
		out[i] = normalize.TransformSpan{SourceID: s.ID}
	}
	return out
}

// contentBlocksToNormalized — non-text blocks filtered, text blocks kept

func TestContentBlocksToNormalized_FiltersNonText(t *testing.T) {
	blocks := []hooks.ContentBlock{
		{Role: "user", Type: "text", Text: "hi"},
		{Role: "user", Type: "image_url", Text: "data:image/png..."},
		{Role: "assistant", Type: "", Text: "implicit text"},
		{Role: "assistant", Type: "tool_calls", Text: ""},
	}
	got := contentBlocksToNormalized(blocks)
	if len(got.Segments) != 2 {
		t.Fatalf("segments=%v want 2 (text + implicit text)", got.Segments)
	}
	if got.Segments[0] != "hi" || got.Segments[1] != "implicit text" {
		t.Errorf("segments=%v want [hi, implicit text]", got.Segments)
	}
}

// Note: buildDailyResponse tests moved to packages/ai-gateway/internal/ingress/envelope.

func TestExtractIngressModel_UnknownFormat(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	_, _, err := ExtractIngressModel(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.Format("xyzzy"),
	}, r, nil)
	if err == nil {
		t.Fatal("unknown format must error")
	}
}

// estimate.go — buildEstimateSummary corner cases

func TestBuildEstimateSummary_AllErrors(t *testing.T) {
	in := []EstimatePerTarget{
		{Error: &EstimateTargetError{Code: "x", Message: "boom"}},
		{Error: &EstimateTargetError{Code: "y", Message: "boom"}},
	}
	got := buildEstimateSummary(in)
	if got.SuccessCount != 0 || got.ErrorsCount != 2 {
		t.Errorf("all-error summary wrong: %+v", got)
	}
	if got.CheapestExpectedTarget != nil {
		t.Errorf("no cheapest when all errored: %+v", got.CheapestExpectedTarget)
	}
}

func TestIsValidReasoningEffort(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"minimal", true},
		{"low", true},
		{"medium", true},
		{"high", true},
		{"MEDIUM", true}, // case-fold
		{"100", true},
		{"0", false},   // budget must be > 0
		{"-5", false},  // negative
		{"abc", false}, // bogus
		{"", true},     // empty allowed (treated as default)
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := isValidReasoningEffort(tc.in); got != tc.want {
				t.Errorf("isValidReasoningEffort(%q)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
}
