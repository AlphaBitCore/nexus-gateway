package core

// coverage_core_test.go pins observable behavior of every core sub-file from
// inside package core (white-box), ensuring per-package coverage ≥95%.
// Tests that also appear in codecs/coverage_gaps_test.go use a different
// namespace or stub ID to avoid prometheus registration collisions.
//
// Sections mirror the source files:
//   - types.go        — Kind.IsHTTP / Kind.IsAI / Direction constants / Message.MarshalJSON
//   - metrics.go      — NewMetrics register+cache + MustRegisterPrometheus
//   - auditbridge.go  — BuildAuditFn arms + StripContentTypeParams + stripContentTypeParams
//   - registry.go     — All / Replace / SetConfidenceThreshold / RegisterTier2 / MaybeGunzip / Normalize tiers
//   - apply_spans.go  — ApplySpans / clonePayload / parseInt / resolveTextRef / mapEntryRef
//   - projection.go   — TextProjection / TextProjectionWith / JoinedText
//   - confidence.go   — scoreTier1Confidence

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/prometheus/client_golang/prometheus"
)

// coreStub is a minimal Normalizer for use within the core package tests.
// It is a separate type from the one in registry_test.go to avoid redeclaration.
type coreStub struct {
	id      string
	payload NormalizedPayload
	err     error
}

func (s *coreStub) ID() string { return s.id }
func (s *coreStub) Normalize(_ context.Context, _ []byte, _ Meta) (NormalizedPayload, error) {
	return s.payload, s.err
}

func TestCoreKind_IsHTTP_AllArms(t *testing.T) {
	cases := map[Kind]bool{
		KindHTTPJSON:      true,
		KindHTTPText:      true,
		KindHTTPForm:      true,
		KindHTTPMultipart: true,
		KindHTTPSSE:       true,
		KindHTTPBinary:    true,
		KindAIChat:        false,
		KindAIEmbedding:   false,
		KindUnsupported:   false,
	}
	for k, want := range cases {
		if got := k.IsHTTP(); got != want {
			t.Errorf("Kind(%q).IsHTTP() = %v want %v", k, got, want)
		}
	}
}

// TestSSEFrame_JSONRoundTrip pins the frozen wire contract the CP-UI
// codes against: an SSEFrame with decoded JSON data serialises Event +
// Data (camelCase keys, no dataText), a verbatim frame serialises
// Event + DataText (no data), and both survive an unmarshal round-trip
// inside an HTTPBodyView with sseFrames / sseTruncated tags.
func TestSSEFrame_JSONRoundTrip(t *testing.T) {
	view := HTTPBodyView{
		SSEFrames: []SSEFrame{
			{Event: "foo", Data: map[string]any{"a": float64(1)}},
			{Event: "bar", DataText: "not-json-text"},
		},
		SSETruncated: true,
	}
	wire, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sseFrames":[{"event":"foo","data":{"a":1}},{"event":"bar","dataText":"not-json-text"}],"sseTruncated":true}`
	if string(wire) != want {
		t.Fatalf("wire shape drifted:\n got %s\nwant %s", wire, want)
	}
	var back HTTPBodyView
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.SSEFrames) != 2 || !back.SSETruncated {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
	if back.SSEFrames[0].Event != "foo" || back.SSEFrames[0].DataText != "" {
		t.Fatalf("JSON-variant frame wrong after round-trip: %+v", back.SSEFrames[0])
	}
	m, ok := back.SSEFrames[0].Data.(map[string]any)
	if !ok || m["a"] != float64(1) {
		t.Fatalf("Data tree wrong after round-trip: %+v", back.SSEFrames[0].Data)
	}
	if back.SSEFrames[1].DataText != "not-json-text" || back.SSEFrames[1].Data != nil {
		t.Fatalf("text-variant frame wrong after round-trip: %+v", back.SSEFrames[1])
	}
}

func TestCoreKind_IsAI_AllArms(t *testing.T) {
	if !KindAIChat.IsAI() || !KindAICompletion.IsAI() || !KindAIImage.IsAI() || !KindAIEmbedding.IsAI() {
		t.Fatal("all ai-* should report IsAI true")
	}
	if KindHTTPJSON.IsAI() || KindUnsupported.IsAI() {
		t.Fatal("http/unsupported should not report IsAI true")
	}
}

func TestCoreDirection_Constants(t *testing.T) {
	if DirectionRequest != "request" || DirectionResponse != "response" {
		t.Fatalf("direction constants wrong: %q %q", DirectionRequest, DirectionResponse)
	}
}

func TestCoreMessage_MarshalJSON_NilContentBecomesEmptyArray(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: nil}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" {
		t.Fatal("marshal returned empty")
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if string(out["content"]) != "[]" {
		t.Fatalf("nil content should become []; got %s", out["content"])
	}
}

func TestCoreMessage_MarshalJSON_NonNilContentPreserved(t *testing.T) {
	m := Message{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("marshal returned empty")
	}
}

func TestCoreNewMetrics_RegistersAndCaches(t *testing.T) {
	reg := prometheus.NewRegistry()
	ns := "test_core_metrics_unique_core_a"
	m1 := NewMetrics(reg, ns)
	if m1 == nil || m1.Total == nil || m1.LatencyMs == nil || m1.PayloadBytes == nil || m1.FallbackTotal == nil {
		t.Fatalf("metric fields nil: %+v", m1)
	}
	m2 := NewMetrics(reg, ns)
	if m1 != m2 {
		t.Fatalf("second call should return cached instance")
	}
	m1.Total.WithLabelValues("a", "b", "c", "d").Inc()
	m1.LatencyMs.WithLabelValues("a", "b").Observe(1.0)
	m1.PayloadBytes.WithLabelValues("a", "b").Observe(1.0)
	m1.FallbackTotal.WithLabelValues("r").Inc()
}

