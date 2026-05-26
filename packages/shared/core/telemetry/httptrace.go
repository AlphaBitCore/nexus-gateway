package telemetry

import (
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
	propagator := otel.GetTextMapPropagator()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

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
var (
	_ http.Flusher        = (*statusWriter)(nil)
	_ http.ResponseWriter = (*statusWriter)(nil)
)
