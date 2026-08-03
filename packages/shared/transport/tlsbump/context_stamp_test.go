package tlsbump

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// Finding C-3: the PhaseSink stamp moved out of prepare() and into stampCPMarker's existing
// clone, taking a bumped request from three http.Request clones to two.
//
// It is safe because nothing between those two points reads the sink OFF A CONTEXT — the
// in-package phases hold x.phaseSink directly, and the only context reader that matters is the
// tracing RoundTripper, which runs inside forwardUpstream. But "safe" was not "tested": deleting
// the stamp entirely left the WHOLE repository green. That is the finding-C-17 failure shape —
// the wiring goes nil, no error is raised, the stream still relays, the audit row still writes,
// and the upstream latency columns are quietly empty forever.
//
// The reason no test could see it is that the package's own harness builds
// &UpstreamTransport{transport: rt} directly, bypassing traffic.NewTracingTransport, which
// production wraps in upstream.go. So the sink had no populator in any test. This file supplies
// one.

// roundTripFunc adapts a func to http.RoundTripper. The package's existing doubles all fabricate
// their response from a maker and drop the request, and this test needs to inspect the request's
// CONTEXT as the transport sees it.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestBumpedRequest_PhaseSinkReachesTheAuditRow is the missing assertion. It wires the real
// tracing transport, so the sink is populated only if it actually arrived on the request context
// by the time forwardUpstream ran.
func TestBumpedRequest_PhaseSinkReachesTheAuditRow(t *testing.T) {
	writer := &recordingAuditWriter{}
	// A DELIBERATE 2 ms upstream. PhaseSink stores time.Since(sendStart).Milliseconds() and
	// TotalMs() returns nil for a value <= 0, so a sub-millisecond in-memory round trip is
	// indistinguishable from a sink that never arrived — which is how the first version of this
	// test failed against correct code, the seventh harness fault in this program. The quantity
	// under test is a millisecond-resolution duration, so the delay is a precondition for
	// observing it at all, not a race workaround.
	// openAIJSONUpstream, not jsonUpstream: the provider must be DETECTABLE so needBuffer is
	// true and the response body is read before the row is emitted. On the pure-relay path
	// (no response hooks AND no provider detected) the row is emitted before any body byte is
	// read, so the sink's Read-side timings are structurally unavailable there and this test
	// would fail against correct code — which is exactly how it first did.
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		time.Sleep(2 * time.Millisecond)
		return openAIJSONUpstream(), nil
	})

	areg := traffic.NewAdapterRegistry("test")
	if err := areg.Register("openai-compat", func() traffic.Adapter { return &openai.Adapter{} }); err != nil {
		t.Fatalf("adapter register: %v", err)
	}
	bo := &bumpOptions{
		policyResolver:  emptyResolver(t),
		auditEmitter:    compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:    adapterDomainEngine(t, "openai-compat"),
		adapterRegistry: areg,
	}
	// The production wrapping (upstream.go). Without it the sink has nothing to populate it and
	// this test would pass vacuously whether or not the stamp exists.
	up := &UpstreamTransport{transport: traffic.NewTracingTransport(rt)}
	h := buildForwardHandler(context.Background(), "api.example.com:443", up, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if events[0].UpstreamTotalMs == nil {
		t.Fatal("the audit row's UpstreamTotalMs is nil: the PhaseSink never reached the request " +
			"context, so the tracing transport had nothing to populate. Nothing errors on this " +
			"path — the request is relayed and the row is written, just with empty upstream " +
			"latency columns — which is why this is asserted on the row and not on a return value.")
	}
	if *events[0].UpstreamTotalMs <= 0 {
		t.Fatalf("UpstreamTotalMs = %d, want > 0", *events[0].UpstreamTotalMs)
	}
}

// TestAttestedPassthrough_KeepsTheClientHelloOnTheContext pins the constraint that stops the
// OTHER stamp from being merged, so a future tidy-up cannot quietly drop it. attestationPeek runs
// BEFORE prepare() and short-circuits to attestationPassthrough, which calls
// upstream.ForwardRequest itself — and the h2 dialer reads clientHelloKey off the request
// context. If the ClientHello stamp were moved later, attested traffic would lose TLS fingerprint
// replay silently: the request still succeeds, it just no longer looks like the real client.
func TestAttestedPassthrough_KeepsTheClientHelloOnTheContext(t *testing.T) {
	hello := []byte{0x16, 0x03, 0x01, 0xde, 0xad, 0xbe, 0xef}

	var seen []byte
	var sawRequest bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sawRequest = true
		seen, _ = r.Context().Value(clientHelloKey{}).([]byte)
		return jsonUpstream(), nil
	})

	bo := &bumpOptions{
		clientHelloRaw: hello,
		// A verifier that accepts, so the attested short-circuit is the path under test.
		attestationVerifier: func(context.Context, string) (bool, string) { return true, "agent-1" },
	}
	up := &UpstreamTransport{transport: rt}
	h := buildForwardHandler(context.Background(), "api.example.com:443", up, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set(AttestationHeaderName, "signed-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !sawRequest {
		t.Fatal("the upstream was never called: the attested passthrough path did not run, so " +
			"this test is not exercising the constraint it exists for")
	}
	if string(seen) != string(hello) {
		t.Fatalf("clientHelloKey on the attested upstream request = %v, want %v.\n"+
			"attestationPeek runs before prepare(), so the ClientHello stamp cannot be merged "+
			"into the later clone: the h2 dialer reads this value to replay the client's TLS "+
			"fingerprint, and losing it degrades attested traffic silently — the request still "+
			"succeeds, it just stops looking like the real client.", seen, hello)
	}
}

