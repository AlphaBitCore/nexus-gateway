package http

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// captureLogger returns a *slog.Logger writing JSON records to the
// returned buffer at LevelDebug.
func captureLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

// records parses the logger buffer into one map per emitted record.
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestRoundTripper_Success_EmitsAllFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	logger, buf := captureLogger(t)
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, WrapOpts{
		Logger: logger,
		Caller: "test-caller",
	})}

	body := strings.NewReader(`{"x":1}`)
	req, _ := http.NewRequestWithContext(WithRequestID(context.Background(), "req-xyz"), http.MethodPost, srv.URL+"/foo", body)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d (%v)", len(recs), recs)
	}
	r := recs[0]
	if r["msg"] != "outbound http" {
		t.Errorf("msg: %v", r["msg"])
	}
	if r["level"] != "DEBUG" {
		t.Errorf("level: %v", r["level"])
	}
	if r["caller"] != "test-caller" {
		t.Errorf("caller: %v", r["caller"])
	}
	if r["method"] != "POST" {
		t.Errorf("method: %v", r["method"])
	}
	if r["host"] == nil || !strings.Contains(r["url"].(string), "/foo") {
		t.Errorf("url/host: %v / %v", r["url"], r["host"])
	}
	if r["status"].(float64) != 200 {
		t.Errorf("status: %v", r["status"])
	}
	if r["nexus_request_id"] != "req-xyz" {
		t.Errorf("nexus_request_id: %v", r["nexus_request_id"])
	}
	if r["resp_bytes"].(float64) != 5 {
		t.Errorf("resp_bytes: %v want 5", r["resp_bytes"])
	}
	if _, hasErr := r["err"]; hasErr {
		t.Errorf("err must be absent on success, got %v", r["err"])
	}
	if _, hasProto := r["proto"]; !hasProto {
		t.Errorf("proto missing")
	}
}

type errorTransport struct{ err error }

func (e errorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestRoundTripper_TransportError_LogsAtWarn(t *testing.T) {
	logger, buf := captureLogger(t)
	rt := WrapTransport(errorTransport{err: errors.New("boom")}, WrapOpts{Logger: logger, Caller: "x"})

	req, _ := http.NewRequestWithContext(WithRequestID(context.Background(), "req-err"), http.MethodGet, "http://example.invalid/", nil)
	resp, err := rt.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil || err.Error() != "boom" {
		t.Fatalf("RoundTrip err: %v", err)
	}

	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d (%v)", len(recs), recs)
	}
	r := recs[0]
	if r["level"] != "WARN" {
		t.Errorf("level: %v want WARN", r["level"])
	}
	if r["status"].(float64) != 0 {
		t.Errorf("status: %v want 0", r["status"])
	}
	if r["err"] != "boom" {
		t.Errorf("err: %v", r["err"])
	}
	if r["nexus_request_id"] != "req-err" {
		t.Errorf("nexus_request_id: %v", r["nexus_request_id"])
	}
}

type captureHeaderTransport struct{ got http.Header }

