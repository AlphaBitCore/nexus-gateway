// proxy_l2_test.go — unit tests for the proxy_l2.go L2 semantic cache
// helpers (fleet enable gate + canonical message → embedding-input plumbing
// + tryL2Lookup / scheduleL2Write branches with stub reader/writer).
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Stub implementations for L2 interface seams

type stubSemanticReader struct {
	result semantic.ReadResult
	err    error
	called atomic.Int32
}

func (s *stubSemanticReader) Read(_ context.Context, _ semantic.ReadRequest) (semantic.ReadResult, error) {
	s.called.Add(1)
	return s.result, s.err
}

type stubSemanticWriter struct {
	result    semantic.WriteResult
	err       error
	called    atomic.Int32
	writeDone chan struct{}
}

func newStubWriter() *stubSemanticWriter {
	return &stubSemanticWriter{writeDone: make(chan struct{}, 1)}
}

func (s *stubSemanticWriter) Write(_ context.Context, _ semantic.WriteRequest) (semantic.WriteResult, error) {
	s.called.Add(1)
	select {
	case s.writeDone <- struct{}{}:
	default:
	}
	return s.result, s.err
}

func enabledFleetCache() *semantic.ConfigCache {
	cc := semantic.NewConfigCache()
	// Include the JOIN fields the L2 hot path reads — without
	// EmbeddingProviderBaseURL/ModelID the tryL2Lookup precheck returns false
	// before the Reader is invoked.
	cc.Set(semantic.ConfigSnapshot{
		Enabled:                       true,
		EmbeddingProviderID:           "p",
		EmbeddingModelID:              "m",
		EmbeddingDimension:            1536,
		EmbeddingProviderBaseURL:      "https://api.openai.com",
		EmbeddingProviderModelID:      "text-embedding-3-small",
		EmbeddingInputPricePerMillion: 0.02,
	})
	return cc
}

// stubCredManager satisfies the CredentialLookup interface for L2 tests.
// Returns a constant API key so the tryL2Lookup precheck passes and tests
// can reach the Reader.
type stubCredManager struct {
	err error
}

func (s *stubCredManager) GetForProvider(_ context.Context, _ string) (string, string, string, error) {
	if s.err != nil {
		return "", "", "", s.err
	}
	return "sk-test", "cred-id", "cred-name", nil
}

func sampleMsgs() []normcore.Message {
	return []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "Capital of France?"},
		}},
	}
}

func makeTryParams(t *testing.T) l2ReadParams {
	t.Helper()
	return l2ReadParams{
		r:             httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		w:             httptest.NewRecorder(),
		rec:           &audit.Record{},
		routeResult:   &routingcore.RouteResult{},
		logger:        noopLogger(),
		canonicalMsgs: sampleMsgs(),
	}
}

// noopLogger returns a slog.Logger that discards all output.
// Used to avoid log noise in unit tests.
func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestFleetSemanticPolicy_NilCache(t *testing.T) {
	if _, ok := fleetSemanticPolicy(nil); ok {
		t.Errorf("want ok=false when ConfigCache is nil, got true")
	}
}

func TestFleetSemanticPolicy_DisabledFleet(t *testing.T) {
	cc := semantic.NewConfigCache()
	// Default state: Enabled=false, EmbeddingProviderID/ModelID empty → EffectiveEnabled=false.
	if _, ok := fleetSemanticPolicy(cc); ok {
		t.Errorf("want ok=false when fleet not configured, got true")
	}
}

func TestFleetSemanticPolicy_EnabledFleet_Defaults(t *testing.T) {
	cc := semantic.NewConfigCache()
	cc.Set(semantic.ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "p",
		EmbeddingModelID:    "m",
		EmbeddingDimension:  1536,
	})
	pol, ok := fleetSemanticPolicy(cc)
	if !ok {
		t.Fatalf("want ok=true with fleet enabled, got false")
	}
	// 0.96 / "vk" are ConfigCache.Set's normalization defaults for a snapshot
	// that left Threshold zero and VaryBy empty.
	if pol.Threshold != 0.96 {
		t.Errorf("threshold: got %v, want 0.96", pol.Threshold)
	}
	if pol.VaryBy != "vk" {
		t.Errorf("varyBy: got %q, want vk", pol.VaryBy)
	}
	if pol.AllowCrossModel {
		t.Error("allowCrossModel: want false, got true")
	}
}

func TestResolveL2VKScope_DefaultVK(t *testing.T) {
	rec := &audit.Record{VirtualKeyID: "vk-abc"}
	got := resolveL2VKScope(rec, "vk")
	if got != "vk-abc" {
		t.Errorf("vk scope: got %q, want %q", got, "vk-abc")
	}
}