func TestCoreMustRegisterPrometheus_NilReturnsNil(t *testing.T) {
	if got := MustRegisterPrometheus(nil, "x"); got != nil {
		t.Fatalf("nil reg → expected nil metrics, got %+v", got)
	}
}

func TestCoreMustRegisterPrometheus_RegistersWhenNonNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := MustRegisterPrometheus(reg, "test_core_metrics_unique_core_b")
	if m == nil {
		t.Fatal("expected non-nil metrics when reg non-nil")
	}
}

func TestCoreBuildAuditFn_NilRegistryReturnsNil(t *testing.T) {
	if got := BuildAuditFn(nil, nil); got != nil {
		t.Fatalf("nil registry → expected nil fn, got %T", got)
	}
}

func TestCoreBuildAuditFn_EmptyBodyShortCircuits(t *testing.T) {
	reg := NewRegistry()
	reg.Register("openai", &coreStub{id: "openai", payload: NormalizedPayload{Kind: KindAIChat, Protocol: "openai-chat"}})
	reg.Freeze()
	fn := BuildAuditFn(reg, nil)
	raw, status, reason := fn("request", "application/json", "openai", "x", "/v1/chat/completions", false, nil)
	if raw != nil || status != "" || reason != "" {
		t.Fatalf("empty body should produce zero values; got raw=%v status=%q reason=%q", raw, status, reason)
	}
}

func TestCoreBuildAuditFn_FailedSurfaceWithMetrics(t *testing.T) {
	reg := NewRegistry()
	reg.Freeze() // no normalizers → guaranteed ErrUnsupported
	pReg := prometheus.NewRegistry()
	m := NewMetrics(pReg, "test_core_audit_unique_c")
	fn := BuildAuditFn(reg, m)
	raw, status, reason := fn("request", "application/json", "novendor", "x", "/v1/x", false, []byte(`{}`))
	if status != "failed" {
		t.Fatalf("status = %q want failed", status)
	}
	if reason == "" {
		t.Fatalf("reason should be populated on failed")
	}
	if raw != nil {
		t.Fatalf("raw should be nil on failed; got %s", raw)
	}
}

func TestCoreBuildAuditFn_PartialStatus(t *testing.T) {
	reg := NewRegistry()
	reg.Register("custom", &coreStub{id: "custom", payload: NormalizedPayload{Kind: KindAIChat, Protocol: "custom"}, err: errors.New("partial parse")})
	reg.Freeze()
	pReg := prometheus.NewRegistry()
	m := NewMetrics(pReg, "test_core_audit_unique_d")
	fn := BuildAuditFn(reg, m)
	raw, status, reason := fn("response", "application/json", "custom", "x", "/v1/x", false, []byte(`{}`))
	if status != "partial" {
		t.Fatalf("status = %q want partial", status)
	}
	if reason == "" {
		t.Fatalf("reason should describe the error")
	}
	if len(raw) == 0 {
		t.Fatalf("partial should still marshal payload; got empty")
	}
}

func TestCoreBuildAuditFn_OkStatusAndDefaults(t *testing.T) {
	reg := NewRegistry()
	reg.Register("custom2", &coreStub{id: "custom2"}) // zero payload, no err
	reg.Freeze()
	fn := BuildAuditFn(reg, nil)
	raw, status, reason := fn("response", "application/json", "custom2", "x", "/v1/x", false, []byte(`{}`))
	if status != "ok" {
		t.Fatalf("status = %q want ok", status)
	}
	if reason != "" {
		t.Fatalf("reason should be empty on ok; got %q", reason)
	}
	if len(raw) == 0 {
		t.Fatal("raw must be populated on ok")
	}
}

func TestCoreStripContentTypeParams(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"application/json":                "application/json",
		"application/json; charset=utf-8": "application/json",
		" text/html ;x=y":                 "text/html",
		"application/x-custom; key=v ":    "application/x-custom",
	}
	for in, want := range cases {
		if got := stripContentTypeParams(in); got != want {
			t.Errorf("stripContentTypeParams(%q) = %q want %q", in, got, want)
		}
	}
}

func TestCoreRegistry_AllReturnsKeys(t *testing.T) {
	r := NewRegistry()
	r.Register("a", &coreStub{id: "a"})
	r.Register("b", &coreStub{id: "b"})
	r.Freeze()
	got := r.All()
	if len(got) != 2 {
		t.Fatalf("All() = %v want 2 keys", got)
	}
}

func TestCoreRegistry_ReplaceOverwrites(t *testing.T) {
	r := NewRegistry()
	first := &coreStub{id: "first"}
	r.Register("k", first)
	second := &coreStub{id: "second"}
	r.Replace("k", second)
	if got := r.Resolve(Meta{AdapterType: "k"}); got != second {
		t.Fatalf("Replace did not overwrite")
	}
}

func TestCoreRegistry_ReplaceOnEmptyKey(t *testing.T) {
	r := NewRegistry()
	stub := &coreStub{id: "z"}
	r.Replace("k", stub)
	if got := r.Resolve(Meta{AdapterType: "k"}); got != stub {
		t.Fatalf("Replace into empty registry failed")
	}
}

func TestCoreRegistry_ReplacePanicsOnFrozen(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.Replace("k", &coreStub{})
}

func TestCoreRegistry_SetConfidenceThreshold_Clamps(t *testing.T) {
	r := NewRegistry()
	r.SetConfidenceThreshold(-1)
	r.SetConfidenceThreshold(2)
	// With clamp to 1.0, a Confidence=0.5 payload should fall to Tier 3 generic.
	r.Register("k", &coreStub{id: "k", payload: NormalizedPayload{Kind: KindAIChat, Confidence: 0.5}})
	r.Register("*:*:*", &coreStub{id: "g", payload: NormalizedPayload{Kind: KindHTTPText, Protocol: "generic"}})
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "generic" {
		t.Fatalf("expected Tier-3 fallback; got Protocol=%q", got.Protocol)
	}
}

