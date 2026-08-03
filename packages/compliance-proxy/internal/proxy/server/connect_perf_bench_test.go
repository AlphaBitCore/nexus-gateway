package server

import (
	"io"
	"log/slog"
	"net"
	"testing"
)

// benchSink keeps results escaping so the compiler cannot delete the work being
// measured. Without it the with-port host/port arm reported ZERO allocations,
// because JoinHostPort's result was discarded and never escaped — in production
// targetHost is used for the rest of the CONNECT and does escape.
var (
	benchSinkString string
	benchSinkLogger *slog.Logger
)

// Per-CONNECT cost benchmarks (finding C-24).
//
// C-24 was filed as "per-CONNECT costs in compliance-proxy's own server — uuid,
// logger.With, WithContext, BuildPipeline — effectively per-request for
// one-tunnel-per-exchange clients". Three of those four are the same constructs
// already priced elsewhere in this program, so re-measuring them here would just
// double-count:
//
//	uuid.New().String()  1 allocation      — same construct as C-7, measured there
//	r.WithContext(...)   2 allocations      — same construct as C-3, measured there
//	BuildPipeline        11 allocations     — measured in session 1's
//	                                          policy_buildpipeline_bench_test.go
//
// What is NOT priced anywhere else is the connection-scoped logger derivation and
// the host/port normalization, both of which are local to this handler. Those are
// the arms below.
//
// Run these the same way as the tlsbump arms: one process per arm, arm order rotated
// each round, benchstat over two files, plus a same-minute after-vs-after null
// control. `go test -bench -count=N` does not interleave sub-benchmarks, and this
// host's ns/op is unreliable — trust allocs/op and B/op.

func benchConnectLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// BenchmarkConnect_LoggerWith prices the connection-scoped logger every CONNECT
// derives. It is a real cost rather than a diagnostic that can be demoted: the
// shared SlogSink lifts trace_id out of these attrs into DiagEvent.TraceID, so every
// thing_diag_event row emitted during the CONNECT carries the typed correlation
// column. Removing it would drop that column, not just a log line.
func BenchmarkConnect_LoggerWith(b *testing.B) {
	logger := benchConnectLogger()
	b.ReportAllocs()
	for range b.N {
		benchSinkLogger = logger.With(
			"source", "203.0.113.7:54321",
			"target", "api.openai.com:443",
			"trace_id", "3f7c1e90-8a2b-4c5d-9e6f-0a1b2c3d4e5f",
		)
	}
}

// BenchmarkConnect_LoggerWithTypedAttrs was the candidate fix for the arm above, and
// it MEASURES WORSE — 13 allocations / 776 B against 10 / 632 B. Kept so the result is
// reproducible rather than remembered.
//
// The reasoning that suggested it: alternating key-value pairs reach With as ...any, so
// each of the six values is boxed, and "pass typed attrs instead" is the standard slog
// advice. The reason it fails here: Logger.With only accepts ...any, so a slog.Attr —
// a struct with a multi-field Value — gets boxed too, and boxing an Attr costs MORE
// than boxing a string. The advice is real but applies to LogAttrs(...slog.Attr),
// which takes attrs directly and boxes nothing; slog offers no With(...slog.Attr).
func BenchmarkConnect_LoggerWithTypedAttrs(b *testing.B) {
	logger := benchConnectLogger()
	b.ReportAllocs()
	for range b.N {
		benchSinkLogger = logger.With(
			slog.String("source", "203.0.113.7:54321"),
			slog.String("target", "api.openai.com:443"),
			slog.String("trace_id", "3f7c1e90-8a2b-4c5d-9e6f-0a1b2c3d4e5f"),
		)
	}
}

// BenchmarkConnect_HostPortNormalize prices the SplitHostPort + JoinHostPort pair
// that turns r.Host into the canonical target. Both spellings a client can send are
// measured, because the with-port case short-circuits nothing — JoinHostPort still
// rebuilds the string.
func BenchmarkConnect_HostPortNormalize(b *testing.B) {
	b.Run("shape=with-port", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			host, port, err := net.SplitHostPort("api.openai.com:443")
			if err != nil {
				b.Fatalf("SplitHostPort: %v", err)
			}
			benchSinkString = net.JoinHostPort(host, port)
		}
	})
	b.Run("shape=bare-host", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			host, port, err := net.SplitHostPort("api.openai.com")
			if err != nil {
				// The handler's own fallback: bare host defaults to 443.
				host, port = "api.openai.com", "443"
			}
			benchSinkString = net.JoinHostPort(host, port)
		}
	})
}