func TestResolveL2VKScope_EmptyVaryBy(t *testing.T) {
	// Any unknown varyBy value falls back to VirtualKeyID.
	rec := &audit.Record{VirtualKeyID: "vk-xyz", UserID: "u-1", OrganizationID: "org-1"}
	got := resolveL2VKScope(rec, "")
	if got != "vk-xyz" {
		t.Errorf("empty varyBy: got %q, want VK %q", got, "vk-xyz")
	}
}

func TestResolveL2VKScope_User(t *testing.T) {
	rec := &audit.Record{UserID: "u-123"}
	got := resolveL2VKScope(rec, "user")
	if got != "u-123" {
		t.Errorf("user scope: got %q, want %q", got, "u-123")
	}
}

func TestResolveL2VKScope_Org(t *testing.T) {
	rec := &audit.Record{OrganizationID: "org-456"}
	got := resolveL2VKScope(rec, "org")
	if got != "org-456" {
		t.Errorf("org scope: got %q, want %q", got, "org-456")
	}
}

func TestResolveL2VKScope_None(t *testing.T) {
	rec := &audit.Record{VirtualKeyID: "vk-abc", UserID: "u-1"}
	got := resolveL2VKScope(rec, "none")
	if got != "" {
		t.Errorf("none scope: got %q, want empty", got)
	}
}

func TestJoinTextBlocksL2_Empty(t *testing.T) {
	got := joinTextBlocksL2(nil)
	if got != "" {
		t.Errorf("nil blocks: got %q, want empty", got)
	}
}

func TestJoinTextBlocksL2_NonTextOnly(t *testing.T) {
	blocks := []normcore.ContentBlock{
		{Type: normcore.ContentImageRef, Text: "ignored"},
	}
	got := joinTextBlocksL2(blocks)
	if got != "" {
		t.Errorf("image-only blocks: got %q, want empty", got)
	}
}

func TestJoinTextBlocksL2_SingleText(t *testing.T) {
	blocks := []normcore.ContentBlock{
		{Type: normcore.ContentText, Text: "hello world"},
	}
	got := joinTextBlocksL2(blocks)
	if got != "hello world" {
		t.Errorf("single text: got %q, want 'hello world'", got)
	}
}

func TestJoinTextBlocksL2_MultipleTextBlocks(t *testing.T) {
	blocks := []normcore.ContentBlock{
		{Type: normcore.ContentText, Text: "part one"},
		{Type: normcore.ContentImageRef, Text: "skip"},
		{Type: normcore.ContentText, Text: "part two"},
	}
	got := joinTextBlocksL2(blocks)
	want := "part one part two"
	if got != want {
		t.Errorf("multi-block: got %q, want %q", got, want)
	}
}

func TestJoinTextBlocksL2_EmptyTextSkipped(t *testing.T) {
	blocks := []normcore.ContentBlock{
		{Type: normcore.ContentText, Text: ""},
		{Type: normcore.ContentText, Text: "real"},
	}
	got := joinTextBlocksL2(blocks)
	if got != "real" {
		t.Errorf("empty text skipped: got %q, want 'real'", got)
	}
}

func TestCanonicalMsgsToInputStaging_Nil(t *testing.T) {
	got := canonicalMsgsToInputStaging(nil)
	if got != nil {
		t.Errorf("nil input: want nil, got %v", got)
	}
}

func TestCanonicalMsgsToInputStaging_AllNonText(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentImageRef, Text: "img"},
		}},
	}
	got := canonicalMsgsToInputStaging(msgs)
	if len(got) != 0 {
		t.Errorf("all-non-text: want empty, got %v", got)
	}
}

func TestCanonicalMsgsToInputStaging_ValidMessages(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "system", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "You are helpful."},
		}},
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "Hello"},
			{Type: normcore.ContentText, Text: "there"},
		}},
	}
	got := canonicalMsgsToInputStaging(msgs)
	if len(got) != 2 {
		t.Fatalf("want 2 staging messages, got %d", len(got))
	}
	if got[0].Role != "system" || got[0].Content != "You are helpful." {
		t.Errorf("msg[0]: got {%q, %q}", got[0].Role, got[0].Content)
	}
	if got[1].Role != "user" || got[1].Content != "Hello there" {
		t.Errorf("msg[1]: got {%q, %q}", got[1].Role, got[1].Content)
	}
}

func TestBuildEmbeddingInput_EmptyMessages(t *testing.T) {
	_, ok := buildEmbeddingInput(nil, inputstaging.StrategySystemPlusLastUser, 0)
	if ok {
		t.Error("want ok=false for nil messages, got true")
	}
}

func TestBuildEmbeddingInput_AllNonText(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentImageRef, Text: "img"},
		}},
	}
	_, ok := buildEmbeddingInput(msgs, inputstaging.StrategySystemPlusLastUser, 0)
	if ok {
		t.Error("want ok=false when all content is non-text, got true")
	}
}