func TestCoreRegistry_SetConfidenceThreshold_PanicsOnFrozen(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.SetConfidenceThreshold(0.5)
}

func TestCoreRegistry_RegisterTier2_PanicsOnFrozen(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.RegisterTier2(&coreStub{})
}

func TestCoreRegistry_RegisterTier2_FiresWhenTier1LowConfidence(t *testing.T) {
	r := NewRegistry()
	tier1 := &coreStub{id: "t1", payload: NormalizedPayload{Kind: KindAIChat, Confidence: 0.4, Protocol: "t1"}}
	tier2 := &coreStub{id: "t2", payload: NormalizedPayload{Kind: KindAIChat, Confidence: 0.9, Protocol: "t2"}}
	r.Register("k", tier1)
	r.RegisterTier2(tier2)
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "t2" {
		t.Fatalf("expected Tier-2 winner, got %q", got.Protocol)
	}
}

func TestCoreRegistry_Tier2_LowConfidenceUsesBestPartial(t *testing.T) {
	r := NewRegistry()
	tier1 := &coreStub{id: "t1", payload: NormalizedPayload{Kind: KindAIChat, Confidence: 0.4, Protocol: "t1"}}
	tier2 := &coreStub{id: "t2", payload: NormalizedPayload{Kind: KindAIChat, Confidence: 0.5, Protocol: "t2"}}
	r.Register("k", tier1)
	r.RegisterTier2(tier2)
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "t2" {
		t.Fatalf("expected bestPartial t2, got %q", got.Protocol)
	}
}

func TestCoreRegistry_Tier2_ErrUnsupportedFallsThrough(t *testing.T) {
	r := NewRegistry()
	r.RegisterTier2(&coreStub{id: "t2", err: ErrUnsupported})
	r.Register("*:*:*", &coreStub{id: "g", payload: NormalizedPayload{Kind: KindHTTPText, Protocol: "generic"}})
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "unknown"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "generic" {
		t.Fatalf("expected Tier-3 fallback, got %q", got.Protocol)
	}
}

func TestCoreRegistry_Tier2_HardErrorTerminates(t *testing.T) {
	r := NewRegistry()
	hardErr := errors.New("bad bytes")
	r.RegisterTier2(&coreStub{id: "t2", err: hardErr, payload: NormalizedPayload{Kind: KindAIChat, Protocol: "t2"}})
	r.Register("*:*:*", &coreStub{id: "g", payload: NormalizedPayload{Kind: KindHTTPText, Protocol: "generic"}})
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "unknown"})
	if !errors.Is(err, hardErr) {
		t.Fatalf("hard error should propagate, got %v", err)
	}
}

func TestCoreRegistry_Tier3_HardErrorTerminates(t *testing.T) {
	r := NewRegistry()
	hardErr := errors.New("generic blew up")
	r.Register("*:*:*", &coreStub{id: "g", err: hardErr, payload: NormalizedPayload{Kind: KindHTTPText}})
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "x"})
	if !errors.Is(err, hardErr) {
		t.Fatalf("Tier-3 hard error must propagate; got %v", err)
	}
}

func TestCoreRegistry_Normalize_Tier1HardErrorTerminates(t *testing.T) {
	r := NewRegistry()
	hard := errors.New("malformed")
	r.Register("k", &coreStub{id: "k", err: hard, payload: NormalizedPayload{Kind: KindAIChat, Protocol: "k"}})
	r.Register("*:*:*", &coreStub{id: "g", payload: NormalizedPayload{Kind: KindHTTPText, Protocol: "g"}})
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "k"})
	if !errors.Is(err, hard) {
		t.Fatalf("Tier-1 hard error should short-circuit; got %v", err)
	}
}

func TestCoreRegistry_Normalize_BestPartialReturned(t *testing.T) {
	r := NewRegistry()
	r.Register("k", &coreStub{id: "k", payload: NormalizedPayload{Kind: KindAIChat, Confidence: 0.5, Protocol: "k"}})
	r.Freeze()
	got, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "k"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Protocol != "k" {
		t.Fatalf("expected bestPartial returned, got %q", got.Protocol)
	}
}

func TestCoreMaybeGunzip_GzipDecompresses(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	out, ok := MaybeGunzip(buf.Bytes())
	if !ok {
		t.Fatal("gzip not detected")
	}
	if string(out) != "hello world" {
		t.Fatalf("decompressed = %q", out)
	}
}

func TestCoreMaybeGunzip_GzipTruncatedKeepsRaw(t *testing.T) {
	raw := []byte{0x1f, 0x8b, 0x00}
	out, ok := MaybeGunzip(raw)
	if ok {
		t.Fatal("truncated gzip should fail-open")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw should be returned unchanged")
	}
}

func TestCoreMaybeGunzip_ZlibDecompresses(t *testing.T) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte("zlib body")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	out, ok := MaybeGunzip(buf.Bytes())
	if !ok {
		t.Fatal("zlib not detected")
	}
	if string(out) != "zlib body" {
		t.Fatalf("zlib decoded = %q", out)
	}
}

func TestCoreMaybeGunzip_ZlibTruncatedKeepsRaw(t *testing.T) {
	raw := []byte{0x78, 0x9c}
	out, ok := MaybeGunzip(raw)
	if ok {
		t.Fatal("truncated zlib should fail-open")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw not preserved")
	}
}

func TestCoreMaybeGunzip_ZstdDecompresses(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll([]byte("zstd body"), nil)
	_ = enc.Close()
	out, ok := MaybeGunzip(compressed)
	if !ok {
		t.Fatal("zstd not detected")
	}
	if string(out) != "zstd body" {
		t.Fatalf("zstd decoded = %q", out)
	}
}

