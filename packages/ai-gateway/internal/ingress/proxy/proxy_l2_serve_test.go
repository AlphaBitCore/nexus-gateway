// proxy_l2_serve_test.go — coverage for the previously-untested REACHABLE
// branches of proxy_l2.go (tryL2Lookup serve paths + credential-skip guards,
// resolveL1CacheScope empty-id fail-safes, buildEmbeddingInput empty-plan) and
// the writeIngressError hint branch in proxy_errors.go.
//
// The L2 HIT *serve* paths (handleStreamHit / handleNonStreamHit) were marked
// "integration-only" by a prior coverage pass; they are in fact reachable in a
// unit test by driving tryL2Lookup with a stub SemanticReader that returns a
// valid Entry over the fully-wired makeOpenAIDeps harness. Each test asserts an
// observable outcome (the served wire body, the stamped audit fields, the
// served HTTP status), never just coverage execution.
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// makeL2ServeParams builds an l2ReadParams wired against the full
// makeOpenAIDeps harness so the HIT serve path (handleStreamHit /
// handleNonStreamHit) can run end-to-end. The request carries an OpenAI
// chat Ingress in its context — required by handleNonStreamHit's
// IngressFromContext reshape step.
func makeL2ServeParams(t *testing.T, isStream bool) l2ReadParams {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(WithIngress(r.Context(), Ingress{BodyFormat: provcore.FormatOpenAI}))
	return l2ReadParams{
		r:           r,
		w:           httptest.NewRecorder(),
		rec:         &audit.Record{},
		routeResult: &routingcore.RouteResult{},
		primary: routingcore.RoutingTarget{
			ProviderID:      "p-openai",
			ProviderName:    "openai",
			ProviderModelID: "gpt-4o",
			ModelID:         "gpt-4o",
			ModelCode:       "gpt-4o",
			AdapterType:     "openai",
		},
		isStream:      isStream,
		endpointType:  "chat",
		requestID:     "req-l2-serve",
		start:         time.Now(),
		logger:        noopLogger(),
		canonicalMsgs: sampleMsgs(),
	}
}