func TestBuildEmbeddingInput_ValidWithInvalidStrategy(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "What is 2+2?"},
		}},
	}
	// Invalid strategy should fall back to StrategySystemPlusLastUser without error.
	text, ok := buildEmbeddingInput(msgs, "invalid_strategy", 0)
	if !ok {
		t.Error("want ok=true even with invalid strategy (fallback to default), got false")
	}
	if text == "" {
		t.Error("want non-empty embedding input, got empty")
	}
}

func TestBuildEmbeddingInput_ValidMessages(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "system", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "You are a helpful assistant."},
		}},
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "What is the capital of France?"},
		}},
	}
	text, ok := buildEmbeddingInput(msgs, inputstaging.StrategySystemPlusLastUser, 0)
	if !ok {
		t.Fatal("want ok=true, got false")
	}
	if text == "" {
		t.Error("want non-empty embedding input text")
	}
}

// normMessagesToFreshness (proxy.go, 0% coverage pre-test)

func TestNormMessagesToFreshness_Nil(t *testing.T) {
	got := normMessagesToFreshness(nil)
	if got != nil {
		t.Errorf("nil input: want nil, got %v", got)
	}
}

func TestNormMessagesToFreshness_AllNonText(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentImageRef, Text: "img"},
		}},
	}
	got := normMessagesToFreshness(msgs)
	if got != nil {
		t.Errorf("all-non-text: want nil (fail-open), got %v", got)
	}
}

func TestNormMessagesToFreshness_SingleText(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "What time is it now?"},
		}},
	}
	got := normMessagesToFreshness(msgs)
	if len(got) != 1 {
		t.Fatalf("want 1 freshness message, got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Errorf("role: got %q, want user", got[0].Role)
	}
	if got[0].Content != "What time is it now?" {
		t.Errorf("content: got %q", got[0].Content)
	}
}

func TestNormMessagesToFreshness_MultipleBlocks(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "user", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "Today's weather"},
			{Type: normcore.ContentImageRef, Text: "skipped"},
			{Type: normcore.ContentText, Text: "in Paris?"},
		}},
	}
	got := normMessagesToFreshness(msgs)
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d", len(got))
	}
	want := "Today's weather in Paris?"
	if got[0].Content != want {
		t.Errorf("content: got %q, want %q", got[0].Content, want)
	}
}

// tryL2Lookup — branch matrix with fleet-gated policy

func TestTryL2Lookup_NilReader(t *testing.T) {
	h := &Handler{deps: &Deps{SemanticConfigCache: enabledFleetCache()}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false when SemanticReader nil")
	}
}

// TestTryL2Lookup_NilCredManager covers the defensive guard at the read
// path's credential lookup: SemanticReader is wired and the fleet policy
// resolves, but CredManager is absent (boot-time wiring would always
// supply one; this guards hand-constructed Handler test doubles). Must
// return false without invoking the reader.
func TestTryL2Lookup_NilCredManager(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache()}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false when CredManager nil")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader must not be called when CredManager nil")
	}
}

func TestTryL2Lookup_FleetDisabled(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: semantic.NewConfigCache()}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false when fleet config disabled")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader should not be called when fleet disabled")
	}
}

func TestTryL2Lookup_NoCanonicalMsgs(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	p.canonicalMsgs = nil
	if h.tryL2Lookup(p) {
		t.Error("want false when no canonical messages")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader should not be called on empty embedding input")
	}
	if p.rec.GatewayCacheSkipReason != audit.GatewayCacheSkipReasonOversizeForEmbedding {
		t.Errorf("skip reason: got %q, want %q",
			p.rec.GatewayCacheSkipReason, audit.GatewayCacheSkipReasonOversizeForEmbedding)
	}
}

func TestTryL2Lookup_ReaderError(t *testing.T) {
	rdr := &stubSemanticReader{err: errors.New("read failed")}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false on reader error")
	}
}

func TestTryL2Lookup_ReaderMiss(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{Outcome: "miss"}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	if h.tryL2Lookup(makeTryParams(t)) {
		t.Error("want false on reader miss")
	}
}

func TestTryL2Lookup_ReaderSkip_StampsReason(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		SkipReason: audit.GatewayCacheSkipReasonEmbeddingTimeout,
	}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	if h.tryL2Lookup(p) {
		t.Error("want false on reader skip")
	}
	if p.rec.GatewayCacheSkipReason != audit.GatewayCacheSkipReasonEmbeddingTimeout {
		t.Errorf("skip reason not propagated: got %q", p.rec.GatewayCacheSkipReason)
	}
}