func (c *captureHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req.Header.Clone()
	return &http.Response{StatusCode: http.StatusNoContent, Proto: "HTTP/1.1", Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestPropagateReqID_True_AddsHeader(t *testing.T) {
	capRT := &captureHeaderTransport{}
	logger, _ := captureLogger(t)
	rt := WrapTransport(capRT, WrapOpts{Logger: logger, Caller: "x", PropagateReqID: true})
	req, _ := http.NewRequestWithContext(WithRequestID(context.Background(), "req-prop"), http.MethodGet, "http://x/", nil)
	resp, _ := rt.RoundTrip(req)
	_ = resp.Body.Close()
	if got := capRT.got.Get("X-Nexus-Request-Id"); got != "req-prop" {
		t.Errorf("X-Nexus-Request-Id outbound: got %q, want %q", got, "req-prop")
	}
}

func TestPropagateReqID_False_NoHeader(t *testing.T) {
	capRT := &captureHeaderTransport{}
	logger, _ := captureLogger(t)
	rt := WrapTransport(capRT, WrapOpts{Logger: logger, Caller: "x", PropagateReqID: false})
	req, _ := http.NewRequestWithContext(WithRequestID(context.Background(), "req-prop"), http.MethodGet, "http://x/", nil)
	resp, _ := rt.RoundTrip(req)
	_ = resp.Body.Close()
	if got := capRT.got.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id outbound: got %q, want empty", got)
	}
}

func TestPropagateReqID_DoesNotOverwriteExisting(t *testing.T) {
	capRT := &captureHeaderTransport{}
	logger, _ := captureLogger(t)
	rt := WrapTransport(capRT, WrapOpts{Logger: logger, Caller: "x", PropagateReqID: true})
	req, _ := http.NewRequestWithContext(WithRequestID(context.Background(), "from-ctx"), http.MethodGet, "http://x/", nil)
	req.Header.Set("X-Nexus-Request-Id", "preset")
	resp, _ := rt.RoundTrip(req)
	_ = resp.Body.Close()
	if got := capRT.got.Get("X-Nexus-Request-Id"); got != "preset" {
		t.Errorf("X-Nexus-Request-Id outbound: got %q, want preset", got)
	}
}

func TestPropagateReqID_True_EmptyCtxNoHeader(t *testing.T) {
	capRT := &captureHeaderTransport{}
	logger, _ := captureLogger(t)
	rt := WrapTransport(capRT, WrapOpts{Logger: logger, Caller: "x", PropagateReqID: true})
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil) // no WithRequestID
	resp, _ := rt.RoundTrip(req)
	_ = resp.Body.Close()
	if got := capRT.got.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id outbound: got %q, want empty (no id in ctx)", got)
	}
}

type nilBodyTransport struct{}

func (nilBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Proto: "HTTP/1.1", Body: nil}, nil
}

func TestRoundTripper_NilResponseBody_DoesNotCrash(t *testing.T) {
	logger, buf := captureLogger(t)
	rt := WrapTransport(nilBodyTransport{}, WrapOpts{Logger: logger, Caller: "x"})
	req, _ := http.NewRequest(http.MethodHead, "http://x/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip err: %v", err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.Body != nil {
		t.Errorf("resp.Body: want nil (preserved), got %T", resp.Body)
	}
	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r["status"].(float64) != 204 {
		t.Errorf("status: %v want 204", r["status"])
	}
	if r["resp_bytes"].(float64) != 0 {
		t.Errorf("resp_bytes: %v want 0", r["resp_bytes"])
	}
	if r["level"] != "DEBUG" {
		t.Errorf("level: %v want DEBUG", r["level"])
	}
}

// readWriteCloserBody satisfies io.ReadWriteCloser — the interface the
// coder/websocket library downcasts resp.Body to after the HTTP 101 upgrade
// hands off the underlying connection for duplex framing. Used to verify the
// logging transport leaves a 101 response body untouched.
type readWriteCloserBody struct {
	io.Reader
	writes [][]byte
	closed atomic.Bool
}

func (b *readWriteCloserBody) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	b.writes = append(b.writes, cp)
	return len(p), nil
}
func (b *readWriteCloserBody) Close() error { b.closed.Store(true); return nil }

type switchingProtocolsTransport struct{ body *readWriteCloserBody }

func (st switchingProtocolsTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusSwitchingProtocols,
		Proto:      "HTTP/1.1",
		Body:       st.body,
	}, nil
}