func TestCoreMaybeGunzip_ZstdTruncatedKeepsRaw(t *testing.T) {
	raw := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}
	out, ok := MaybeGunzip(raw)
	if ok {
		t.Fatal("invalid zstd should fail-open")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw not preserved")
	}
}

func TestCoreMaybeGunzip_TooShortKeepsRaw(t *testing.T) {
	out, ok := MaybeGunzip([]byte{0x1f})
	if ok || !bytes.Equal(out, []byte{0x1f}) {
		t.Fatalf("short body should fail-open; got ok=%v out=%v", ok, out)
	}
}

func TestCoreMaybeGunzip_UnknownMagicKeepsRaw(t *testing.T) {
	raw := []byte("plain text content")
	out, ok := MaybeGunzip(raw)
	if ok {
		t.Fatal("plain text must not be flagged as compressed")
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("raw not preserved")
	}
}

var coreApplySpansMu sync.Mutex

func TestCoreApplySpans_HTTPFormMapEntry(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind: KindHTTPForm,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Form: map[string]string{"email": "user@example.com"}}},
	}
	spans := []TransformSpan{{
		Source: SourceHook, Action: ActionRedact,
		ContentAddress: "http.bodyView.form.email",
		Start:          0, End: 4, Replacement: "[REDACTED]",
	}}
	got, skipped := ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if got.HTTP.BodyView.Form["email"] != "[REDACTED]@example.com" {
		t.Fatalf("form entry not mutated: %q", got.HTTP.BodyView.Form["email"])
	}
	if p.HTTP.BodyView.Form["email"] != "user@example.com" {
		t.Fatalf("original map mutated: %q", p.HTTP.BodyView.Form["email"])
	}
}

func TestCoreApplySpans_HTTPFormMapEntryMissing(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind: KindHTTPForm,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Form: map[string]string{"k": "v"}}},
	}
	spans := []TransformSpan{{ContentAddress: "http.bodyView.form.missing", Start: 0, End: 1, Replacement: "x"}}
	_, skipped := ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", skipped)
	}
}

func TestCoreApplySpans_HTTPFormNilFormSkipped(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind: KindHTTPForm,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Form: nil}},
	}
	spans := []TransformSpan{{ContentAddress: "http.bodyView.form.k", Start: 0, End: 1, Replacement: "x"}}
	_, skipped := ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", skipped)
	}
}

func TestCoreApplySpans_ResolveTextRef_InvalidAddresses(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "x"}}}},
	}
	cases := []string{
		"messages",
		"messages.0",
		"messages.x.content.0",
		"messages.0.content.x",
		"messages.0.content.99",
		"messages.99.content.0",
		"messages.-1.content.0",
		"messages.0.wrong.0",
		"messages.0.content.0.foo",
		"http.bodyView",
		"http.bodyView.form.x",
		"http.unknownPath",
		"unknown.top",
	}
	for _, addr := range cases {
		span := []TransformSpan{{ContentAddress: addr, Start: 0, End: 1, Replacement: "z"}}
		_, skipped := ApplySpans(p, span)
		if len(skipped) != 1 {
			t.Errorf("addr=%q: expected 1 skipped, got %v", addr, skipped)
		}
	}
}

func TestCoreApplySpans_ResolveTextRef_ToolResult(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role: RoleUser, Content: []ContentBlock{{Type: ContentToolResult, ToolResult: &ToolResult{Output: "abc"}}},
		}},
	}
	spans := []TransformSpan{{ContentAddress: "messages.0.content.0.toolResult", Start: 0, End: 3, Replacement: "XYZ"}}
	got, skipped := ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	if got.Messages[0].Content[0].ToolResult.Output != "XYZ" {
		t.Fatalf("tool result not mutated: %+v", got.Messages[0].Content[0].ToolResult)
	}
}

func TestCoreApplySpans_ResolveTextRef_ToolResultNil(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role: RoleUser, Content: []ContentBlock{{Type: ContentToolResult, ToolResult: nil}},
		}},
	}
	spans := []TransformSpan{{ContentAddress: "messages.0.content.0.toolResult", Start: 0, End: 1, Replacement: "x"}}
	_, skipped := ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", skipped)
	}
}

func TestCoreApplySpans_NegativeStartClamped(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "abcdef"}}}},
	}
	spans := []TransformSpan{{ContentAddress: "messages.0.content.0", Start: -2, End: 2, Replacement: "X"}}
	got, _ := ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "Xcdef" {
		t.Fatalf("expected Xcdef, got %q", got.Messages[0].Content[0].Text)
	}
}

func TestCoreApplySpans_EndPastLengthClamped(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "abc"}}}},
	}
	spans := []TransformSpan{{ContentAddress: "messages.0.content.0", Start: 1, End: 999, Replacement: "Z"}}
	got, _ := ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "aZ" {
		t.Fatalf("expected aZ, got %q", got.Messages[0].Content[0].Text)
	}
}