// TestTryL2Lookup_NonStreamHit_ServesCachedBody covers proxy_l2.go:351-364:
// a non-stream L2 HIT converts the Entry, stamps the audit record as a
// semantic HIT, and serves the cached canonical body through
// handleNonStreamHit. Asserts both the stamped audit fields AND the served
// body so it is a real-behavior test, not a coverage tap.
func TestTryL2Lookup_NonStreamHit_ServesCachedBody(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	canonicalBody := []byte(`{"id":"chatcmpl_l2","object":"chat.completion","created":1747353700,` +
		`"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"L2 cached reply"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)

	rdr := &stubSemanticReader{result: semantic.ReadResult{
		EmbeddingCostUSD: 0.0001,
		EmbeddingModelID: "text-embedding-3-small",
		Entry: &semantic.Entry{
			ResponseBody:     canonicalBody,
			UpstreamProvider: "openai",
			UpstreamModel:    "gpt-4o",
			EntryKey:         "nexus:semantic-cache:v1:abc123",
			Usage:            map[string]any{"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7},
			CachedAt:         time.Now().UTC(),
		},
	}}
	deps.SemanticReader = rdr
	deps.SemanticConfigCache = enabledFleetCache()
	deps.CredManager = &stubCredManager{}

	h := NewHandler(deps)
	p := makeL2ServeParams(t, false)
	w := p.w.(*httptest.ResponseRecorder)

	if !h.tryL2Lookup(p) {
		t.Fatal("want hit=true on non-stream L2 HIT")
	}
	if p.rec.GatewayCacheStatus != audit.GatewayCacheHit {
		t.Errorf("GatewayCacheStatus=%q want %q", p.rec.GatewayCacheStatus, audit.GatewayCacheHit)
	}
	if p.rec.GatewayCacheKind != audit.GatewayCacheKindSemantic {
		t.Errorf("GatewayCacheKind=%q want %q", p.rec.GatewayCacheKind, audit.GatewayCacheKindSemantic)
	}
	if p.rec.GatewayCacheL2EntryKey != "nexus:semantic-cache:v1:abc123" {
		t.Errorf("GatewayCacheL2EntryKey=%q want the entry key", p.rec.GatewayCacheL2EntryKey)
	}
	if p.rec.EmbeddingCostUsd != 0.0001 {
		t.Errorf("EmbeddingCostUsd=%v want 0.0001 (always stamped)", p.rec.EmbeddingCostUsd)
	}
	if p.rec.EmbeddingModelID != "text-embedding-3-small" {
		t.Errorf("EmbeddingModelID=%q want text-embedding-3-small", p.rec.EmbeddingModelID)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "L2 cached reply") {
		t.Errorf("served body missing cached content: %s", w.Body.String())
	}
}

// TestTryL2Lookup_StreamHit_ServesReplay covers proxy_l2.go:345-348: a
// stream L2 HIT converts the chunk-array Entry and replays it through
// handleStreamHit. Asserts the replayed deltas surface in the SSE body and
// the audit record carries the semantic-HIT stamps.
func TestTryL2Lookup_StreamHit_ServesReplay(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	// Stream entries store a JSON array of cachecore.ChunkRecord; the field
	// tags are "d" (delta) and "done" (see cache/core ChunkRecord).
	chunkBody := []byte(`[{"d":"streamed "},{"d":"L2 reply"},` +
		`{"done":true,"u":{"prompt_tokens":2,"completion_tokens":4,"total_tokens":6}}]`)

	rdr := &stubSemanticReader{result: semantic.ReadResult{
		EmbeddingCostUSD: 0.0002,
		Entry: &semantic.Entry{
			ResponseBody:     chunkBody,
			UpstreamProvider: "openai",
			UpstreamModel:    "gpt-4o",
			EntryKey:         "nexus:semantic-cache:v1:stream999",
			CachedAt:         time.Now().UTC(),
		},
	}}
	deps.SemanticReader = rdr
	deps.SemanticConfigCache = enabledFleetCache()
	deps.CredManager = &stubCredManager{}

	h := NewHandler(deps)
	p := makeL2ServeParams(t, true)
	w := p.w.(*httptest.ResponseRecorder)

	if !h.tryL2Lookup(p) {
		t.Fatal("want hit=true on stream L2 HIT")
	}
	if p.rec.GatewayCacheKind != audit.GatewayCacheKindSemantic {
		t.Errorf("GatewayCacheKind=%q want %q", p.rec.GatewayCacheKind, audit.GatewayCacheKindSemantic)
	}
	if p.rec.GatewayCacheL2EntryKey != "nexus:semantic-cache:v1:stream999" {
		t.Errorf("GatewayCacheL2EntryKey=%q want the stream entry key", p.rec.GatewayCacheL2EntryKey)
	}
	out := w.Body.String()
	if !strings.Contains(out, "streamed ") || !strings.Contains(out, "L2 reply") {
		t.Errorf("replayed stream deltas missing from SSE body: %s", out)
	}
}

// TestTryL2Lookup_NonStream_ConversionError_ResetsStamps covers
// proxy_l2.go:351-360: a non-stream Entry whose body is not a valid response
// envelope... in practice ToCacheResponseEntry only errors on a nil entry, so
// this path is genuinely unreachable for a non-nil entry. Documented in the
// report — no test fabricated.

// TestTryL2Lookup_CredError_SkipsQuietly covers proxy_l2.go:280-286: the
// embedding credential lookup returns an error, so L2 is skipped without
// calling the reader and without serving anything. The skip must NOT stamp a
// HIT.
func TestTryL2Lookup_CredError_SkipsQuietly(t *testing.T) {
	rdr := &stubSemanticReader{}
	h := &Handler{deps: &Deps{
		SemanticReader:      rdr,
		SemanticConfigCache: enabledFleetCache(),
		CredManager:         &stubCredManager{err: errCredLookupFailed},
	}}
	p := makeTryParams(t)
	if h.tryL2Lookup(p) {
		t.Error("want hit=false when embedding cred lookup fails")
	}
	if rdr.called.Load() != 0 {
		t.Error("reader must not be called when cred lookup fails")
	}
	if p.rec.GatewayCacheStatus == audit.GatewayCacheHit {
		t.Error("must not stamp a HIT on cred-lookup failure")
	}
}

// errCredLookupFailed is a sentinel cred-lookup error for the skip test.
var errCredLookupFailed = &credErrStub{}

type credErrStub struct{}

func (*credErrStub) Error() string { return "vault unreachable" }

// TestTryL2Lookup_StampsEmbeddingModelID covers proxy_l2.go:312-314: on a
// reader MISS that nonetheless reports the embedding model used, the model id
// is stamped onto the audit record so the embedding charge is attributable
// even when there is no cache hit.
func TestTryL2Lookup_StampsEmbeddingModelID(t *testing.T) {
	rdr := &stubSemanticReader{result: semantic.ReadResult{
		EmbeddingCostUSD: 0.00005,
		EmbeddingModelID: "text-embedding-3-large",
		// Entry nil → miss; the model id stamp at 312-314 still fires.
	}}
	h := &Handler{deps: &Deps{
		SemanticReader:      rdr,
		SemanticConfigCache: enabledFleetCache(),
		CredManager:         &stubCredManager{},
	}}
	p := makeTryParams(t)
	if h.tryL2Lookup(p) {
		t.Error("want hit=false on miss")
	}
	if p.rec.EmbeddingModelID != "text-embedding-3-large" {
		t.Errorf("EmbeddingModelID=%q want text-embedding-3-large (stamped on miss)", p.rec.EmbeddingModelID)
	}
	if p.rec.EmbeddingCostUsd != 0.00005 {
		t.Errorf("EmbeddingCostUsd=%v want 0.00005", p.rec.EmbeddingCostUsd)
	}
}

// TestScheduleL2Write_NilCredManager covers proxy_l2.go:405-409: the defensive
// CredManager-nil guard on the write path. With no cred manager the write is
// skipped without firing the writer goroutine.
func TestScheduleL2Write_NilCredManager(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{
		SemanticWriter:      w,
		SemanticConfigCache: enabledFleetCache(),
		// CredManager omitted on purpose.
	}}
	h.scheduleL2Write(&audit.Record{}, routingcore.RoutingTarget{},
		sampleMsgs(), []byte(`{"id":"r"}`), nil, false, Ingress{}, noopLogger())
	if w.called.Load() != 0 {
		t.Error("writer must not be called when CredManager is nil")
	}
}

// TestScheduleL2Write_CredError covers proxy_l2.go:412-416: the embedding cred
// lookup fails on the write path, so the write is skipped (logged best-effort)
// and the writer goroutine never fires.
func TestScheduleL2Write_CredError(t *testing.T) {
	w := newStubWriter()
	h := &Handler{deps: &Deps{
		SemanticWriter:      w,
		SemanticConfigCache: enabledFleetCache(),
		CredManager:         &stubCredManager{err: errCredLookupFailed},
	}}
	h.scheduleL2Write(&audit.Record{}, routingcore.RoutingTarget{},
		sampleMsgs(), []byte(`{"id":"r"}`), nil, false, Ingress{}, noopLogger())
	if w.called.Load() != 0 {
		t.Error("writer must not be called when embedding cred lookup fails")
	}
}

// resolveL1CacheScope empty-id fail-safes (proxy_l2.go:107-109, 117-119):
// when vary_by selects a dimension whose id is empty on the record, the scope
// falls back to fleet-wide ("") rather than emitting a "user:" / "vk:" token
// with an empty id (which would collide across all such records).

func TestResolveL1CacheScope_UserVaryBy_EmptyUserID_FallsFleetWide(t *testing.T) {
	cc := newConfigCacheVaryBy("user")
	rec := &audit.Record{VirtualKeyID: "vk-1"} // no UserID
	if got := resolveL1CacheScope(cc, rec); got != "" {
		t.Errorf("user vary_by with empty user id: got %q, want empty (fleet-wide)", got)
	}
}

func TestResolveL1CacheScope_VKVaryBy_EmptyVKID_FallsFleetWide(t *testing.T) {
	cc := newConfigCacheVaryBy("vk")
	rec := &audit.Record{UserID: "u-1"} // no VirtualKeyID
	if got := resolveL1CacheScope(cc, rec); got != "" {
		t.Errorf("vk vary_by with empty vk id: got %q, want empty (fleet-wide)", got)
	}
}

// TestBuildEmbeddingInput_EmptyPlan covers proxy_l2.go:190-192: messages carry
// embeddable text but the default system_plus_last_user strategy finds neither
// a system nor a user message (assistant-only turn) → Plan returns an empty
// message set → buildEmbeddingInput returns ("", false).
func TestBuildEmbeddingInput_EmptyPlan_AssistantOnly(t *testing.T) {
	msgs := []normcore.Message{
		{Role: "assistant", Content: []normcore.ContentBlock{
			{Type: normcore.ContentText, Text: "I am an assistant reply with no user turn."},
		}},
	}
	got, ok := buildEmbeddingInput(msgs, inputstaging.StrategySystemPlusLastUser, 8192)
	if ok {
		t.Errorf("want ok=false when staging plan yields no messages; got text=%q", got)
	}
}

// TestWriteIngressError_NonOpenAIIngress_WithHint covers proxy_errors.go:50-52:
// a non-OpenAI ingress (anthropic) error with a non-empty hint must fold the
// hint into the reshaped error message "(hint)".
func TestWriteIngressError_NonOpenAIIngress_WithHint(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{IngressFormat: string(provcore.FormatAnthropic)}
	h.writeDetailedErr(w, rec, http.StatusBadRequest, "invalid_request",
		"model not allowed", "enable it in the virtual key")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "model not allowed") {
		t.Errorf("error body missing message: %s", body)
	}
	if !strings.Contains(body, "enable it in the virtual key") {
		t.Errorf("hint not folded into anthropic error body: %s", body)
	}
	// Anthropic envelope is {"type":"error",...} — confirm it reshaped, not the
	// OpenAI proxy_error shape.
	if strings.Contains(body, "proxy_error") {
		t.Errorf("expected anthropic error envelope, got OpenAI proxy_error: %s", body)
	}
	if rec.ResponseBody == nil {
		t.Error("rec.ResponseBody not stamped with the error envelope")
	}
}
