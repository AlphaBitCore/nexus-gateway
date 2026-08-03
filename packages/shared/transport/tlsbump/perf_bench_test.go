package tlsbump

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// Per-request benchmarks for the bumped path.
//
// This package had NO benchmarks at all, despite being the compliance-proxy and
// agent hot path — `packages/compliance-proxy` only assembles BumpOptions and
// delegates to BumpConnection. Seven per-request findings were therefore
// unmeasurable and could not reach a terminal state under the program's
// measure-everything rule. Playbook section 1 already says a hot path without
// benchmarks makes wiring them item #1; the program applied that to the agent and
// missed the same gap here.
//
// Each arm drives a REAL production function with in-package fakes, never a
// restatement of what that function does. A benchmark that reimplements the
// sequence it claims to price measures the copy, not the code — which is the error
// that made an earlier guard test in this program inert.
//
// HOW TO RUN THESE (the protocol matters more than the numbers):
//
//	go test -bench does NOT interleave sub-benchmarks — it runs all N repetitions of
//	the first, then all N of the second. Build two binaries with `go test -c -o`,
//	differing in exactly one file, and alternate them one process per arm with the
//	arm order rotated each round; then `benchstat before.txt after.txt`. Take a
//	same-minute `after`-vs-`after` null control: on the development host identical
//	binaries have drifted +14.1% at p=0.62. Trust `allocs/op` and `B/op`, which are
//	±0%; treat `ns/op` as inconclusive unless the effect clears the null control.

// benchDiscardLogger keeps log handling out of the measurement. Note this hides
// exactly the cost finding C-4 is about — slog boxes its arguments at the call site
// whether or not the level is enabled — so the C-4 arm below uses a real handler at
// a level that discards, which is the production shape.
func benchDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// benchFlow builds the minimum bumpedFlow that prepare() needs. bo is a zero-value
// bumpOptions on purpose: every field prepare() touches is nil-guarded, so this is
// the cheapest production-reachable configuration — a keep-alive request on a tunnel
// with no payload-capture store wired. Anything heavier would price the store rather
// than the preamble.
func benchFlow(logger *slog.Logger) *bumpedFlow {
	return &bumpedFlow{
		ctx:               context.Background(),
		targetHost:        "api.openai.com",
		logger:            logger,
		bo:                &bumpOptions{},
		complianceEnabled: true,
	}
}

func benchRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("User-Agent", "bench/1.0")
	return r
}

// BenchmarkBumpedExchange_Prepare prices the per-request preamble every bumped
// request pays before any policy or upstream work: the PhaseSink and its context
// clone, the phaseBreakdown map, the payload-capture snapshot, the correlation-id
// mint, the URL rewrite, and the seven-argument entry Debug.
//
// It is the arm for findings C-2 (the map, allocated per request and usually left
// completely empty because tls_handshake_ms is stamped at most once per tunnel via
// sync.Once), C-3 (the WithContext clones — this covers the two in newExchange and
// prepare), C-4 (the Debug boxing its arguments regardless of level) and C-7 (the
// per-request uuid when the client sent no correlation header).
func BenchmarkBumpedExchange_Prepare(b *testing.B) {
	logger := benchDiscardLogger()
	flow := benchFlow(logger)
	w := httptest.NewRecorder()

	b.ReportAllocs()
	for range b.N {
		// A fresh request per iteration: prepare() mutates headers and the URL, and
		// reusing one would let the second iteration skip the uuid mint entirely
		// because X-Nexus-Request-Id is now set — measuring a path production only
		// takes when the client supplied the header.
		x := flow.newExchange(w, benchRequest())
		x.prepare()
	}
}

// BenchmarkBumpedExchange_Prepare_ClientSuppliedID is the other half of C-7: an
// agent-intercepted flow seeds X-Nexus-Request-Id, so the uuid is not minted. The
// delta between this arm and the one above is what a correlation header saves, and
// therefore what generating one costs.
func BenchmarkBumpedExchange_Prepare_ClientSuppliedID(b *testing.B) {
	logger := benchDiscardLogger()
	flow := benchFlow(logger)
	w := httptest.NewRecorder()

	b.ReportAllocs()
	for range b.N {
		r := benchRequest()
		r.Header.Set("X-Nexus-Request-Id", "3f7c1e90-8a2b-4c5d-9e6f-0a1b2c3d4e5f")
		x := flow.newExchange(w, r)
		x.prepare()
	}
}

// benchRoundTripper answers every request without touching the network, so
// ForwardRequest's own work — the request clone, the hop-by-hop deletes and the
// Accept-Encoding strip — is what gets measured rather than a dial.
type benchRoundTripper struct{}

