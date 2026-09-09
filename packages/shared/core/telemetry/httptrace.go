package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// HTTPTrace returns middleware that creates a server span for each HTTP request.
// It extracts incoming trace context via the global TextMapPropagator and
// records standard HTTP semantic-convention attributes on each span.
func HTTPTrace(serviceName string) func(http.Handler) http.Handler {
	tracer := otel.Tracer(serviceName)
	// The global propagator carries whatever an operator configured (baggage
	// included) and is what continues the caller's trace into our spans.
	propagator := otel.GetTextMapPropagator()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			// A trace id belongs to the CALLER only when it arrived on an
			// inbound W3C traceparent, and this is the only point where that
			// is knowable. Extract yields a remote SpanContext exactly when
			// the header parsed, so a malformed traceparent drops out here on
			// its own; after tracer.Start the current SpanContext is this
			// server's own span, whose IsRemote is always false and whose
			// trace id — absent an inbound traceparent — is one the
			// IDGenerator derived from the request id (see idgen.go).
			// Persisting that derived value would file a Nexus-minted id in
			// the column that means "the caller's distributed trace".
			// Read the caller's trace id from traceparent DIRECTLY, not from
			// the context the global propagator just produced. Span
			// continuation may legitimately depend on whatever propagator an
			// operator configured; persisting trace_id must not — a service
			// whose telemetry.Init failed still has to record the trace the
			// caller sent. See traceContextOnly.
			if id := InboundTraceIDFromHeaders(r.Header); id != "" {
				ctx = WithInboundTraceID(ctx, id)
			}

			spanName := fmt.Sprintf("%s %s", r.Method, r.URL.Path)
			ctx, span := tracer.Start(ctx, spanName,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					semconv.HTTPRequestMethodKey.String(r.Method),
					semconv.URLPath(r.URL.Path),
					semconv.ServerAddress(r.Host),
				),
			)
			defer span.End()

			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r.WithContext(ctx))

			span.SetAttributes(semconv.HTTPResponseStatusCode(sw.status))
			if sw.status >= 400 {
				span.SetAttributes(attribute.Bool("error", true))
			}
		})
	}
}

// inboundTraceIDOf reads the caller's trace id out of a context that has just
// been through a propagator Extract and nothing else. Valid-and-remote is the
// exact test for "this came off the wire": a context with no traceparent
// yields an invalid span context, and a malformed one yields the same, so both
// fall out without a special case.
func inboundTraceIDOf(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && sc.IsRemote() {
		return sc.TraceID().String()
	}
	return ""
}

// traceContextOnly parses traceparent and nothing else.
//
// Deliberately NOT the global propagator. The global is installed as a side
// effect of telemetry.Init, which a service skips when its OTLP endpoint fails
// to build — it logs a warning and carries on. Reading traceparent is not a
// tracing feature, it is how traffic_event.trace_id gets its value, and it must
// not stop working because an exporter is misconfigured. This costs nothing:
// TraceContext is stateless.
var traceContextOnly = propagation.TraceContext{}

// InboundTraceIDFromHeaders reads the caller's W3C trace id straight off a set
// of request headers, for the services that intercept traffic rather than
// serve it and so never run HTTPTrace: the compliance proxy and the agent.
// They are passive — they record the trace the intercepted client was already
// carrying, and record nothing when it carried none.
func InboundTraceIDFromHeaders(h http.Header) string {
	ctx := traceContextOnly.Extract(context.Background(), propagation.HeaderCarrier(h))
	return inboundTraceIDOf(ctx)
}

// inboundTraceIDKey carries the caller's own trace id — set only when the
// request arrived with a parseable W3C traceparent.
type inboundTraceIDKey struct{}

// WithInboundTraceID records the trace id the caller supplied on traceparent.
func WithInboundTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, inboundTraceIDKey{}, traceID)
}

// InboundTraceID returns the caller's W3C trace id, or "" when the request
// carried no usable traceparent. Audit paths persist this — and only this — as
// the traffic row's trace id: an empty result means the caller runs no
// distributed tracing we can join to, which is the common case and must stay
// distinguishable from a trace id Nexus made up for its own span.
func InboundTraceID(ctx context.Context) string {
	id, _ := ctx.Value(inboundTraceIDKey{}).(string)
	return id
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.written {
		sw.status = code
		sw.written = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter. Without this method
// declared on the wrapper, the embedded interface's Flush is NOT in the
// wrapper's method set (Go does not promote methods declared only via
// type assertion), so any handler doing `w.(http.Flusher)` against a
// chain wrapped by HTTPTrace would see canFlush=false and silently fall
// back to a Content-Length-buffered response. That broke SSE for every
// AI Gateway streaming consumer (Claude Code rendered a blank UI).
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.ResponseController
// can reach it for SetWriteDeadline / Hijack / etc. Required by Go 1.20+
// to keep optional capabilities discoverable through middleware chains.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// Compile-time assertions that the wrapper exposes the same optional
// capabilities the underlying ResponseWriter has, so SSE / streaming
// downstream consumers can satisfy the type assertions they rely on.
//
// The Unwrap assertion carries the same weight as the Flusher one:
// http.ResponseController walks Unwrap() until it finds a writer implementing
// the capability it wants, so this wrapper's Unwrap is the only thing
// connecting a handler's SetWriteDeadline (and Hijack) to the real connection.
// Dropping it breaks nothing visible — the wrapper still satisfies
// http.ResponseWriter and the build stays green — while every response written
// after a long upstream call starts failing against the flat
// server.writeTimeout at runtime.
var (
	_ http.Flusher                              = (*statusWriter)(nil)
	_ http.ResponseWriter                       = (*statusWriter)(nil)
	_ interface{ Unwrap() http.ResponseWriter } = (*statusWriter)(nil)
)