func TestRoundTripper_SwitchingProtocols_PreservesBody(t *testing.T) {
	// Regression: with debug enabled, the transport used to wrap resp.Body
	// in loggingBody (io.ReadCloser only), which broke coder/websocket's
	// resp.Body.(io.ReadWriteCloser) downcast and produced a noisy
	// "response body is not a io.ReadWriteCloser" failure on every WS dial.
	// The fix returns the body untouched for status 101 and emits the
	// success record up front.
	rwc := &readWriteCloserBody{Reader: strings.NewReader("")}
	logger, buf := captureLogger(t)
	rt := WrapTransport(switchingProtocolsTransport{body: rwc}, WrapOpts{Logger: logger, Caller: "ws"})
	req, _ := http.NewRequest(http.MethodGet, "http://x/ws", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip err: %v", err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if _, isLogging := resp.Body.(*loggingBody); isLogging {
		t.Fatal("resp.Body must NOT be wrapped in loggingBody on HTTP 101")
	}
	if _, ok := resp.Body.(io.ReadWriteCloser); !ok {
		t.Fatalf("resp.Body lost io.ReadWriteCloser interface, type=%T", resp.Body)
	}
	if _, err := resp.Body.(io.Writer).Write([]byte("ping")); err != nil {
		t.Fatalf("Write through preserved body: %v", err)
	}
	if got := len(rwc.writes); got != 1 || string(rwc.writes[0]) != "ping" {
		t.Fatalf("underlying body did not receive Write; writes=%v", rwc.writes)
	}
	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 log record, got %d", len(recs))
	}
	r := recs[0]
	if r["status"].(float64) != float64(http.StatusSwitchingProtocols) {
		t.Errorf("status: %v want 101", r["status"])
	}
	if r["resp_bytes"].(float64) != 0 {
		t.Errorf("resp_bytes: %v want 0", r["resp_bytes"])
	}
	if r["level"] != "DEBUG" {
		t.Errorf("level: %v want DEBUG", r["level"])
	}
}

// markerBody flips a flag the first time Read is called so the test can
// assert whether the response body was wrapped.
type markerBody struct {
	io.Reader
	read   atomic.Bool
	closed atomic.Bool
}

func (m *markerBody) Read(p []byte) (int, error) {
	m.read.Store(true)
	return m.Reader.Read(p)
}
func (m *markerBody) Close() error { m.closed.Store(true); return nil }

type markerTransport struct{ body *markerBody }

func (mt *markerTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Proto: "HTTP/1.1", Body: mt.body}, nil
}

func TestDebugDisabled_DoesNotWrapBody(t *testing.T) {
	mb := &markerBody{Reader: strings.NewReader("payload")}
	mt := &markerTransport{body: mb}
	infoLogger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rt := WrapTransport(mt, WrapOpts{Logger: infoLogger, Caller: "x"})
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, isLogging := resp.Body.(*loggingBody); isLogging {
		t.Fatal("resp.Body should NOT be wrapped when debug disabled")
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if !mb.read.Load() || !mb.closed.Load() {
		t.Fatal("marker body Read/Close not called")
	}
}

func TestDebugDisabled_TransportErrorStillLogsAtWarn(t *testing.T) {
	var buf bytes.Buffer
	infoLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rt := WrapTransport(errorTransport{err: errors.New("boom")}, WrapOpts{Logger: infoLogger, Caller: "x"})
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	resp, err := rt.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(buf.String(), `"level":"WARN"`) || !strings.Contains(buf.String(), `"err":"boom"`) {
		t.Fatalf("warn/err missing: %s", buf.String())
	}
}

func TestReqBytes_FromContentLength(t *testing.T) {
	// httptest server reads the body so the wrapper sees the full request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger, buf := captureLogger(t)
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, WrapOpts{Logger: logger, Caller: "x"})}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader("12345"))
	resp, _ := client.Do(req)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	r := records(t, buf)[0]
	if r["req_bytes"].(float64) != 5 {
		t.Errorf("req_bytes: %v want 5", r["req_bytes"])
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoggingTransport_UnwrapReturnsBase(t *testing.T) {
	base := errorTransport{err: errors.New("sentinel")}
	rt := WrapTransport(base, WrapOpts{Caller: "x"})
	type unwrapper interface{ Unwrap() http.RoundTripper }
	u, ok := rt.(unwrapper)
	if !ok {
		t.Fatalf("WrapTransport result does not implement Unwrap")
	}
	got, ok := u.Unwrap().(errorTransport)
	if !ok {
		t.Fatalf("Unwrap: got %T, want errorTransport", u.Unwrap())
	}
	if got.err.Error() != "sentinel" {
		t.Errorf("Unwrap returned wrong base: %v", got)
	}
}

func TestReqBytes_FromCounterWhenNoContentLength(t *testing.T) {
	logger, buf := captureLogger(t)
	rt := WrapTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// Drain the body so the counting wrapper observes all bytes.
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{StatusCode: http.StatusOK, Proto: "HTTP/1.1", Body: io.NopCloser(strings.NewReader(""))}, nil
	}), WrapOpts{Logger: logger, Caller: "x"})

	req, _ := http.NewRequest(http.MethodPost, "http://x/", io.NopCloser(strings.NewReader("abcdefg")))
	req.ContentLength = -1 // simulate chunked / unknown
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	r := records(t, buf)[0]
	if r["req_bytes"].(float64) != 7 {
		t.Errorf("req_bytes: %v want 7", r["req_bytes"])
	}
}

