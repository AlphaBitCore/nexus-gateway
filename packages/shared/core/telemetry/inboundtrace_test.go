package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// A well-formed W3C traceparent: version-traceid-spanid-flags.
	validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	validTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
)

func init() {
	// HTTPTrace reads the globally registered propagator; without one, Extract
	// is a no-op and every case below would look like "caller sent nothing".
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// A REAL tracer provider, carrying the same IDGenerator production uses.
	// Without it every span is non-recording with an invalid span context, and
	// the negative cases below pass for the wrong reason: a capture written in
	// the wrong place would read that invalid context, find nothing, and look
	// correct. With it, a server span always has a valid trace id — derived
	// from the request id when the caller sent no traceparent — which is
	// exactly the value these tests must prove never reaches the column.
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(requestIDGenerator{}),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	))
}

// TestInboundTraceID_OnlyWhenTheCallerSentOne is the load-bearing test for the
// trace_id column's meaning. That column answers "which of the caller's traces
// does this row belong to", so it must hold a value only when the caller
// actually had one. The negative cases are the point: with no traceparent the
// tracer still assigns the server span a trace id (derived from the request id
// by the IDGenerator), and persisting THAT would fill a column meaning "the
// caller's trace" with an id Nexus invented — indistinguishable, downstream,
// from a real customer trace.
func TestInboundTraceID_OnlyWhenTheCallerSentOne(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
		want        string
		why         string
	}{
		{
			name:        "valid traceparent is captured",
			traceparent: validTraceparent,
			want:        validTraceID,
			why:         "the caller runs tracing; their trace id is what makes the row joinable to their APM",
		},
		{
			name:        "no traceparent captures nothing",
			traceparent: "",
			want:        "",
			why:         "the common case: no caller trace exists, so the column must stay empty",
		},
		{
			name:        "malformed traceparent captures nothing",
			traceparent: "not-a-traceparent",
			want:        "",
			why:         "Extract yields an invalid span context, which must not be mistaken for a real trace",
		},
		{
			name:        "all-zero trace id is not a trace",
			traceparent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
			want:        "",
			why:         "the W3C spec forbids an all-zero trace id; treating it as valid would store a meaningless key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := HTTPTrace("test-svc")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = InboundTraceID(r.Context())
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.traceparent != "" {
				req.Header.Set("traceparent", tc.traceparent)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Errorf("InboundTraceID = %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestInboundTraceID_NotTheServerSpansOwnTrace pins the reason the capture sits
// between Extract and tracer.Start. Inside the handler the current span context
// is the server's OWN span, which always carries a valid, non-remote trace id.
// A capture written there would return a value for every request, and the
// negative cases above would silently start passing for the wrong reason.
func TestInboundTraceID_NotTheServerSpansOwnTrace(t *testing.T) {
	var inbound, serverSpanTrace string
	var spanTraceID trace.TraceID
	h := HTTPTrace("test-svc")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inbound = InboundTraceID(r.Context())
		serverSpanTrace = inboundTraceIDOf(r.Context())
		spanTraceID = trace.SpanContextFromContext(r.Context()).TraceID()
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if inbound != "" {
		t.Errorf("InboundTraceID = %q, want empty: this request carried no traceparent", inbound)
	}
	// The server span exists, but it is not remote, so the helper reports
	// nothing for it — which is exactly why the capture must run earlier.
	if serverSpanTrace != "" {
		t.Errorf("inboundTraceIDOf inside the handler = %q; a non-remote span must never be reported as the caller's trace", serverSpanTrace)
	}
	// Guard the guard: if the tracer were a no-op, the server span would carry
	// no trace id at all and the two checks above would hold for a reason that
	// has nothing to do with the code under test.
	if !spanTraceID.IsValid() {
		t.Fatal("the server span carries no trace id — the tracer provider is not installed, so this test proves nothing")
	}
}

// TestInboundTraceIDFromHeaders_ForInterceptingServices covers the passive
// path: the compliance proxy and the agent never run HTTPTrace, so they read
// the intercepted client's trace straight off the headers. Same contract —
// record what the client was carrying, record nothing otherwise.
func TestInboundTraceIDFromHeaders_ForInterceptingServices(t *testing.T) {
	withHeader := http.Header{}
	withHeader.Set("traceparent", validTraceparent)
	if got := InboundTraceIDFromHeaders(withHeader); got != validTraceID {
		t.Errorf("InboundTraceIDFromHeaders = %q, want %q", got, validTraceID)
	}

	if got := InboundTraceIDFromHeaders(http.Header{}); got != "" {
		t.Errorf("InboundTraceIDFromHeaders on a bare request = %q, want empty — a passive interception point invents nothing", got)
	}

	garbled := http.Header{}
	garbled.Set("traceparent", "00-tooshort-00f067aa0ba902b7-01")
	if got := InboundTraceIDFromHeaders(garbled); got != "" {
		t.Errorf("InboundTraceIDFromHeaders on a malformed header = %q, want empty", got)
	}
}

// TestWithInboundTraceID_RoundTrips keeps the context pair honest: a reader on
// a context that was never stamped must get the zero value rather than panic on
// the type assertion.
func TestWithInboundTraceID_RoundTrips(t *testing.T) {
	if got := InboundTraceID(context.Background()); got != "" {
		t.Errorf("InboundTraceID on a bare context = %q, want empty", got)
	}
	ctx := WithInboundTraceID(context.Background(), validTraceID)
	if got := InboundTraceID(ctx); got != validTraceID {
		t.Errorf("InboundTraceID round-trip = %q, want %q", got, validTraceID)
	}
}

// TestInboundTraceID_SurvivesAMisconfiguredGlobalPropagator pins the reason the
// capture parses traceparent itself instead of reusing the global propagator.
//
// The global is installed as a side effect of Init. A service whose OTLP
// endpoint fails to build never gets that far — Init returns an error and the
// caller logs a warning and carries on serving. Reading traceparent is not a
// tracing feature: it is where traffic_event.trace_id comes from, and a broken
// exporter must not silently empty that column for every request.
func TestInboundTraceID_SurvivesAMisconfiguredGlobalPropagator(t *testing.T) {
	restore := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(restore) })

	// Exactly what a process that never completed Init has.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	h := http.Header{}
	h.Set("traceparent", validTraceparent)
	if got := InboundTraceIDFromHeaders(h); got != validTraceID {
		t.Errorf("InboundTraceIDFromHeaders = %q, want %q — the capture must not depend on the global propagator", got, validTraceID)
	}

	var viaMiddleware string
	mw := HTTPTrace("test-svc")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		viaMiddleware = InboundTraceID(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("traceparent", validTraceparent)
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if viaMiddleware != validTraceID {
		t.Errorf("InboundTraceID through HTTPTrace = %q, want %q — the server-span path must capture it too", viaMiddleware, validTraceID)
	}
}