func TestCoreClonePayload_AllFields(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
			{Type: ContentText, Text: "hi"},
			{Type: ContentToolResult, ToolResult: &ToolResult{Output: "tr"}},
		}}},
		Tools:   []ToolDef{{Name: "t1"}},
		RuleIDs: []string{"r1", "r2"},
		HTTP: &HTTPPayload{
			Method:          "POST",
			URL:             "https://x",
			HeadersFiltered: map[string]string{"h": "v"},
			BodyView: &HTTPBodyView{
				Text: "body",
				Form: map[string]string{"k": "v"},
			},
		},
	}
	clone := clonePayload(p)
	clone.Messages[0].Content[0].Text = "MUT"
	clone.Messages[0].Content[1].ToolResult.Output = "MUT"
	clone.Tools[0].Name = "MUT"
	clone.RuleIDs[0] = "MUT"
	clone.HTTP.HeadersFiltered["h"] = "MUT"
	clone.HTTP.BodyView.Text = "MUT"
	clone.HTTP.BodyView.Form["k"] = "MUT"

	if p.Messages[0].Content[0].Text != "hi" ||
		p.Messages[0].Content[1].ToolResult.Output != "tr" {
		t.Errorf("messages mutated through clone: %+v", p.Messages)
	}
	if p.Tools[0].Name != "t1" {
		t.Errorf("tools mutated: %+v", p.Tools)
	}
	if p.RuleIDs[0] != "r1" {
		t.Errorf("ruleIDs mutated: %+v", p.RuleIDs)
	}
	if p.HTTP.HeadersFiltered["h"] != "v" {
		t.Errorf("headers mutated: %+v", p.HTTP.HeadersFiltered)
	}
	if p.HTTP.BodyView.Text != "body" {
		t.Errorf("body view text mutated: %+v", p.HTTP.BodyView)
	}
	if p.HTTP.BodyView.Form["k"] != "v" {
		t.Errorf("form mutated: %+v", p.HTTP.BodyView.Form)
	}
}

func TestCoreClonePayload_NilSlicesPreserved(t *testing.T) {
	p := NormalizedPayload{Kind: KindAIChat}
	c := clonePayload(p)
	if c.Messages != nil || c.Tools != nil || c.RuleIDs != nil || c.HTTP != nil {
		t.Fatalf("nil slices should stay nil: %+v", c)
	}
}

func TestCoreClonePayload_HTTPWithoutBodyView(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindHTTPText,
		HTTP: &HTTPPayload{Method: "GET"},
	}
	c := clonePayload(p)
	if c.HTTP == nil || c.HTTP.Method != http.MethodGet {
		t.Fatalf("HTTP not cloned: %+v", c.HTTP)
	}
	if c.HTTP.BodyView != nil || c.HTTP.HeadersFiltered != nil {
		t.Fatalf("nil sub-fields should stay nil: %+v", c.HTTP)
	}
}

func TestCoreClonePayload_HTTPBodyViewWithoutForm(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindHTTPText,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Text: "raw"}},
	}
	c := clonePayload(p)
	if c.HTTP.BodyView == nil || c.HTTP.BodyView.Text != "raw" {
		t.Fatalf("body view text lost: %+v", c.HTTP.BodyView)
	}
	if c.HTTP.BodyView.Form != nil {
		t.Fatalf("nil form should stay nil: %+v", c.HTTP.BodyView)
	}
}

func TestCoreParseInt_EmptyAndNonDigit(t *testing.T) {
	if _, err := parseInt(""); err == nil {
		t.Fatal("empty must error")
	}
	if _, err := parseInt("12a"); err == nil {
		t.Fatal("non-digit must error")
	}
	v, err := parseInt("42")
	if err != nil || v != 42 {
		t.Fatalf("parseInt(42)=%d,%v", v, err)
	}
}

func TestCoreTextProjection_AIChat(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{
			{Role: RoleSystem, Content: []ContentBlock{{Type: ContentText, Text: "sys"}}},
			{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "user msg"}}},
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: ContentText, Text: "response"},
				{Type: ContentToolResult, ToolResult: &ToolResult{Output: "tool out"}},
				{Type: ContentReasoning, Text: "reasoning"},
			}},
		},
	}
	got := p.TextProjection()
	// Without IncludeReasoning: sys, user msg, response, tool out (no reasoning)
	if len(got) != 4 {
		t.Fatalf("expected 4 texts, got %v", got)
	}
	withReasoning := p.TextProjectionWith(TextProjectionOptions{IncludeReasoning: true})
	if len(withReasoning) != 5 {
		t.Fatalf("with reasoning expected 5 texts, got %v", withReasoning)
	}
}

func TestCoreTextProjection_HTTPText(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindHTTPText,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Text: "body text"}},
	}
	got := p.TextProjection()
	if len(got) != 1 || got[0] != "body text" {
		t.Fatalf("expected [body text], got %v", got)
	}
}

func TestCoreTextProjection_HTTPForm(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindHTTPForm,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Form: map[string]string{"k": "v"}}},
	}
	got := p.TextProjection()
	if len(got) != 1 {
		t.Fatalf("expected 1 form entry, got %v", got)
	}
}

// A compliance hook configured for http/all traffic must scan SSE
// stream content: verbatim text frames project as-is, JSON frames as
// their re-marshaled document, and empty frames are skipped. This is
// the contract that keeps fallback http-sse rows visible to keyword /
// PII hooks (they previously landed as http-text and were scanned).
func TestCoreTextProjection_HTTPSSEFrames(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindHTTPSSE,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{SSEFrames: []SSEFrame{
			{Event: "msg", Data: map[string]any{"ssn": "123-45-6789"}},
			{Event: "done", DataText: "[DONE]"},
			{Event: "ping"}, // empty data line: neither Data nor DataText
		}}},
	}
	got := p.TextProjection()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (empty frame skipped), got %v", got)
	}
	if got[0] != `{"ssn":"123-45-6789"}` {
		t.Fatalf("JSON frame should project its re-marshaled document, got %q", got[0])
	}
	if got[1] != "[DONE]" {
		t.Fatalf("text frame should project verbatim, got %q", got[1])
	}
}

// CT-less JSON bodies now land as http-json instead of http-text; the
// projection must keep them scannable as a single compact document.
func TestCoreTextProjection_HTTPJSONTree(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindHTTPJSON,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{JSON: map[string]any{"email": "a@b.c"}}},
	}
	got := p.TextProjection()
	if len(got) != 1 || got[0] != `{"email":"a@b.c"}` {
		t.Fatalf("JSON tree should project as one compact document, got %v", got)
	}
}