func (benchRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// BenchmarkUpstream_ForwardRequest is the arm for finding C-5: ForwardRequest clones
// the whole request and then deletes the hop-by-hop headers one at a time plus
// Accept-Encoding. Driving the real method through a stub RoundTripper keeps the
// clone and the delete loop in the measurement while leaving the wire out.
func BenchmarkUpstream_ForwardRequest(b *testing.B) {
	u := &UpstreamTransport{transport: benchRoundTripper{}}
	ctx := context.Background()

	b.ReportAllocs()
	for range b.N {
		resp, err := u.ForwardRequest(ctx, benchRequest())
		if err != nil {
			b.Fatalf("ForwardRequest: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// BenchmarkUpstream_ForwardRequest_ManyHeaders scales the header count. C-5's cost
// has two components — the clone, which is linear in header count, and the fixed
// delete loop — and a single-shape arm cannot tell them apart. The delta against the
// arm above is the clone's share.
func BenchmarkUpstream_ForwardRequest_ManyHeaders(b *testing.B) {
	u := &UpstreamTransport{transport: benchRoundTripper{}}
	ctx := context.Background()

	b.ReportAllocs()
	for range b.N {
		r := benchRequest()
		for i := range 20 {
			r.Header.Set("X-Bench-Header-"+string(rune('A'+i)), "value")
		}
		resp, err := u.ForwardRequest(ctx, r)
		if err != nil {
			b.Fatalf("ForwardRequest: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// benchUpstreamResponse builds a response with the header shape a real provider
// returns, so copyResponse's per-header work is measured on realistic input.
func benchUpstreamResponse(extra int) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Request-Id", "req_abc123")
	h.Set("Openai-Version", "2020-10-01")
	h.Set("Openai-Processing-Ms", "42")
	for i := range extra {
		h.Set("X-Extra-"+string(rune('A'+i)), "v")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

// BenchmarkCopyResponse is the arm for finding C-6: copyResponse re-canonicalizes
// header keys that are already canonical by going through Header().Add. The header
// count is the variable that matters, so both a typical provider response and a
// header-heavy one are measured.
func BenchmarkCopyResponse(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		w := httptest.NewRecorder()
		if err := copyResponse(w, benchUpstreamResponse(0), nil); err != nil {
			b.Fatalf("copyResponse: %v", err)
		}
	}
}

func BenchmarkCopyResponse_ManyHeaders(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		w := httptest.NewRecorder()
		if err := copyResponse(w, benchUpstreamResponse(20), nil); err != nil {
			b.Fatalf("copyResponse: %v", err)
		}
	}
}

// The two arms below are FIXTURE-ONLY controls. copyResponse mutates resp.Header
// (it deletes hop-by-hop headers), so the response cannot be hoisted out of the
// timed loop — which means every CopyResponse number above includes the cost of
// building its own fixture. Without these controls a reader would attribute the
// fixture's h.Set calls and the recorder to copyResponse and optimize the wrong
// thing; subtract these to get copyResponse's own cost.
func BenchmarkCopyResponse_FixtureOnly(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		w := httptest.NewRecorder()
		resp := benchUpstreamResponse(0)
		_ = w
		_ = resp
	}
}

func BenchmarkCopyResponse_FixtureOnly_ManyHeaders(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		w := httptest.NewRecorder()
		resp := benchUpstreamResponse(20)
		_ = w
		_ = resp
	}
}

// Same control for the ForwardRequest arms, for the same reason: ForwardRequest
// clones and mutates the request it is given, so benchRequest() runs inside the
// timed loop and its cost is included in every C-5 number.
func BenchmarkUpstream_FixtureOnly(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		r := benchRequest()
		_ = r
	}
}

func BenchmarkUpstream_FixtureOnly_ManyHeaders(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		r := benchRequest()
		for i := range 20 {
			r.Header.Set("X-Bench-Header-"+string(rune('A'+i)), "value")
		}
		_ = r
	}
}

// benchSSEBumpOptions wires the minimum bumpOptions handleSSEResponse needs with a
// REAL resolver carrying one buildable observe-only response hook. The hook must be
// buildable: an unbuildable one would make the strict arm refuse with 451 at entry
// and measure the refusal path instead of the streaming path.
func benchSSEBumpOptions(b *testing.B, strict bool) *bumpOptions {
	b.Helper()
	var runs atomic.Int64
	store := streampolicy.NewStore(streampolicy.Policy{
		Mode:           streampolicy.ModeChunkedAsync,
		ChunkBytes:     1024,
		HookTimeoutMs:  1000,
		MaxBufferBytes: 1 << 20,
		FailBehavior:   streampolicy.FailOpen,
	})
	reg := core.NewHookRegistry()
	reg.Register("counting", func(_ *core.HookConfig) (core.Hook, error) {
		return countingHook{runs: &runs}, nil
	})
	resolver := compliance.NewPolicyResolver([]core.HookConfig{{
		ID: "h-count", ImplementationID: "counting", Name: "counting-hook",
		Stage: "response", Enabled: true, FailBehavior: "fail-open",
		ApplicableIngress: []string{"ALL"},
	}}, reg, benchDiscardLogger())
	return &bumpOptions{
		policyResolver:       resolver,
		streamingPolicyStore: store,
		strictFailClosed:     strict,
		auditEmitter:         compliance.NewAuditEmitter(benchDiscardAuditWriter{}, benchDiscardLogger()),
	}
}

// benchDiscardAuditWriter drops every event. The recording writer used by the tests
// accumulates records across iterations, so its growing slice put amortized realloc
// cost inside the measurement — the B/op column moved +-200 B run to run and could
// not resolve a ~270 B effect. Discarding keeps the audit emit on the measured path
// (it is real per-request work) without the harness's own growth.
type benchDiscardAuditWriter struct{}

func (benchDiscardAuditWriter) Enqueue(audit.AuditEvent)    {}
func (benchDiscardAuditWriter) Flush(context.Context) error { return nil }
func (benchDiscardAuditWriter) Close(context.Context) error { return nil }

// BenchmarkHandleSSEResponse_Live_NonStrict is the finding C-19 arm for the agent's
// posture: the SSE path used to build the response pipeline twice per request (the
// scope routing, then the mode branch) with byte-identical arguments.
func BenchmarkHandleSSEResponse_Live_NonStrict(b *testing.B) {
	benchRunSSE(b, benchSSEBumpOptions(b, false))
}

// BenchmarkHandleSSEResponse_Live_Strict is the compliance-proxy appliance posture,
// where it was THREE builds: the strict entry guard discarded the pipeline it built,
// then the scope routing built it again, then the mode branch built it a third time.
func BenchmarkHandleSSEResponse_Live_Strict(b *testing.B) {
	benchRunSSE(b, benchSSEBumpOptions(b, true))
}

func benchRunSSE(b *testing.B, bo *bumpOptions) {
	logger := benchDiscardLogger()
	start := time.Now()
	b.ReportAllocs()
	for range b.N {
		rec := httptest.NewRecorder()
		respInput := &core.HookInput{Stage: "response", TargetHost: "api.openai.com", IngressType: "COMPLIANCE_PROXY"}
		auditInfo := &compliance.AuditInfo{TransactionID: "tx-bench"}
		audCtx := &requestAuditCtx{input: respInput, info: *auditInfo}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}
		handleSSEResponse(context.Background(), rec, resp, audCtx, respInput, auditInfo, bo, logger, start)
	}
}

// benchAdapterDomainEngine mirrors the tests' adapterDomainEngine for a *testing.B.
func benchAdapterDomainEngine(b *testing.B, adapterID string) *domain.Engine {
	b.Helper()
	eng := domain.NewEngine()
	if err := eng.Swap([]domain.InterceptionDomain{{
		ID: "dom-adapter", Name: "example", HostPattern: "api.example.com",
		HostMatchType: domain.HostMatchExact, DefaultPathAction: domain.PathActionProcess,
		AdapterID: adapterID, Enabled: true, Priority: 10,
	}}); err != nil {
		b.Fatalf("engine swap: %v", err)
	}
	return eng
}

// BenchmarkForwardHandler_NoHooks is the finding C-18 arm: an intercepted, adapter-
// matched request whose scope binds NO hooks. This is the production-common shape on a
// monitored host — most traffic matches a domain rule but only some scopes carry hooks —
// and it used to pay the full Tier 1+2+3 normalize decode on both the request and the
// response body, then discard both results because nothing consumed them.
//
// The audit row is still emitted, so this measures the skip and not a disabled path.
func BenchmarkForwardHandler_NoHooks(b *testing.B) {
	logger := benchDiscardLogger()
	areg := traffic.NewAdapterRegistry("bench")
	if err := areg.Register("openai-compat", func() traffic.Adapter { return &openai.Adapter{} }); err != nil {
		b.Fatalf("adapter register: %v", err)
	}
	bo := &bumpOptions{
		policyResolver:  compliance.NewPolicyResolver(nil, core.NewHookRegistry(), logger),
		auditEmitter:    compliance.NewAuditEmitter(benchDiscardAuditWriter{}, logger),
		domainEngine:    benchAdapterDomainEngine(b, "openai-compat"),
		adapterRegistry: areg,
	}
	rt := benchRoundTripper{}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, logger, bo)

	b.ReportAllocs()
	for range b.N {
		req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello there"}]}`))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkForwardHandler_WithHook is the finding C-9 arm. C-18's arm above measures the
// no-hooks shape, where the normalize call is now skipped entirely and C-9's log lines
// never run. This arm binds one approve-only hook so the normalize path — and therefore
// the two per-normalize log lines this finding demotes — is actually on it.
func BenchmarkForwardHandler_WithHook(b *testing.B) {
	logger := benchDiscardLogger()
	areg := traffic.NewAdapterRegistry("bench")
	if err := areg.Register("openai-compat", func() traffic.Adapter { return &openai.Adapter{} }); err != nil {
		b.Fatalf("adapter register: %v", err)
	}
	hreg := core.NewHookRegistry()
	hreg.Register("bench-approve", func(_ *core.HookConfig) (core.Hook, error) {
		return cannedHook{result: core.HookResult{HookID: "h-b", HookName: "bench", Decision: core.Approve}}, nil
	})
	bo := &bumpOptions{
		policyResolver: compliance.NewPolicyResolver([]core.HookConfig{{
			ID: "h-b", ImplementationID: "bench-approve", Name: "bench",
			Stage: "request", Enabled: true, FailBehavior: "fail-open",
			ApplicableIngress: []string{"ALL"},
		}}, hreg, logger),
		auditEmitter:    compliance.NewAuditEmitter(benchDiscardAuditWriter{}, logger),
		domainEngine:    benchAdapterDomainEngine(b, "openai-compat"),
		adapterRegistry: areg,
		// A REAL normalize registry, not nil. The two log lines this finding demotes
		// live inside runtimeNormalize's `if reg != nil` arm, so a nil registry takes
		// the adapter-extraction fallback and the arm measures a path that does not
		// contain the change at all. Same harness class as findings R-12 / C-5 / C-24.
		normalizeRegistry: normalize.BuildRegistry(),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: benchRoundTripper{}}, logger, bo)

	b.ReportAllocs()
	for range b.N {
		req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello there"}]}`))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkContextPlumbing_Ceiling is finding C-3's ceiling, measured before deciding
// whether the refactor is worth its risk. It performs exactly the four
// WithContext+WithValue pairs a bumped request pays — clientHello, PhaseSink, CPMarker,
// requestAuditCtx — on a realistic request, so its allocation count is the MOST the
// context-holder refactor could possibly recover. It deliberately does not model the
// holder alternative: the point is to price the current cost, not to prototype.
func BenchmarkContextPlumbing_Ceiling(b *testing.B) {
	sink := &traffic.PhaseSink{}
	hello := []byte("\x16\x03\x01\x00\x05fake-clienthello")
	b.ReportAllocs()
	for range b.N {
		r := benchRequest()
		r = r.WithContext(context.WithValue(r.Context(), clientHelloKey{}, hello))
		r = r.WithContext(traffic.WithPhaseSink(r.Context(), sink))
		r = r.WithContext(contextWithCPMarker(r.Context(), &CPMarker{}))
		r = r.WithContext(context.WithValue(r.Context(), requestAuditKey{}, &requestAuditCtx{}))
		benchSinkRequest = r
	}
}

// benchSinkRequest keeps the final request alive so escape analysis cannot delete the
// clones the arm exists to measure — the failure that made findings R-12 and C-24 report
// zero allocations for work that really happens in production.
var benchSinkRequest *http.Request

// BenchmarkContextPlumbing_FixtureOnly subtracts benchRequest()'s own cost, since the
// request must be built inside the loop (WithContext mutates nothing but the arm needs a
// fresh base each iteration to avoid measuring an ever-deepening context chain).
func BenchmarkContextPlumbing_FixtureOnly(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		benchSinkRequest = benchRequest()
	}
}

// ---------------------------------------------------------------------------
// Finding C-11: the bumped path's request and response body reads.
//
// The arms above deliberately use a 72-byte request body and an EMPTY upstream response,
// because they were built to price header and pipeline work. Neither half of C-11 is
// measurable on them: at 72 bytes io.ReadAll's first 512-byte allocation already fits, and
// http.NoBody means the response read never happens at all. Measuring C-11 there would have
// reported "no win" for a harness reason — the fifth time this program would have made that
// mistake, so the arm is built to the shape the finding is actually about.
//
// A real bumped request on a monitored host is a chat completion: a system prompt plus a few
// turns, reliably several KiB, with a Content-Length the client already computed. The response
// is a completion of similar size. Both are what these arms carry.

// c11RequestBody builds a request body in the size class production actually sees. Built once
// at init rather than per iteration so the benchmark measures the read, not the construction.
var c11RequestBody = func() string {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-4o","messages":[{"role":"system","content":"`)
	sb.WriteString(strings.Repeat("You are a careful assistant. ", 120)) // ~3.4 KiB
	sb.WriteString(`"},{"role":"user","content":"`)
	sb.WriteString(strings.Repeat("Summarize the following paragraph. ", 100)) // ~3.4 KiB
	sb.WriteString(`"}]}`)
	return sb.String()
}()

var c11ResponseBody = func() string {
	var sb strings.Builder
	sb.WriteString(`{"id":"c-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"`)
	sb.WriteString(strings.Repeat("Here is the summary you asked for. ", 100))
	sb.WriteString(`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":900,"completion_tokens":700,"total_tokens":1600}}`)
	return sb.String()
}()

// c11RoundTripper answers with a realistic JSON completion AND a Content-Length, which is what
// the response half of the finding needs: benchRoundTripper's http.NoBody leaves that read
// unexercised, and a body without a declared length would only measure the fallback path.
type c11RoundTripper struct{}

func (c11RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(c11ResponseBody)),
		ContentLength: int64(len(c11ResponseBody)),
		Request:       req,
	}, nil
}

// BenchmarkForwardHandler_RealisticBodies is finding C-11's arm. One approve-only request hook
// is bound so the request normalize is on the path, and providerDetected plus the real response
// body put the response read on it too — so a single iteration pays both reads that C-11
// changes, at production sizes.
func BenchmarkForwardHandler_RealisticBodies(b *testing.B) {
	logger := benchDiscardLogger()
	areg := traffic.NewAdapterRegistry("bench")
	if err := areg.Register("openai-compat", func() traffic.Adapter { return &openai.Adapter{} }); err != nil {
		b.Fatalf("adapter register: %v", err)
	}
	hreg := core.NewHookRegistry()
	hreg.Register("bench-approve", func(_ *core.HookConfig) (core.Hook, error) {
		return cannedHook{result: core.HookResult{HookID: "h-b", HookName: "bench", Decision: core.Approve}}, nil
	})
	bo := &bumpOptions{
		policyResolver: compliance.NewPolicyResolver([]core.HookConfig{{
			ID: "h-b", ImplementationID: "bench-approve", Name: "bench",
			Stage: "request", Enabled: true, FailBehavior: "fail-open",
			ApplicableIngress: []string{"ALL"},
		}}, hreg, logger),
		auditEmitter:      compliance.NewAuditEmitter(benchDiscardAuditWriter{}, logger),
		domainEngine:      benchAdapterDomainEngine(b, "openai-compat"),
		adapterRegistry:   areg,
		normalizeRegistry: normalize.BuildRegistry(),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: c11RoundTripper{}}, logger, bo)

	b.ReportAllocs()
	for range b.N {
		req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
			strings.NewReader(c11RequestBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkReadBody_Isolated prices the read on its own, away from the ~200 allocations of
// pipeline and audit work the whole-handler arm carries. Both numbers are reported: the
// isolated one shows the change's real size, the handler one shows what fraction of a bumped
// request it actually is. Reporting only the isolated figure would overstate the finding.
func BenchmarkReadBody_Isolated(b *testing.B) {
	body := []byte(c11RequestBody)
	b.ReportAllocs()
	for range b.N {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		out, err := readBody(req, 10<<20)
		if err != nil || len(out) != len(body) {
			b.Fatalf("readBody = %d bytes, err %v", len(out), err)
		}
		benchBodySink = out
	}
}

// BenchmarkReadResponseBodyBounded_Isolated is the response half of the isolated pair.
func BenchmarkReadResponseBodyBounded_Isolated(b *testing.B) {
	body := []byte(c11ResponseBody)
	b.ReportAllocs()
	for range b.N {
		resp := &http.Response{
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
		}
		out, err := readResponseBodyBounded(resp, 10<<20)
		if err != nil || len(out) != len(body) {
			b.Fatalf("readResponseBodyBounded = %d bytes, err %v", len(out), err)
		}
		benchBodySink = out
	}
}

var benchBodySink []byte