func TestRoundTripper_AttemptComesFromContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	logger, buf := captureLogger(t)
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, WrapOpts{Logger: logger, Caller: "x"})}

	req, _ := http.NewRequestWithContext(WithAttempt(context.Background(), 4), http.MethodGet, srv.URL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	r := records(t, buf)[0]
	if r["attempt"].(float64) != 4 {
		t.Errorf("attempt: %v want 4", r["attempt"])
	}
}

func TestRoundTripper_AttemptDefaultsTo1_WhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	logger, buf := captureLogger(t)
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, WrapOpts{Logger: logger, Caller: "x"})}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil) // no WithAttempt
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	r := records(t, buf)[0]
	if r["attempt"].(float64) != 1 {
		t.Errorf("attempt: %v want 1", r["attempt"])
	}
}

func TestRedactURLQuery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no query", "https://api.example.com/v1/chat", "https://api.example.com/v1/chat"},
		{"non-sensitive query preserved", "https://api.example.com/v1/chat?model=gpt-4&temperature=0.7", "https://api.example.com/v1/chat?model=gpt-4&temperature=0.7"},
		{"gemini ?key=", "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent?key=AIzaSyABC123", "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent?key=%2A%2A%2A"},
		{"api_key", "https://provider.example/v1?api_key=secret123", "https://provider.example/v1?api_key=%2A%2A%2A"},
		{"api-key dashed", "https://provider.example/v1?api-key=secret123", "https://provider.example/v1?api-key=%2A%2A%2A"},
		{"case-insensitive Key", "https://provider.example/v1?Key=secret123", "https://provider.example/v1?Key=%2A%2A%2A"},
		{"linkkey not redacted (substring would be wrong)", "https://provider.example/v1?linkkey=public", "https://provider.example/v1?linkkey=public"},
		{"mixed sensitive + non-sensitive", "https://provider.example/v1?key=secret&model=x&token=t1", "https://provider.example/v1?key=%2A%2A%2A&model=x&token=%2A%2A%2A"},
		{"multiple values for same param", "https://provider.example/v1?key=a&key=b", "https://provider.example/v1?key=%2A%2A%2A&key=%2A%2A%2A"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := redactURLQuery(u); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRedactURLQuery_NilURL(t *testing.T) {
	if got := redactURLQuery(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// End-to-end: outbound log line must show *** for sensitive params.
// slog JSON handler does not double-escape — the URL string is
// emitted verbatim, so the URL-escaped form (%2A%2A%2A) appears in
// the JSON record.
func TestRoundTripper_RedactsSensitiveQueryInLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger, buf := captureLogger(t)
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, WrapOpts{Logger: logger, Caller: "x"})}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo?key=SECRET&model=gpt-4", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	r := records(t, buf)[0]
	urlStr := r["url"].(string)
	if strings.Contains(urlStr, "SECRET") {
		t.Errorf("URL must not contain raw SECRET: %s", urlStr)
	}
	if !strings.Contains(urlStr, "key=") {
		t.Errorf("URL must keep param name 'key': %s", urlStr)
	}
	if !strings.Contains(urlStr, "%2A%2A%2A") {
		t.Errorf("URL must contain %%2A%%2A%%2A (URL-escaped ***): %s", urlStr)
	}
	if !strings.Contains(urlStr, "model=gpt-4") {
		t.Errorf("URL must keep non-sensitive params: %s", urlStr)
	}
}