// Legacy http-json rows that carried raw bytes in Text keep the Text
// branch: Text wins over the JSON tree when both are set.
func TestCoreTextProjection_TextWinsOverJSON(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindHTTPJSON,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Text: "raw", JSON: map[string]any{"k": "v"}}},
	}
	got := p.TextProjection()
	if len(got) != 1 || got[0] != "raw" {
		t.Fatalf("Text should win over JSON tree, got %v", got)
	}
}

func TestCoreTextProjection_NilPayload(t *testing.T) {
	var p *NormalizedPayload
	if got := p.TextProjection(); got != nil {
		t.Fatalf("nil payload should return nil, got %v", got)
	}
}

func TestCoreTextProjection_Redacted(t *testing.T) {
	p := &NormalizedPayload{Kind: KindAIChat, Redacted: true}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("redacted payload should return nil, got %v", got)
	}
}

func TestCoreTextProjection_HTTPNilBodyView(t *testing.T) {
	p := &NormalizedPayload{Kind: KindHTTPJSON, HTTP: &HTTPPayload{}}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("nil body view should return nil, got %v", got)
	}
}

func TestCoreTextProjection_UnsupportedKind(t *testing.T) {
	p := &NormalizedPayload{Kind: KindUnsupported}
	if got := p.TextProjection(); got != nil {
		t.Fatalf("unsupported kind should return nil, got %v", got)
	}
}

func TestCoreJoinedText(t *testing.T) {
	p := &NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "a"}}},
			{Role: RoleAssistant, Content: []ContentBlock{{Type: ContentText, Text: "b"}}},
		},
	}
	if got := p.JoinedText(" | "); got != "a | b" {
		t.Fatalf("JoinedText = %q want %q", got, "a | b")
	}
	empty := &NormalizedPayload{Kind: KindAIChat}
	if got := empty.JoinedText("|"); got != "" {
		t.Fatalf("empty payload should produce empty join; got %q", got)
	}
}

func TestCoreScoreTier1Confidence_FullMatch(t *testing.T) {
	spec := FieldSpec{
		Required: []string{"model", "choices", "usage"},
		Optional: []string{"id", "object", "created"},
	}
	body := []byte(`{"model":"gpt-4","choices":[],"usage":{},"id":"c1","object":"chat","created":1}`)
	got := scoreTier1Confidence(body, spec)
	// Full match: 0.5 + 0.4 + 0.1 - small_penalty ≈ near 1.0
	if got < 0.9 {
		t.Fatalf("full match confidence too low: %.3f", got)
	}
}

func TestCoreScoreTier1Confidence_AllRequiredMissing(t *testing.T) {
	spec := FieldSpec{
		Required: []string{"model", "choices", "usage"},
		Optional: []string{"id"},
	}
	body := []byte(`{"unknown1":"x","unknown2":"y"}`)
	got := scoreTier1Confidence(body, spec)
	// 0.5 + 0 + 0 - unknown_penalty = around 0.4-0.5
	if got > 0.6 {
		t.Fatalf("all-required-missing confidence too high: %.3f", got)
	}
}

func TestCoreScoreTier1Confidence_EmptyBody(t *testing.T) {
	spec := FieldSpec{Required: []string{"model"}, Optional: []string{"id"}}
	got := scoreTier1Confidence([]byte(`{}`), spec)
	// Empty observed set → returns 0.90
	if got != 0.90 {
		t.Fatalf("empty body should return 0.90; got %.3f", got)
	}
}

func TestCoreScoreTier1Confidence_InvalidJSON(t *testing.T) {
	spec := FieldSpec{Required: []string{"model"}}
	got := scoreTier1Confidence([]byte("not json"), spec)
	// Non-JSON → empty set → 0.90
	if got != 0.90 {
		t.Fatalf("invalid JSON should return 0.90; got %.3f", got)
	}
}

func TestCoreScoreTier1Confidence_OptionalBonus_Capped(t *testing.T) {
	spec := FieldSpec{
		Required: []string{"a"},
		Optional: []string{"b", "c", "d", "e", "f", "g"}, // many optional
	}
	// All present including all 6 optional — bonus capped at 0.10
	body := []byte(`{"a":1,"b":1,"c":1,"d":1,"e":1,"f":1,"g":1}`)
	got := scoreTier1Confidence(body, spec)
	// 0.5 + 0.4 + 0.1 (capped) - 0 = 1.0
	if got < 0.99 {
		t.Fatalf("capped optional bonus should still hit 1.0; got %.3f", got)
	}
}

func TestCoreScoreTier1Confidence_SSEBody(t *testing.T) {
	spec := FieldSpec{
		Required: []string{"model", "choices"},
		Optional: []string{"id"},
	}
	// SSE body: topLevelKeys should extract the data: payload JSON
	body := []byte("data: {\"model\":\"gpt\",\"choices\":[],\"id\":\"c1\"}\n\ndata: [DONE]\n")
	got := scoreTier1Confidence(body, spec)
	// model + choices present: should be above sub-threshold
	if got < 0.7 {
		t.Fatalf("SSE body confidence too low: %.3f", got)
	}
}

func TestCoreScoreTier1Confidence_ExportedWrapper(t *testing.T) {
	spec := FieldSpec{Required: []string{"model"}}
	body := []byte(`{"model":"gpt"}`)
	// Exported wrapper must delegate to scoreTier1Confidence and return same result.
	exported := ScoreTier1Confidence(body, spec)
	internal := scoreTier1Confidence(body, spec)
	if exported != internal {
		t.Fatalf("ScoreTier1Confidence = %.3f, scoreTier1Confidence = %.3f", exported, internal)
	}
}