func TestTryL2Lookup_HitStream_ConversionError(t *testing.T) {
	// Stream HIT with malformed chunk array → ToCacheStreamEntry returns error
	// → handler resets stamps and returns false so broker dispatch can retry.
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		Entry: &semantic.Entry{
			ResponseBody: []byte(`{not a valid chunk array}`),
			EntryKey:     "nexus:semantic-cache:v1:1234567890abcdef",
		},
	}}
	h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	p := makeTryParams(t)
	p.isStream = true
	if h.tryL2Lookup(p) {
		t.Error("want false on stream conversion error")
	}
	if p.rec.GatewayCacheStatus != "" {
		t.Errorf("status should be reset; got %q", p.rec.GatewayCacheStatus)
	}
	if p.rec.GatewayCacheKind != "" {
		t.Errorf("kind should be reset; got %q", p.rec.GatewayCacheKind)
	}
	// GatewayCacheL2EntryKey must also be reset on the stream-conversion
	// fallback path so the broker re-stamps it cleanly (otherwise the
	// failing partial stamp would leak into the audit row).
	if p.rec.GatewayCacheL2EntryKey != "" {
		t.Errorf("L2 entry key should be reset; got %q", p.rec.GatewayCacheL2EntryKey)
	}
}

// scheduleL2Write — branch matrix

func TestScheduleL2Write_NilWriter(t *testing.T) {
	h := &Handler{deps: &Deps{SemanticConfigCache: enabledFleetCache()}}
	// No panic, no goroutine — just early return.
	h.scheduleL2Write(&routingcore.RouteResult{}, routingcore.RoutingTarget{},
		sampleMsgs(), []byte(`{}`), nil, "vk", false, Ingress{}, noopLogger())
}

func TestScheduleL2Write_EmptyBody(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{SemanticWriter: w, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	h.scheduleL2Write(&routingcore.RouteResult{}, routingcore.RoutingTarget{},
		sampleMsgs(), nil, nil, "vk", false, Ingress{}, noopLogger())
	if w.called.Load() != 0 {
		t.Error("writer should not be called with empty body")
	}
}

func TestScheduleL2Write_IsStream(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{SemanticWriter: w, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	h.scheduleL2Write(&routingcore.RouteResult{}, routingcore.RoutingTarget{},
		sampleMsgs(), []byte(`{}`), nil, "vk", true, Ingress{}, noopLogger())
	if w.called.Load() != 0 {
		t.Error("writer should not be called for stream responses")
	}
}

func TestScheduleL2Write_FleetDisabled(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{SemanticWriter: w, SemanticConfigCache: semantic.NewConfigCache()}}
	h.scheduleL2Write(&routingcore.RouteResult{}, routingcore.RoutingTarget{},
		sampleMsgs(), []byte(`{}`), nil, "vk", false, Ingress{}, noopLogger())
	if w.called.Load() != 0 {
		t.Error("writer should not be called when fleet disabled")
	}
}

func TestScheduleL2Write_NoTextMsgs(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{SemanticWriter: w, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	h.scheduleL2Write(&routingcore.RouteResult{}, routingcore.RoutingTarget{},
		nil, []byte(`{}`), nil, "vk", false, Ingress{}, noopLogger())
	if w.called.Load() != 0 {
		t.Error("writer should not be called when no text content")
	}
}

func TestScheduleL2Write_GoroutineLogsOnError(t *testing.T) {
	w := newStubWriter()
	w.err = errors.New("simulated valkey timeout")
	h := &Handler{deps: &Deps{SemanticWriter: w, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	h.scheduleL2Write(
		&routingcore.RouteResult{},
		routingcore.RoutingTarget{ProviderID: "openai", ProviderModelID: "gpt-4o-mini"},
		sampleMsgs(),
		[]byte(`{"id":"r"}`),
		nil, "vk-scope", false, Ingress{}, noopLogger(),
	)
	select {
	case <-w.writeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("writer goroutine did not fire within 3 seconds")
	}
	if w.called.Load() != 1 {
		t.Errorf("expected writer.Write called once; got %d", w.called.Load())
	}
}

func TestScheduleL2Write_FiresGoroutine(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{SemanticWriter: w, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
	h.scheduleL2Write(
		&routingcore.RouteResult{},
		routingcore.RoutingTarget{ProviderID: "openai", ProviderModelID: "gpt-4o-mini"},
		sampleMsgs(),
		[]byte(`{"id":"r","choices":[{"message":{"content":"Paris."}}]}`),
		nil, "vk-scope", false, Ingress{}, noopLogger(),
	)
	select {
	case <-w.writeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("writer goroutine did not fire within 3 seconds")
	}
	if w.called.Load() != 1 {
		t.Errorf("expected writer.Write called once; got %d", w.called.Load())
	}
}
