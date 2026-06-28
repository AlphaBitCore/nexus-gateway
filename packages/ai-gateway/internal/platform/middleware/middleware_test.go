package middleware

import (
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// TestRequestID_StampsHeaderAndContext asserts that RequestID:
//   - Sets x-nexus-request-id on the response.
//   - Mirrors the same value into the request header (so downstream
//     handlers can read it without consulting the response writer).
//   - Stashes the same ID into the request context via
//     nexushttp.WithRequestID.
func TestRequestID_StampsHeaderAndContext(t *testing.T) {
	var (
		ctxID    string
		reqHdrID string
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = nexushttp.RequestIDFromContext(r.Context())
		reqHdrID = r.Header.Get("X-Nexus-Request-Id")
		w.WriteHeader(http.StatusOK)
	})
	h := RequestID(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	respID := w.Header().Get("X-Nexus-Request-Id")
	if respID == "" {
		t.Fatal("response X-Nexus-Request-Id not set")
	}
	if reqHdrID != respID {
		t.Fatalf("req header id = %q, resp header id = %q; want identical", reqHdrID, respID)
	}
	if ctxID != respID {
		t.Fatalf("ctx id = %q, resp header id = %q; want identical", ctxID, respID)
	}
}

// TestRequestID_PreservesClientRequestID covers the
// `if clientID := r.Header.Get("X-Request-Id"); clientID != ""` branch:
// a client-supplied X-Request-Id must be mirrored into the response so
// audit pipelines can correlate it.
func TestRequestID_PreservesClientRequestID(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := RequestID(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-Id", "audit-corr-42")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-Id"); got != "audit-corr-42" {
		t.Fatalf("response X-Request-Id = %q, want client-supplied value preserved", got)
	}
	// Nexus-managed ID must still be set independently.
	if w.Header().Get("X-Nexus-Request-Id") == "" {
		t.Fatal("X-Nexus-Request-Id also expected")
	}
}

// newCaptureLogger returns an slog.Logger writing text-format records into
// an in-memory buffer so tests can assert level routing and attribute
// presence without depending on stdout.
func newCaptureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// TestLogger_LevelRouting_2xx_4xx_5xx asserts the Logger middleware:
//   - Logs at DEBUG for 2xx (redundant with the traffic_event audit row and the
//     Prometheus request/latency series; kept off the Info hot path).
//   - Logs at WARN for 4xx.
//   - Logs at ERROR for 5xx.
//   - Logs at DEBUG for /healthz and /metrics regardless of status.
func TestLogger_LevelRouting_2xx_4xx_5xx(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		status    int
		wantLevel string
	}{
		{"2xx routes to DEBUG", "/v1/models", http.StatusOK, "DEBUG"},
		{"4xx routes to WARN", "/v1/models", http.StatusBadRequest, "WARN"},
		{"5xx routes to ERROR", "/v1/models", http.StatusInternalServerError, "ERROR"},
		{"healthz routes to DEBUG", "/healthz", http.StatusOK, "DEBUG"},
		{"metrics routes to DEBUG", "/metrics", http.StatusOK, "DEBUG"},
		// 4xx on a probe path stays DEBUG (the path takes precedence).
		{"healthz 503 still DEBUG", "/healthz", http.StatusServiceUnavailable, "DEBUG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newCaptureLogger()
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			h := Logger(logger)(next)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if !strings.Contains(buf.String(), "level="+tc.wantLevel) {
				t.Fatalf("log output %q missing level=%s", buf.String(), tc.wantLevel)
			}
			// Sanity: path attr present.
			if !strings.Contains(buf.String(), "path="+tc.path) {
				t.Fatalf("log output missing path=%s; got %q", tc.path, buf.String())
			}
		})
	}
}

// TestLogger_DefaultStatusIs200 — when the inner handler writes a body
// without an explicit WriteHeader, statusWriter retains 200 and the log
// record reflects status=200.
func TestLogger_DefaultStatusIs200(t *testing.T) {
	logger, buf := newCaptureLogger()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	h := Logger(logger)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(buf.String(), "status=200") {
		t.Fatalf("log missing status=200; got %q", buf.String())
	}
}