func TestCoreFirstSSEDataChunk_ExtractsFirstData(t *testing.T) {
	raw := []byte("event: message\ndata: {\"key\":\"val\"}\n\ndata: [DONE]\n")
	chunk := firstSSEDataChunk(raw)
	if string(chunk) != `{"key":"val"}` {
		t.Fatalf("firstSSEDataChunk = %q", chunk)
	}
}

func TestCoreFirstSSEDataChunk_SkipsDone(t *testing.T) {
	raw := []byte("data: [DONE]\n")
	chunk := firstSSEDataChunk(raw)
	if chunk != nil {
		t.Fatalf("[DONE] should return nil, got %q", chunk)
	}
}

func TestCoreFirstSSEDataChunk_EmptyLine(t *testing.T) {
	raw := []byte("data: \n")
	chunk := firstSSEDataChunk(raw)
	if chunk != nil {
		t.Fatalf("empty data line should return nil, got %q", chunk)
	}
}

func TestCoreTopLevelKeys_SSEPrefix(t *testing.T) {
	raw := []byte("data: {\"a\":1,\"b\":2}\n")
	keys := topLevelKeys(raw)
	if _, ok := keys["a"]; !ok {
		t.Fatalf("expected key 'a' from SSE body, got %v", keys)
	}
}

func TestCoreTopLevelKeys_SSENoUsableData(t *testing.T) {
	raw := []byte("data: [DONE]\n")
	keys := topLevelKeys(raw)
	if keys != nil {
		t.Fatalf("DONE-only SSE should return nil keys, got %v", keys)
	}
}

func TestCoreTopLevelKeys_NonObjectJSON(t *testing.T) {
	// Array body at top level should return nil (not an object)
	keys := topLevelKeys([]byte(`["a","b"]`))
	if keys != nil {
		t.Fatalf("array body should return nil keys, got %v", keys)
	}
}

func TestCoreExportedParseInt(t *testing.T) {
	v, err := ParseInt("7")
	if err != nil || v != 7 {
		t.Fatalf("ParseInt(7) = %d, %v", v, err)
	}
	_, err = ParseInt("")
	if err == nil {
		t.Fatal("ParseInt('') should error")
	}
}

func TestCoreExportedClonePayload(t *testing.T) {
	p := NormalizedPayload{Kind: KindAIChat, Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "x"}}}}}
	c := ClonePayload(p)
	c.Messages[0].Content[0].Text = "MUTATED"
	if p.Messages[0].Content[0].Text != "x" {
		t.Fatal("ClonePayload should deep copy")
	}
}

func TestCoreExportedStripContentTypeParams(t *testing.T) {
	got := StripContentTypeParams("application/json; charset=utf-8")
	if got != "application/json" {
		t.Fatalf("StripContentTypeParams = %q", got)
	}
	if StripContentTypeParams("") != "" {
		t.Fatal("empty input should return empty")
	}
}

func TestCoreRegistry_Resolve_ContentTypePath(t *testing.T) {
	r := NewRegistry()
	ctStub := &coreStub{id: "ct", payload: NormalizedPayload{Kind: KindHTTPJSON}}
	r.Register(":application/json:", ctStub)
	r.Freeze()
	got := r.Resolve(Meta{ContentType: "application/json"})
	if got != ctStub {
		t.Fatalf("content-type key lookup failed; got %v", got)
	}
}

func TestCoreRegistry_Resolve_PathOnly(t *testing.T) {
	r := NewRegistry()
	pathStub := &coreStub{id: "path", payload: NormalizedPayload{Kind: KindAIChat}}
	r.Register("::/v1/messages", pathStub)
	r.Freeze()
	got := r.Resolve(Meta{EndpointPath: "/v1/messages"})
	if got != pathStub {
		t.Fatalf("path-only key lookup failed; got %v", got)
	}
}

func TestCoreRegistry_Normalize_NoMatchReturnsUnsupported(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	_, err := r.Normalize(context.Background(), []byte(`{}`), Meta{AdapterType: "unknown"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestCoreMaybeGunzip_GzipCorruptCRC(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write([]byte("hello"))
	_ = w.Close()
	bad := buf.Bytes()
	// Corrupt the CRC32 trailer (last 8 bytes).
	for i := len(bad) - 4; i < len(bad); i++ {
		bad[i] = 0xff
	}
	out, ok := MaybeGunzip(bad)
	if ok {
		t.Fatal("corrupt gzip trailer should fail-open")
	}
	if !bytes.Equal(out, bad) {
		t.Fatalf("raw must be unchanged on read err")
	}
}

func TestCoreApplySpans_StartGreaterThanEndSkipped(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "abcdef"}}}},
	}
	spans := []TransformSpan{{ContentAddress: "messages.0.content.0", Start: 4, End: 2, Replacement: "X"}}
	got, _ := ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "abcdef" {
		t.Fatalf("text changed despite invalid span: %q", got.Messages[0].Content[0].Text)
	}
}

// aiTextProjection embedding branch

// TestCoreTextProjection_AIEmbedding verifies that KindAIEmbedding payloads
// return their Inputs via TextProjection so content-scanning hooks can
// inspect embedding text inputs without kind-specific awareness.
func TestCoreTextProjection_AIEmbedding(t *testing.T) {
	p := &NormalizedPayload{
		Kind:   KindAIEmbedding,
		Inputs: []string{"hello world", "foo bar"},
	}
	got := p.TextProjection()
	if len(got) != 2 {
		t.Fatalf("expected 2 inputs, got %v", got)
	}
	if got[0] != "hello world" || got[1] != "foo bar" {
		t.Fatalf("unexpected inputs: %v", got)
	}
}

// TestCoreTextProjection_AIEmbedding_EmptyInputs verifies that an embedding
// payload with no non-empty inputs returns an empty slice.
func TestCoreTextProjection_AIEmbedding_EmptyInputs(t *testing.T) {
	p := &NormalizedPayload{Kind: KindAIEmbedding, Inputs: []string{"", ""}}
	got := p.TextProjection()
	if len(got) != 0 {
		t.Fatalf("expected empty projection for all-empty inputs, got %v", got)
	}
}