// Finding C-34. The stream-through arm — no response hooks AND no provider detected, i.e. the
// non-AI traffic a compliance proxy still audits — used to emit its audit row BEFORE relaying the
// body. Two columns are populated off the body read (PhaseSink stamps upstream TTFB on the first
// Read returning content and refreshes upstream-total on every Read), so both were NULL on every
// such row forever; and latency_ms was computed before the transfer, so a large download
// under-reported its own duration by the entire transfer time.
//
// The emission is now deferred to after the relay. These two tests pin both halves of why that is
// safe: the row gains the timings, and the row is still written when the relay panics.
func TestStreamThroughRow_CarriesUpstreamTimings(t *testing.T) {
	writer := &recordingAuditWriter{}
	// No adapter registry and no response hooks => providerDetected false, respPipeline nil =>
	// needBuffer false => the stream-through arm. jsonUpstream is deliberately the opaque body
	// here, which is what makes this arm the one under test.
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		time.Sleep(2 * time.Millisecond) // millisecond-resolution quantity; see the note above
		return jsonUpstream(), nil
	})
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:   adapterDomainEngine(t, "openai-compat"),
	}
	up := &UpstreamTransport{transport: traffic.NewTracingTransport(rt)}
	h := buildForwardHandler(context.Background(), "api.example.com:443", up, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
		strings.NewReader(`{"opaque":"not-an-ai-payload"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 — a deferred emission must not double-write", len(events))
	}
	ev := events[0]
	if ev.UpstreamTotalMs == nil {
		t.Fatal("the stream-through row's UpstreamTotalMs is nil. The row is being emitted before " +
			"the relay again, so the PhaseSink's Read-side timings have not landed yet. Nothing " +
			"errors on this path — the response still reaches the client and the row still writes, " +
			"just with permanently empty upstream latency columns.")
	}
	if *ev.UpstreamTotalMs <= 0 {
		t.Fatalf("UpstreamTotalMs = %d, want > 0", *ev.UpstreamTotalMs)
	}
	// latency_ms must now include the transfer, so it cannot be below the upstream total.
	if ev.LatencyMs < *ev.UpstreamTotalMs {
		t.Fatalf("latency_ms = %d but upstream_total_ms = %d. The row's own duration cannot be "+
			"shorter than the upstream exchange it contains; emitting before the relay is what "+
			"used to make that happen.", ev.LatencyMs, *ev.UpstreamTotalMs)
	}
}

// panicOnSecondReadBody relays some bytes and then panics, standing in for any fault inside the
// relay. The row must still be written, because that is the whole reason the emission is a defer
// rather than a call placed after relayResponse.
type panicOnSecondReadBody struct{ n int }

func (b *panicOnSecondReadBody) Read(p []byte) (int, error) {
	b.n++
	if b.n > 1 {
		panic("relay blew up mid-body")
	}
	return copy(p, []byte("partial")), nil
}
func (b *panicOnSecondReadBody) Close() error { return nil }

func TestStreamThroughRow_SurvivesAPanickingRelay(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/octet-stream"}},
			Body:          &panicOnSecondReadBody{},
			ContentLength: -1,
		}, nil
	})
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:   adapterDomainEngine(t, "openai-compat"),
	}
	up := &UpstreamTransport{transport: traffic.NewTracingTransport(rt)}
	h := buildForwardHandler(context.Background(), "api.example.com:443", up, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions",
		strings.NewReader(`{"opaque":"not-an-ai-payload"}`))
	req.Header.Set("Content-Type", "application/json")

	func() {
		// net/http recovers a handler panic per connection; this stands in for that so the
		// test observes what the audit writer got rather than dying with the handler.
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	if got := len(writer.snapshot()); got != 1 {
		t.Fatalf("audit events after a panicking relay = %d, want 1. Deferring the emission is only "+
			"acceptable because a defer runs while the panic unwinds — if this is a plain call "+
			"placed after relayResponse, a relay fault loses the audit row entirely, which on a "+
			"compliance product is far worse than an empty latency column.", got)
	}
}