// TestRecovery_CatchesPanic_Returns500 asserts Recovery converts a
// downstream panic into a 500 response with the canonical error body and
// logs the panic at ERROR level.
func TestRecovery_CatchesPanic_Returns500(t *testing.T) {
	logger, buf := newCaptureLogger()
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("simulated downstream boom")
	})
	h := Recovery(logger)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"error":"internal server error"`) {
		t.Fatalf("body = %q, want canonical JSON error envelope", w.Body.String())
	}
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Fatalf("log missing panic-recovered message; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "simulated downstream boom") {
		t.Fatalf("log missing panic payload; got %q", buf.String())
	}
}

// TestRecovery_NoPanic_PassesThrough — when the inner handler does not
// panic, Recovery must transparently forward its response.
func TestRecovery_NoPanic_PassesThrough(t *testing.T) {
	logger, _ := newCaptureLogger()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "tea")
	})
	h := Recovery(logger)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", w.Code)
	}
	if w.Body.String() != "tea" {
		t.Fatalf("body = %q, want 'tea'", w.Body.String())
	}
}

// TestCORS_AllowedMethodsAndHeaders_AreEchoed — the
// `len(cfg.AllowedMethods) > 0` and `len(cfg.AllowedHeaders) > 0`
// branches: caller-supplied lists override the defaults.
func TestCORS_AllowedMethodsAndHeaders_AreEchoed(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := CORS(CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "POST"},
		AllowedHeaders: []string{"X-Foo", "X-Bar"},
	})(inner)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Fatalf("ACAM = %q, want 'GET, POST'", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "X-Foo, X-Bar" {
		t.Fatalf("ACAH = %q, want 'X-Foo, X-Bar'", got)
	}
}

// TestStatusWriter_Flush forwards Flush to the underlying ResponseWriter
// when it implements http.Flusher.
func TestStatusWriter_Flush(t *testing.T) {
	flushable := &flushableRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: flushable, status: 200}
	sw.Flush()
	if !flushable.flushed {
		t.Fatal("Flush did not propagate to underlying Flusher")
	}
}

// TestStatusWriter_Flush_NonFlushableNoop — when the underlying writer
// does not implement http.Flusher, Flush is a no-op (no panic).
func TestStatusWriter_Flush_NonFlushableNoop(t *testing.T) {
	sw := &statusWriter{ResponseWriter: nonFlushableWriter{}, status: 200}
	sw.Flush() // must not panic / explode
}

// TestStatusWriter_Unwrap returns the underlying writer so
// http.ResponseController can reach it.
func TestStatusWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: 200}
	if got := sw.Unwrap(); got != rec {
		t.Fatalf("Unwrap() returned %T, want the original ResponseRecorder", got)
	}
}

// TestStatusWriter_WriteHeader_RecordsCode — covered indirectly by the
// Logger tests, but assert here explicitly for safety: the status field
// is updated AND the underlying writer is notified.
func TestStatusWriter_WriteHeader_RecordsCode(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: 200}
	sw.WriteHeader(http.StatusAccepted)
	if sw.status != http.StatusAccepted {
		t.Fatalf("sw.status = %d, want 202", sw.status)
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("underlying writer Code = %d, want 202 propagated", rec.Code)
	}
}

// TestClientIP_XForwardedFor_SingleEntryNoComma covers the
// `else return strings.TrimSpace(xff)` branch (no comma in XFF).
func TestClientIP_XForwardedFor_SingleEntryNoComma(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "  203.0.113.7  ") // padded; TrimSpace
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want 203.0.113.7", got)
	}
}

// TestClientIP_RemoteAddrWithoutPort covers the err branch of
// net.SplitHostPort: when RemoteAddr lacks a port, ClientIP returns the
// raw RemoteAddr.
func TestClientIP_RemoteAddrWithoutPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.55" // no port → SplitHostPort errors
	if got := ClientIP(r); got != "203.0.113.55" {
		t.Fatalf("ClientIP = %q, want raw RemoteAddr fallback", got)
	}
}

// TestTLSInfoFromRequest_WithTLS_PopulatesSNI covers the populated path.
func TestTLSInfoFromRequest_WithTLS_PopulatesSNI(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{ServerName: "api.example.com"}
	info := tlsInfoFromRequest(r)
	if info == nil {
		t.Fatal("tlsInfoFromRequest returned nil despite r.TLS being set")
	}
	if info.SNI != "api.example.com" {
		t.Fatalf("SNI = %q, want api.example.com", info.SNI)
	}
	// ClientCertFingerprint left empty per the documented contract.
	var zero hooks.TLSInfo
	zero.SNI = "api.example.com"
	if info.ClientCertFingerprint != zero.ClientCertFingerprint {
		t.Fatalf("ClientCertFingerprint = %q, want empty", info.ClientCertFingerprint)
	}
}

// flushableRecorder wraps httptest.ResponseRecorder so it satisfies
// http.Flusher and records that Flush was invoked.
type flushableRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushableRecorder) Flush() {
	f.flushed = true
}

// nonFlushableWriter implements only http.ResponseWriter — used to confirm
// statusWriter.Flush is a no-op when the underlying writer is not a Flusher.
type nonFlushableWriter struct{}

func (nonFlushableWriter) Header() http.Header         { return http.Header{} }
func (nonFlushableWriter) Write(p []byte) (int, error) { return len(p), nil }
func (nonFlushableWriter) WriteHeader(_ int)           {}