// TestCoreTextProjection_AIEmbedding_NilInputs verifies that an embedding
// payload with nil Inputs returns an empty projection (not nil — caller
// expects a slice).
func TestCoreTextProjection_AIEmbedding_NilInputs(t *testing.T) {
	p := &NormalizedPayload{Kind: KindAIEmbedding, Inputs: nil}
	got := p.TextProjection()
	if got == nil {
		t.Fatalf("expected empty (non-nil) slice for nil Inputs, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0-length slice for nil Inputs, got %v", got)
	}
}

// TestCoreTextProjection_AIEmbedding_WithReasoningOption verifies that the
// IncludeReasoning option does not affect embedding projection (embeddings
// have no reasoning blocks).
func TestCoreTextProjection_AIEmbedding_WithReasoningOption(t *testing.T) {
	p := &NormalizedPayload{
		Kind:   KindAIEmbedding,
		Inputs: []string{"embed this"},
	}
	got := p.TextProjectionWith(TextProjectionOptions{IncludeReasoning: true})
	if len(got) != 1 || got[0] != "embed this" {
		t.Fatalf("expected [embed this], got %v", got)
	}
}

func TestCoreApplySpans_StartLargerThanText(t *testing.T) {
	coreApplySpansMu.Lock()
	defer coreApplySpansMu.Unlock()
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "ab"}}}},
	}
	spans := []TransformSpan{{ContentAddress: "messages.0.content.0", Start: 5, End: 6, Replacement: "X"}}
	got, _ := ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "ab" {
		t.Fatalf("text shouldn't change for out-of-range start: %q", got.Messages[0].Content[0].Text)
	}
}

func TestCoreScoreTier1Confidence_NoRequiredDeclared(t *testing.T) {
	// A spec with no required keys must not penalise the parse: the
	// required component is granted in full, so a body of declared
	// optional keys scores well above the claim threshold.
	spec := FieldSpec{Optional: []string{"a", "b"}}
	got := scoreTier1Confidence([]byte(`{"a":1,"b":2}`), spec)
	if got < 0.9 {
		t.Fatalf("no-required spec should score >= 0.9; got %.3f", got)
	}
}

func TestCoreTopLevelKeys_WhitespaceOnly(t *testing.T) {
	if keys := topLevelKeys([]byte("  \r\n\t ")); keys != nil {
		t.Fatalf("whitespace-only body should return nil keys, got %v", keys)
	}
}

func TestCoreTopLevelKeys_MalformedObject(t *testing.T) {
	if keys := topLevelKeys([]byte(`{"a":`)); keys != nil {
		t.Fatalf("malformed JSON object should return nil keys, got %v", keys)
	}
}

func TestCoreTextProjection_NilReceiver(t *testing.T) {
	var p *NormalizedPayload
	if got := p.TextProjectionWith(TextProjectionOptions{}); got != nil {
		t.Fatalf("nil payload should project nothing, got %v", got)
	}
}

func TestCoreTextProjection_RedactedProjectsNothing(t *testing.T) {
	// Redacted payloads must never leak text into hooks/judges.
	p := &NormalizedPayload{
		Kind:     KindAIChat,
		Redacted: true,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "secret"}}}},
	}
	if got := p.TextProjectionWith(TextProjectionOptions{}); got != nil {
		t.Fatalf("redacted payload should project nothing, got %v", got)
	}
}

func TestCoreTextProjection_HTTPEmptyBodyView(t *testing.T) {
	p := &NormalizedPayload{Kind: KindHTTPText, HTTP: &HTTPPayload{BodyView: &HTTPBodyView{}}}
	if got := p.TextProjectionWith(TextProjectionOptions{}); got != nil {
		t.Fatalf("empty bodyView should project nothing, got %v", got)
	}
}

func TestCoreBuildAuditFn_MarshalErrorFailsClosed(t *testing.T) {
	// A payload that cannot marshal (here: an Inf tool input, which
	// encoding/json rejects) must surface status=failed with a marshal
	// reason and a nil wire body — never a half-written row.
	bad := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role: RoleAssistant,
			Content: []ContentBlock{{
				Type:    ContentToolUse,
				ToolUse: &ToolUse{Name: "t", Input: map[string]any{"x": math.Inf(1)}},
			}},
		}},
	}
	reg := NewRegistry()
	reg.Register("badcodec", &coreStub{id: "badcodec", payload: bad})
	reg.Freeze()
	pReg := prometheus.NewRegistry()
	m := NewMetrics(pReg, "test_core_audit_marshal_err")
	fn := BuildAuditFn(reg, m)
	raw, status, reason := fn("response", "application/json", "badcodec", "x", "/v1/x", false, []byte(`{}`))
	if status != "failed" {
		t.Fatalf("status = %q want failed", status)
	}
	if raw != nil {
		t.Fatalf("raw must be nil on marshal failure, got %s", raw)
	}
	if !strings.Contains(reason, "marshal normalized payload") {
		t.Fatalf("reason should name the marshal failure, got %q", reason)
	}
}

func TestCoreDrainDecompressor_ConstructorErrorKeepsRaw(t *testing.T) {
	// The shared decompressor tail must fall back to the original bytes
	// when the codec constructor rejected the header, so normalizers
	// still see SOMETHING for a body with lying magic bytes.
	orig := []byte{0x78, 0x9c, 0xde, 0xad}
	out, ok := drainDecompressor(nil, errors.New("bad header"), orig)
	if ok || !bytes.Equal(out, orig) {
		t.Fatalf("constructor error must return (raw, false); got ok=%v out=%v", ok, out)
	}
}
